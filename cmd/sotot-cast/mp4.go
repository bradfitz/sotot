package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// mp4Streamer encodes the hub's frames with ffmpeg as fragmented MP4 and
// serves it as one never-ending HTTP response per client.
//
// ffmpeg's output is an init segment (ftyp and moov boxes) followed by
// fragments (moof and mdat box pairs), each starting with a keyframe. Each
// client gets the init segment and then fragments from the next one on.
type mp4Streamer struct {
	mu      sync.Mutex
	init    []byte        // ftyp+moov; nil until ffmpeg writes it
	frag    []byte        // newest fragment
	seq     int           // number of frag
	at      time.Time     // when frag arrived
	changed chan struct{} // closed and replaced when init or frag changes

	// start is when the first frame was fed to ffmpeg. Media time t in
	// the stream shows what was published at start+t.
	start time.Time
}

// startMP4 starts ffmpeg, feeding it frames from h until ctx is done.
func startMP4(ctx context.Context, h *hub, width, height, fps int) (*mp4Streamer, error) {
	args := encoderArgs(width, height, fps, fps) // one-second fragments
	args = append(args,
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"pipe:1",
	)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &mp4Streamer{changed: make(chan struct{}), start: time.Now()}
	go func() {
		err := s.readBoxes(bufio.NewReaderSize(stdout, 1<<20))
		werr := cmd.Wait()
		if ctx.Err() == nil {
			log.Fatalf("ffmpeg: %v (wait: %v)", err, werr)
		}
	}()
	go feedFrames(ctx, h, stdin, fps)
	return s, nil
}

// readBoxes reads ffmpeg's MP4 output and publishes init and fragments.
func (s *mp4Streamer) readBoxes(r io.Reader) error {
	var pending []byte // boxes of the fragment being assembled
	for {
		box, typ, err := readBox(r)
		if err != nil {
			return err
		}
		switch typ {
		case "ftyp", "moov":
			s.mu.Lock()
			s.init = append(s.init, box...)
			if typ == "moov" {
				s.notifyLocked()
			}
			s.mu.Unlock()
		case "mdat":
			frag := append(pending, box...)
			pending = nil
			s.mu.Lock()
			s.frag, s.at = frag, time.Now()
			s.seq++
			s.notifyLocked()
			s.mu.Unlock()
		default: // moof, and any styp/sidx/prft boxes preceding it
			pending = append(pending, box...)
		}
	}
}

func (s *mp4Streamer) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// readBox reads one complete top-level MP4 box.
func readBox(r io.Reader) (box []byte, typ string, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, "", err
	}
	size := uint64(binary.BigEndian.Uint32(hdr[:4]))
	typ = string(hdr[4:8])
	box = hdr[:]
	if size == 1 { // 64-bit size follows
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, "", err
		}
		size = binary.BigEndian.Uint64(ext[:])
		box = append(box, ext[:]...)
	}
	if size < uint64(len(box)) || size > 256<<20 {
		return nil, "", fmt.Errorf("bad MP4 box %q size %d", typ, size)
	}
	b := make([]byte, size)
	copy(b, box)
	if _, err := io.ReadFull(r, b[len(box):]); err != nil {
		return nil, "", err
	}
	return b, typ, nil
}

// waitReady waits until the init segment and a first fragment exist.
func (s *mp4Streamer) waitReady(ctx context.Context) error {
	for {
		s.mu.Lock()
		ready, changed := s.init != nil && s.seq > 0, s.changed
		s.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *mp4Streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("mp4: %s by %s (Range %q) started", r.Method, r.RemoteAddr, r.Header.Get("Range"))
	defer log.Printf("mp4: stream to %s ended", r.RemoteAddr)
	setStreamHeaders(w, "video/mp4")
	if r.Method == "HEAD" {
		return
	}
	rc := http.NewResponseController(w)
	if err := s.waitReady(r.Context()); err != nil {
		return
	}
	s.mu.Lock()
	init, seq := s.init, s.seq-1 // start with the newest fragment
	s.mu.Unlock()
	if _, err := w.Write(init); err != nil {
		return
	}
	for {
		s.mu.Lock()
		frag, cur, at, changed := s.frag, s.seq, s.at, s.changed
		s.mu.Unlock()
		if cur > seq {
			if cur > seq+1 {
				log.Printf("mp4: client %s fell behind; skipped %d fragments", r.RemoteAddr, cur-seq-1)
			}
			seq = cur
			if _, err := w.Write(frag); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
			if *flagVerbose {
				log.Printf("mp4: fragment %d (%d bytes) sent to %s %v after encode", cur, len(frag), r.RemoteAddr, time.Since(at).Round(time.Millisecond))
			}
			continue
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}
