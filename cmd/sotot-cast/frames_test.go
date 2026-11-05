package main

import (
	"bytes"
	"context"
	"fmt"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/sotot/internal/cast"
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

func startTestMP4(t *testing.T) (*mp4Streamer, *httptest.Server) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	hb := newHub()
	r := newRenderer(640, 360, "test")
	hb.publish(r.render(time.Now()))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := startMP4(ctx, hb, 640, 360, 5)
	if err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	start := time.Now()
	if err := s.waitReady(wctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("first fragment after %v", time.Since(start))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

// fetch reads url (with an optional Range header) for d, or n bytes if n > 0.
func fetch(t *testing.T, url, rangeHdr string, d time.Duration, n int64) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body io.Reader = res.Body
	if n > 0 {
		body = io.LimitReader(res.Body, n)
	}
	b, _ := io.ReadAll(body) // until the deadline or n bytes
	res.Body.Close()
	return res, b
}

func probe(t *testing.T, b []byte) string {
	t.Helper()
	f := t.TempDir() + "/stream.mp4"
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-count_frames",
		"-show_entries", "stream=codec_type,nb_read_frames", "-of", "csv=p=0", f).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	return string(out)
}

func TestMP4Stream(t *testing.T) {
	s, ts := startTestMP4(t)
	_, body := fetch(t, ts.URL+s.newSession(), "", 3500*time.Millisecond, 0)
	out := probe(t, body)
	t.Logf("%d bytes; ffprobe streams:\n%s", len(body), out)
	if !strings.Contains(out, "video") || !strings.Contains(out, "audio") {
		t.Errorf("want video and audio streams")
	}

	// Fragments carry their media times, one second apart.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timescale == 0 || len(s.frags) < 4 {
		t.Fatalf("timescale %d, %d fragments", s.timescale, len(s.frags))
	}
	// (The first is a little longer, aligning video with audio priming.)
	for i := 2; i < len(s.frags); i++ {
		if d := s.frags[i].mediaTime - s.frags[i-1].mediaTime; d < 0.99 || d > 1.01 {
			t.Fatalf("fragment %d media time %.3f follows %.3f", i, s.frags[i].mediaTime, s.frags[i-1].mediaTime)
		}
	}
}

// TestMP4Resume checks that a session reopened with a Range request
// continues exactly where the first response stopped, as the Chromecast
// does when it closes and reopens a download.
func TestMP4Resume(t *testing.T) {
	s, ts := startTestMP4(t)
	url := ts.URL + s.newSession()
	res, part1 := fetch(t, url, "bytes=0-", 10*time.Second, 50_000)
	// From the start, a plain 200: given a length for the endless file,
	// the Chromecast never starts playing.
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Range") != "" {
		t.Fatalf("status %v, Content-Range %q", res.Status, res.Header.Get("Content-Range"))
	}
	if len(part1) != 50_000 {
		t.Fatalf("got %d bytes", len(part1))
	}
	time.Sleep(2 * time.Second) // the live edge moves on meanwhile
	res, part2 := fetch(t, url, fmt.Sprintf("bytes=%d-", len(part1)), 3*time.Second, 0)
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("resume status %v", res.Status)
	}
	all := append(part1, part2...)
	out := probe(t, all) // fails on a discontinuity
	t.Logf("%d+%d bytes; ffprobe streams:\n%s", len(part1), len(part2), out)

	// A fresh request from the start serves the same bytes.
	_, again := fetch(t, url, "", 10*time.Second, int64(len(all)))
	if !bytes.Equal(again, all[:len(again)]) {
		t.Fatal("re-reading the session from 0 gave different bytes")
	}

	if res, _ := fetch(t, ts.URL+"/stream/nope.mp4", "", time.Second, 0); res.StatusCode != 404 {
		t.Errorf("unknown session: %v", res.Status)
	}
}

// TestMP4SessionOutlivesBuffer checks that a client reading along keeps
// getting data after its session's first fragments age out of the
// resume buffer. (Sessions used to end when that happened, every three
// minutes.)
func TestMP4SessionOutlivesBuffer(t *testing.T) {
	defer func(n int) { maxFrags = n }(maxFrags)
	maxFrags = 3
	s := &mp4Streamer{changed: make(chan struct{}), init: []byte("INIT")}
	frag := func(i int) []byte { return []byte(fmt.Sprintf("[frag%02d]", i)) }
	s.addFrag(frag(0))
	ts := httptest.NewServer(s)
	defer ts.Close()
	url := ts.URL + s.newSession()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	want := "INIT" + string(frag(0))
	got := make([]byte, 0, 256)
	read := func(n int) {
		t.Helper()
		buf := make([]byte, n)
		if _, err := io.ReadFull(res.Body, buf); err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		got = append(got, buf...)
	}
	read(len(want))
	for i := 1; i <= 10; i++ { // far past maxFrags
		s.addFrag(frag(i))
		want += string(frag(i))
		read(len(frag(i)))
	}
	if string(got) != want {
		t.Fatalf("got %q; want %q", got, want)
	}

	// Resuming at an offset still in the buffer works; before it, it's gone.
	resume := func(off int) int {
		r, _ := fetch(t, url, fmt.Sprintf("bytes=%d-", off), 300*time.Millisecond, 0)
		return r.StatusCode
	}
	if c := resume(len(want) - len(frag(10))); c != http.StatusPartialContent {
		t.Errorf("resume at last fragment: %v", c)
	}
	if c := resume(len("INIT") + 1); c != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("resume in an evicted fragment: %v", c)
	}
}

func TestPadFrag(t *testing.T) {
	defer func(n int) { minFragBytes = n }(minFragBytes)
	minFragBytes = 100
	f := padFrag([]byte("0123456789"))
	if len(f) != 100 || string(f[14:18]) != "free" {
		t.Fatalf("len %d, box type %q", len(f), f[14:18])
	}
	boxes := children(f[10:])
	if len(boxes["free"]) != 1 {
		t.Fatalf("free box not parseable: %v", boxes)
	}
	big := make([]byte, 200)
	if got := padFrag(big); len(got) != 200 {
		t.Fatalf("large fragment padded to %d", len(got))
	}
}

func TestBusyApp(t *testing.T) {
	for _, tt := range []struct {
		apps []cast.App
		want string
	}{
		{nil, ""},
		{[]cast.App{{AppID: "E8C28D3C", DisplayName: "Backdrop", IsIdleScreen: true}}, ""},
		{[]cast.App{{AppID: "CA5E8412", DisplayName: "Netflix"}}, "Netflix"},
		{[]cast.App{{AppID: cast.DefaultMediaReceiver, DisplayName: "Default Media Receiver", StatusText: "Now Casting: sotot"}}, ""},
		{[]cast.App{{AppID: cast.DefaultMediaReceiver, DisplayName: "Default Media Receiver", StatusText: "Now Casting: vacation.mp4"}}, "Default Media Receiver"},
		{[]cast.App{{AppID: "X"}}, "another app"},
	} {
		if got := busyApp(tt.apps); got != tt.want {
			t.Errorf("busyApp(%+v) = %q; want %q", tt.apps, got, tt.want)
		}
	}
}
