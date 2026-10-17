package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"
)

// encoderArgs returns ffmpeg arguments that read raw RGBA frames from
// stdin and encode them as H.264 with a keyframe every gop frames, plus a
// silent AAC track: Cast receivers can sit buffering forever on video-only
// streams, waiting for audio. The caller appends output arguments.
func encoderArgs(width, height, fps, gop int) []string {
	return []string{
		"-hide_banner", "-loglevel", "warning",
		"-f", "rawvideo", "-pix_fmt", "rgba",
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-framerate", strconv.Itoa(fps),
		"-i", "-",
		"-re", "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-map", "0:v", "-map", "1:a",
		"-c:a", "aac", "-b:a", "32k",
		"-shortest", // exit when the video pipe closes, despite endless audio
		// -shortest otherwise buffers 10s of output, delaying everything.
		"-shortest_buf_duration", "0.1",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage,zerolatency",
		"-profile:v", "high", "-level", "4.1", "-pix_fmt", "yuv420p",
		"-crf", *flagCRF,
		"-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop), "-sc_threshold", "0",
	}
}

// feedFrames writes the hub's latest frame to w at a constant frame rate,
// repeating frames that haven't changed, until ctx is done or a write
// fails. It closes w on return.
func feedFrames(ctx context.Context, h *hub, w io.WriteCloser, fps int) {
	defer w.Close()
	t := time.NewTicker(time.Second / time.Duration(fps))
	defer t.Stop()
	for {
		if f, _ := h.latest(); f != nil {
			if _, err := w.Write(f.img.Pix); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
