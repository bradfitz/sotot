package main

import (
	"bytes"
	"context"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestGIFStream checks that the never-ending GIF stream is a valid GIF
// whose partial frames composite to the published images.
func TestGIFStream(t *testing.T) {
	hb := newHub()
	r := newRenderer(1280, 720, "test")
	base := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	hb.publish(r.render(base))

	ts := httptest.NewServer(http.HandlerFunc(hb.serveGIF))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get("Content-Type"); got != "image/gif" {
		t.Errorf("Content-Type = %q", got)
	}

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		io.Copy(&buf, res.Body)
		close(done)
	}()
	const frames = 3
	last := r.render(base)
	for i := 1; i < frames; i++ {
		time.Sleep(100 * time.Millisecond)
		last = r.render(base.Add(time.Duration(i) * time.Second))
		hb.publish(last)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	buf.WriteByte(0x3b) // trailer, so the image decoder accepts it
	g, err := gif.DecodeAll(&buf)
	if err != nil {
		t.Fatalf("decoding stream: %v", err)
	}
	if len(g.Image) != frames {
		t.Fatalf("got %d frames; want %d", len(g.Image), frames)
	}
	if !g.Image[1].Bounds().In(g.Image[0].Bounds()) || g.Image[1].Bounds() == g.Image[0].Bounds() {
		t.Errorf("frame 2 bounds %v; want a sub-rectangle of %v", g.Image[1].Bounds(), g.Image[0].Bounds())
	}

	// Composite all frames and compare with the last published image,
	// quantized the same way.
	canvas := g.Image[0]
	for _, fr := range g.Image[1:] {
		b := fr.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			copy(canvas.Pix[canvas.PixOffset(b.Min.X, y):canvas.PixOffset(b.Max.X, y)],
				fr.Pix[fr.PixOffset(b.Min.X, y):fr.PixOffset(b.Max.X, y)])
		}
	}
	var q gifQuantizer
	want := q.quantize(last)
	if !bytes.Equal(canvas.Pix, want.Pix) {
		t.Errorf("composited frames don't match the last published image")
	}
}

func TestHLS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	hb := newHub()
	r := newRenderer(640, 360, "test")
	hb.publish(r.render(time.Now()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	enc, err := startHLS(ctx, hb, 640, 360, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.waitReady(ctx, 2); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	enc.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/live.m3u8", nil))
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("seg")) {
		t.Fatalf("playlist: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMP4Stream(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	hb := newHub()
	r := newRenderer(640, 360, "test")
	hb.publish(r.render(time.Now()))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := startMP4(ctx, hb, 640, 360, 5)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("first fragment after %v", time.Since(start))

	ts := httptest.NewServer(s)
	defer ts.Close()
	reqCtx, reqCancel := context.WithTimeout(ctx, 3500*time.Millisecond)
	defer reqCancel()
	req, _ := http.NewRequestWithContext(reqCtx, "GET", ts.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body) // until reqCtx expires

	f := t.TempDir() + "/stream.mp4"
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames",
		"-show_entries", "stream=codec_type,nb_read_frames", "-of", "csv=p=0", f).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	t.Logf("%d bytes; ffprobe streams:\n%s", len(body), out)
	if !bytes.Contains(out, []byte("video")) || !bytes.Contains(out, []byte("audio")) {
		t.Errorf("want video and audio streams")
	}
}
