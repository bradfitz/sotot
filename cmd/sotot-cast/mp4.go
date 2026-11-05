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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mp4Streamer encodes the hub's frames with ffmpeg as fragmented MP4 and
// serves it as never-ending HTTP responses.
//
// ffmpeg's output is an init segment (ftyp and moov boxes) followed by
// fragments (moof and mdat box pairs), each starting with a keyframe.
//
// Each cast LOAD gets its own session URL, which serves a consistent
// virtual file: the init segment, then fragments from the newest one at
// session creation onward. The receiver (Chrome) may close a download and
// later reopen it with "Range: bytes=N-" to resume, so sessions support
// ranges: a reopened request continues at exactly byte N, rather than
// jumping to the live edge, which the receiver rejects.
type mp4Streamer struct {
	mu        sync.Mutex
	init      []byte        // ftyp+moov; nil until ffmpeg writes it
	videoID   uint32        // track ID of the video track, from init
	timescale uint32        // of the video track
	frags     []*mp4Frag    // recent fragments, oldest first
	nextPos   int64         // stream byte offset of the next fragment
	changed   chan struct{} // closed and replaced when init or frags change
	sessions  map[string]*mp4Session
	nextSess  int

	debugDropped atomic.Bool // whether --debug-drop-after has been armed

	// start is when the first frame was fed to ffmpeg. Media time t in
	// the stream shows what was published at start+t.
	start time.Time
}

// mp4Frag is one fragment of the stream.
type mp4Frag struct {
	seq       int
	data      []byte
	pos       int64     // byte offset in the stream of fragments (excluding init)
	at        time.Time // when ffmpeg finished writing it
	mediaTime float64   // seconds; the video track's baseMediaDecodeTime
}

// mp4Session is the virtual file served to one cast LOAD.
type mp4Session struct {
	id    string
	start *mp4Frag // its first fragment, at byte offset len(init)
}

// maxFrags bounds the fragments kept for resuming sessions: three minutes.
// A variable for tests.
var maxFrags = 180

// startMP4 starts ffmpeg, feeding it frames from h until ctx is done.
func startMP4(ctx context.Context, h *hub, width, height, fps int) (*mp4Streamer, error) {
	args := encoderArgs(width, height, fps, fps) // one-second fragments
	args = append(args,
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"pipe:1",
	)
	cmd := exec.CommandContext(ctx, *flagFFmpeg, args...)
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
				s.videoID, s.timescale = videoTrack(box)
				s.notifyLocked()
			}
			s.mu.Unlock()
		case "mdat":
			data := append(pending, box...)
			pending = nil
			s.addFrag(data)
		default: // moof, and any styp/sidx/prft boxes preceding it
			pending = append(pending, box...)
		}
	}
}

// minFragBytes is the least data sent per (one-second) fragment. The
// receiver won't start or keep playing a stream that trickles in: an
// almost blank, static dashboard encodes to ~28 KiB/s and stalls in
// "buffering" forever, while ~85 KiB/s plays fine. So small fragments
// are padded with a "free" box, which players skip. A variable for
// tests.
var minFragBytes = 96 << 10

// padFrag returns frag followed by a "free" box making it at least
// minFragBytes long.
func padFrag(frag []byte) []byte {
	const hdr = 8
	n := minFragBytes - len(frag)
	if n <= 0 {
		return frag
	}
	n = max(n, hdr)
	box := make([]byte, n)
	binary.BigEndian.PutUint32(box, uint32(n))
	copy(box[4:], "free")
	return append(frag, box...)
}

// addFrag appends a fragment, dropping the oldest beyond maxFrags.
func (s *mp4Streamer) addFrag(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &mp4Frag{data: data, pos: s.nextPos, at: time.Now()}
	if n := len(s.frags); n > 0 {
		f.seq = s.frags[n-1].seq + 1
	}
	if s.timescale > 0 {
		f.mediaTime = float64(fragDecodeTime(data, s.videoID)) / float64(s.timescale)
	}
	if s.timescale > 0 { // a real encoder stream, not a test's
		f.data = padFrag(data)
	}
	s.nextPos += int64(len(f.data))
	s.frags = append(s.frags, f)
	if len(s.frags) > maxFrags {
		s.frags[0] = nil
		s.frags = s.frags[1:]
	}
	s.notifyLocked()
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
		ready, changed := s.init != nil && len(s.frags) > 0, s.changed
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

// newSession returns the URL path of a new session starting at the
// newest fragment. It must be called after waitReady.
func (s *mp4Streamer) newSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextSess++
	sess := &mp4Session{id: fmt.Sprint(s.nextSess), start: s.frags[len(s.frags)-1]}
	if s.sessions == nil {
		s.sessions = map[string]*mp4Session{}
	}
	s.sessions[sess.id] = sess
	log.Printf("mp4: session %s starts at fragment %d, media time %.1fs", sess.id, sess.start.seq, sess.start.mediaTime)
	return "/stream/" + sess.id + ".mp4"
}

// liveMediaTime returns the media time at the live edge: the end of the
// newest fragment, plus time since it arrived. It's measured from the
// stream itself rather than the wall clock, since a busy host can make
// the encoder fall behind real time.
func (s *mp4Streamer) liveMediaTime() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frags) == 0 {
		return 0
	}
	f := s.frags[len(s.frags)-1]
	const fragDur = 1.0 // seconds: one keyframe interval
	return f.mediaTime + fragDur + time.Since(f.at).Seconds()
}

// sessionStart returns the media time at which the session with the
// given URL starts.
func (s *mp4Streamer) sessionStart(url string) float64 {
	_, path, _ := strings.Cut(url, "/stream/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[strings.TrimSuffix(path, ".mp4")]; sess != nil {
		return sess.start.mediaTime
	}
	return 0
}

// fakeLength is the total length claimed for a session's endless file in
// range responses; HTTP has no way to say "unbounded".
const fakeLength = 1 << 40

func (s *mp4Streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/stream/"), ".mp4")
	s.mu.Lock()
	sess := s.sessions[id]
	init := s.init
	s.mu.Unlock()
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	rangeHdr := r.Header.Get("Range")
	off, ok := parseRangeStart(rangeHdr)
	if !ok {
		http.Error(w, "unsupported range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	log.Printf("mp4: session %s: %s by %s (Range %q)", id, r.Method, r.RemoteAddr, rangeHdr)
	defer log.Printf("mp4: session %s: response to %s ended", id, r.RemoteAddr)

	setStreamHeaders(w, "video/mp4")
	status := http.StatusOK
	if off > 0 {
		// A resumption. (A plain 200 for offset 0 matters: told the
		// endless file has a length, the receiver never starts playing.)
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, fakeLength-1, fakeLength))
	}

	// fragAt finds the fragment containing virtual-file offset off, or
	// reports that it's not yet written (wait) or no longer kept (gone).
	fragAt := func(off int64) (f *mp4Frag, inner int64, changed <-chan struct{}, gone bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		streamOff := off - int64(len(init)) + sess.start.pos
		// Only data before the oldest kept fragment is gone. (The
		// session's own first fragment going is fine: offsets are
		// absolute, so a client reading along needn't notice.)
		if len(s.frags) == 0 || streamOff < s.frags[0].pos {
			return nil, 0, nil, true
		}
		i := sort.Search(len(s.frags), func(i int) bool { return s.frags[i].pos+int64(len(s.frags[i].data)) > streamOff })
		if i == len(s.frags) {
			return nil, 0, s.changed, false
		}
		f = s.frags[i]
		return f, streamOff - f.pos, nil, false
	}

	if off >= int64(len(init)) {
		if _, _, _, gone := fragAt(off); gone {
			http.Error(w, "range no longer available", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	w.WriteHeader(status)
	if r.Method == "HEAD" {
		return
	}
	rc := http.NewResponseController(w)
	var drop <-chan time.Time
	if *flagDebugDrop > 0 && s.debugDropped.CompareAndSwap(false, true) {
		drop = time.After(*flagDebugDrop)
	}
	if off < int64(len(init)) {
		if _, err := w.Write(init[off:]); err != nil {
			return
		}
		off = int64(len(init))
	}
	for {
		f, inner, changed, gone := fragAt(off)
		switch {
		case gone:
			log.Printf("mp4: session %s: client %s fell more than %d fragments behind", id, r.RemoteAddr, maxFrags)
			return
		case f == nil:
			select {
			case <-changed:
			case <-r.Context().Done():
				return
			case <-drop:
				log.Printf("mp4: session %s: debug: dropping connection to %s at offset %d", id, r.RemoteAddr, off)
				panic(http.ErrAbortHandler) // like a network failure: no clean end
			}
			continue
		}
		if _, err := w.Write(f.data[inner:]); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		off += int64(len(f.data)) - inner
		if *flagVerbose {
			log.Printf("mp4: session %s: fragment %d (media %.1fs) sent to %s %v after encode", id, f.seq, f.mediaTime, r.RemoteAddr, time.Since(f.at).Round(time.Millisecond))
		}
	}
}

// parseRangeStart parses a "bytes=N-" Range header, the only form an
// endless stream can serve. An empty header means offset 0.
func parseRangeStart(h string) (int64, bool) {
	if h == "" {
		return 0, true
	}
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	start, end, ok := strings.Cut(spec, "-")
	if !ok || strings.Contains(spec, ",") {
		return 0, false
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	if end != "" {
		// A bounded range; serve from its start, which a media client
		// reading sequentially accepts.
		if _, err := strconv.ParseInt(end, 10, 64); err != nil {
			return 0, false
		}
	}
	return n, true
}

// children returns the child boxes of an MP4 container box's payload.
func children(payload []byte) map[string][][]byte {
	m := map[string][][]byte{}
	for len(payload) >= 8 {
		size := int(binary.BigEndian.Uint32(payload))
		hdr := 8
		if size == 1 && len(payload) >= 16 {
			size, hdr = int(binary.BigEndian.Uint64(payload[8:])), 16
		}
		if size < hdr || size > len(payload) {
			break
		}
		typ := string(payload[4:8])
		m[typ] = append(m[typ], payload[hdr:size])
		payload = payload[size:]
	}
	return m
}

func first(bs [][]byte) []byte {
	if len(bs) == 0 {
		return nil
	}
	return bs[0]
}

// videoTrack returns the track ID and media timescale of the video
// track described by a moov box.
func videoTrack(moov []byte) (id, timescale uint32) {
	for _, trak := range children(moov[8:])["trak"] {
		tk := children(trak)
		mdia := children(first(tk["mdia"]))
		hdlr := first(mdia["hdlr"])
		if len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
			continue
		}
		tkhd, mdhd := first(tk["tkhd"]), first(mdia["mdhd"])
		switch {
		case len(tkhd) >= 24 && tkhd[0] == 1:
			id = binary.BigEndian.Uint32(tkhd[20:])
		case len(tkhd) >= 16:
			id = binary.BigEndian.Uint32(tkhd[12:])
		}
		switch {
		case len(mdhd) >= 24 && mdhd[0] == 1:
			timescale = binary.BigEndian.Uint32(mdhd[20:])
		case len(mdhd) >= 16:
			timescale = binary.BigEndian.Uint32(mdhd[12:])
		}
		return id, timescale
	}
	return 0, 0
}

// fragDecodeTime returns the baseMediaDecodeTime of track id's fragment
// within a fragment's moof box.
func fragDecodeTime(frag []byte, id uint32) uint64 {
	moof := first(children(frag)["moof"])
	for _, traf := range children(moof)["traf"] {
		tr := children(traf)
		tfhd, tfdt := first(tr["tfhd"]), first(tr["tfdt"])
		if len(tfhd) < 8 || binary.BigEndian.Uint32(tfhd[4:]) != id {
			continue
		}
		switch {
		case len(tfdt) >= 12 && tfdt[0] == 1:
			return binary.BigEndian.Uint64(tfdt[4:])
		case len(tfdt) >= 8:
			return uint64(binary.BigEndian.Uint32(tfdt[4:]))
		}
	}
	return 0
}
