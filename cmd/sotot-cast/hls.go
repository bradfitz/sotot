package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hlsEncoder pipes the hub's latest frame to ffmpeg at a constant frame
// rate and serves the resulting live HLS stream.
type hlsEncoder struct {
	dir string
}

// startHLS starts ffmpeg, feeding it frames from h until ctx is done.
func startHLS(ctx context.Context, h *hub, width, height, fps int) (*hlsEncoder, error) {
	dir, err := os.MkdirTemp("", "sotot-cast-hls-")
	if err != nil {
		return nil, err
	}
	args := encoderArgs(width, height, fps, fps**flagSegSecs) // one keyframe per segment
	args = append(args,
		"-f", "hls", "-hls_time", strconv.Itoa(*flagSegSecs), "-hls_list_size", "10",
		"-hls_flags", "delete_segments+independent_segments",
		"-hls_segment_filename", filepath.Join(dir, "seg%06d.ts"),
		// A master playlist declares the CODECS, so the receiver needn't
		// guess what the segments contain.
		"-master_pl_name", "master.m3u8",
		filepath.Join(dir, "live.m3u8"),
	)
	cmd := exec.CommandContext(ctx, *flagFFmpeg, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		defer os.RemoveAll(dir)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			log.Fatalf("ffmpeg: %v", err)
		}
	}()
	go feedFrames(ctx, h, stdin, fps)
	return &hlsEncoder{dir: dir}, nil
}

// waitReady waits until the playlist lists at least n segments, so the
// receiver doesn't start on an empty playlist.
func (e *hlsEncoder) waitReady(ctx context.Context, n int) error {
	for {
		b, _ := os.ReadFile(filepath.Join(e.dir, "live.m3u8"))
		if strings.Count(string(b), "#EXTINF") >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ServeHTTP serves the playlist and segments under /hls/.
func (e *hlsEncoder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/hls/")
	if name == "" || strings.ContainsAny(name, `/\`) {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if *flagVerbose {
		log.Printf("hls: %s %s by %s", r.Method, name, r.RemoteAddr)
	}
	http.ServeFile(w, r, filepath.Join(e.dir, name))
}
