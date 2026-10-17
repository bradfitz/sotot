// Command sotot-cast shows the sotot dashboard on a Chromecast.
//
// This initial version renders a test pattern and casts it to the
// Chromecast's Default Media Receiver in one of several ways, to compare
// text crispness, update latency, and flicker:
//
//   - png: a new still image LOAD per frame (flashes between frames)
//   - gif: one never-ending animated GIF, extended as frames change
//   - multipart: one never-ending multipart/x-mixed-replace PNG stream
//   - hls: H.264 live video encoded by ffmpeg, as HLS
//   - mp4: H.264 live video encoded by ffmpeg, as one endless fragmented MP4
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bradfitz/sotot/internal/cast"
)

var (
	flagTV       = flag.String("tv", "", "friendly name of the Cast device, or its host:port")
	flagList     = flag.Bool("list", false, "list Cast devices on the LAN and exit")
	flagMode     = flag.String("mode", "png", "how to cast: png, gif, multipart, hls, mp4, or url")
	flagInterval = flag.Duration("interval", 0, "how often to render a new frame (default 3s for png, else 1s)")
	flagSize     = flag.String("size", "1920x1080", "frame size in pixels")
	flagPort     = flag.Int("http-port", 0, "TCP port for serving frames to the device; 0 picks one")
	flagFPS      = flag.Int("fps", 5, "hls mode: video frame rate")
	flagSegSecs  = flag.Int("segment-secs", 2, "hls mode: segment duration in seconds")
	flagCRF      = flag.String("crf", "16", "hls mode: x264 constant rate factor; lower is higher quality")
	flagVerbose  = flag.Bool("v", false, "verbose logging")

	flagMaxLag      = flag.Float64("max-lag", 4, "mp4 mode: speed up playback when it's more than this many seconds behind live")
	flagTargetLag   = flag.Float64("target-lag", 3, "mp4 mode: return to normal speed when playback is this close to live")
	flagCatchUpRate = flag.Float64("catch-up-rate", 1.25, "mp4 mode: playback rate while catching up")

	flagURL         = flag.String("url", "", "url mode: media URL to cast")
	flagContentType = flag.String("content-type", "video/mp4", "url mode: media content type")
	flagStreamType  = flag.String("stream-type", "BUFFERED", "url mode: NONE, BUFFERED, or LIVE")
)

func main() {
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *flagList {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		devs, err := cast.Discover(ctx)
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range devs {
			fmt.Printf("%-20q %-20s %s\n", d.Name, d.Model, d.Addr)
		}
		return
	}
	if *flagTV == "" {
		log.Fatal("--tv is required (use --list to see devices)")
	}
	switch *flagMode {
	case "png", "gif", "multipart", "hls", "mp4":
	case "url":
		if *flagURL == "" {
			log.Fatal("--mode=url requires --url")
		}
	default:
		log.Fatalf("unknown --mode %q", *flagMode)
	}
	interval := *flagInterval
	if interval == 0 {
		interval = time.Second
		if *flagMode == "png" {
			interval = 3 * time.Second
		}
	}
	w, h, err := parseSize(*flagSize)
	if err != nil {
		log.Fatalf("--size: %v", err)
	}

	addr := *flagTV
	if _, _, err := net.SplitHostPort(addr); err != nil {
		d, err := cast.Find(ctx, *flagTV, 3*time.Second)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("found %q (%s) at %s", d.Name, d.Model, d.Addr)
		addr = d.Addr.String()
	}

	localIP, err := cast.LocalAddrFor(addr)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(localIP.String(), strconv.Itoa(*flagPort)))
	if err != nil {
		log.Fatal(err)
	}
	baseURL := "http://" + ln.Addr().String()

	hb := newHub()
	r := newRenderer(w, h, *flagMode)
	hb.publish(r.render(time.Now()))
	go func() {
		// Render on interval boundaries so a seconds clock is never stale.
		for {
			now := time.Now()
			t := time.NewTimer(now.Truncate(interval).Add(interval).Sub(now))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case now := <-t.C:
				hb.publish(r.render(now))
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/frame/", hb.serveFrame)
	mux.HandleFunc("/stream.gif", hb.serveGIF)
	mux.HandleFunc("/stream.mpng", hb.serveMultipart)
	var media cast.Media
	var liveStart time.Time // for mp4, when media time 0 was published
	switch *flagMode {
	case "gif":
		media = cast.Media{URL: baseURL + "/stream.gif", ContentType: "image/gif"}
	case "multipart":
		media = cast.Media{URL: baseURL + "/stream.mpng", ContentType: "image/png"}
	case "hls":
		enc, err := startHLS(ctx, hb, w, h, *flagFPS)
		if err != nil {
			log.Fatalf("starting ffmpeg: %v", err)
		}
		mux.Handle("/hls/", enc)
		log.Printf("waiting for ffmpeg to produce segments")
		if err := enc.waitReady(ctx, 2); err != nil {
			log.Fatal(err)
		}
		media = cast.Media{
			URL:                   baseURL + "/hls/master.m3u8",
			ContentType:           "application/x-mpegurl",
			StreamType:            "LIVE",
			HLSSegmentFormat:      "ts",
			HLSVideoSegmentFormat: "mpeg2_ts",
		}
	case "mp4":
		s, err := startMP4(ctx, hb, w, h, *flagFPS)
		if err != nil {
			log.Fatalf("starting ffmpeg: %v", err)
		}
		mux.Handle("/stream.mp4", s)
		log.Printf("waiting for ffmpeg to produce video")
		start := time.Now()
		if err := s.waitReady(ctx); err != nil {
			log.Fatal(err)
		}
		log.Printf("first fragment after %v", time.Since(start).Round(time.Millisecond))
		media = cast.Media{URL: baseURL + "/stream.mp4", ContentType: "video/mp4", StreamType: "LIVE"}
		liveStart = s.start
	case "url":
		media = cast.Media{URL: *flagURL, ContentType: *flagContentType, StreamType: *flagStreamType}
	}
	media.Title = "sotot"
	go http.Serve(ln, mux)
	log.Printf("serving at %s", baseURL)

	for ctx.Err() == nil {
		var err error
		if *flagMode == "png" {
			err = runStills(ctx, addr, baseURL, hb)
		} else {
			err = runStream(ctx, addr, media, liveStart)
		}
		if ctx.Err() != nil {
			break
		}
		if err == cast.ErrSessionEnded {
			log.Fatalf("receiver session ended (another app took over the TV?); exiting")
		}
		log.Printf("cast: %v; reconnecting in 5s", err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// connect dials the device and launches the media receiver. The returned
// cleanup func stops the receiver if ctx is done, so the TV returns to its
// idle screen on exit, and closes the connection.
func connect(ctx context.Context, addr string) (*cast.Conn, *cast.Session, func(), error) {
	c, err := cast.Dial(ctx, addr)
	if err != nil {
		return nil, nil, nil, err
	}
	c.Logf = log.Printf
	c.LogRaw = *flagVerbose
	sess, err := c.Launch(ctx, cast.DefaultMediaReceiver)
	if err != nil {
		c.Close()
		return nil, nil, nil, err
	}
	log.Printf("launched receiver session %s", sess.SessionID)
	cleanup := func() {
		defer c.Close()
		if ctx.Err() == nil {
			return
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Stop(stopCtx, sess); err != nil {
			log.Printf("stopping receiver: %v", err)
		} else {
			log.Printf("stopped receiver")
		}
	}
	return c, sess, cleanup, nil
}

// runStills LOADs each newly published frame as its own image.
func runStills(ctx context.Context, addr, baseURL string, hb *hub) error {
	c, sess, cleanup, err := connect(ctx, addr)
	if err != nil {
		return err
	}
	defer cleanup()
	for {
		f, changed := hb.latest()
		url := fmt.Sprintf("%s/frame/%d.png", baseURL, f.seq)
		start := time.Now()
		if _, err := c.Load(ctx, sess, cast.Media{URL: url, ContentType: "image/png", Title: "sotot"}); err != nil {
			return cmp.Or(ctx.Err(), err)
		}
		log.Printf("frame %d: LOAD acknowledged in %v", f.seq, time.Since(start).Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			return c.Err()
		case <-sess.Ended():
			return cast.ErrSessionEnded
		case <-changed:
		}
	}
}

// runStream LOADs a single never-ending stream and waits.
//
// If liveStart is non-zero, it's the wall time corresponding to media time
// zero, and runStream keeps playback near the live edge. The receiver
// buffers several seconds before it starts playing and would otherwise
// stay that far behind forever. Seeking doesn't work in an endless stream,
// so it catches up by briefly playing faster than real time.
func runStream(ctx context.Context, addr string, m cast.Media, liveStart time.Time) error {
	c, sess, cleanup, err := connect(ctx, addr)
	if err != nil {
		return err
	}
	defer cleanup()
	statusc := make(chan cast.MediaStatus, 16)
	c.OnMediaStatus = func(st cast.MediaStatus) {
		select {
		case statusc <- st:
		default:
		}
	}
	start := time.Now()
	mediaID, err := c.Load(ctx, sess, m)
	if err != nil {
		return cmp.Or(ctx.Err(), err)
	}
	log.Printf("LOAD %s acknowledged in %v", m.URL, time.Since(start).Round(time.Millisecond))

	// The receiver sends status updates on its own while playing, about
	// every second; polling is only a fallback. Frequent polling may make
	// the receiver flash its playback bar.
	poll := time.NewTicker(30 * time.Second)
	defer poll.Stop()
	var lastLog time.Time
	wantRate := 1.0
	setRate := func(st cast.MediaStatus, rate float64, why string) {
		if rate == wantRate {
			return
		}
		wantRate = rate
		log.Printf("%s; playback rate %v", why, rate)
		go func() {
			if err := c.SetPlaybackRate(ctx, sess, st.MediaSessionID, rate); err != nil {
				log.Printf("set playback rate: %v", err)
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			return c.Err()
		case <-sess.Ended():
			return cast.ErrSessionEnded
		case <-poll.C:
			go c.RequestMediaStatus(ctx, sess)
		case st := <-statusc:
			if st.MediaSessionID != mediaID {
				continue // about earlier media, such as what our LOAD replaced
			}
			if st.PlayerState == "IDLE" && st.IdleReason != "" {
				// The stream should never end; reload it. (IDLE without
				// a reason is normal right after LOAD, before buffering.)
				return fmt.Errorf("receiver player went idle (%s)", st.IdleReason)
			}
			if liveStart.IsZero() {
				continue
			}
			if st.PlayerState == "BUFFERING" {
				setRate(st, 1, "receiver is buffering")
				continue
			}
			if st.PlayerState != "PLAYING" {
				continue
			}
			lag := time.Since(liveStart).Seconds() - st.CurrentTime
			if time.Since(lastLog) > 10*time.Second {
				log.Printf("playback is %.1fs behind live (t=%.1fs, rate %v)", lag, st.CurrentTime, st.PlaybackRate)
				lastLog = time.Now()
			}
			switch {
			case lag > *flagMaxLag:
				setRate(st, *flagCatchUpRate, fmt.Sprintf("playback is %.1fs behind live", lag))
			case lag < *flagTargetLag:
				setRate(st, 1, fmt.Sprintf("playback is %.1fs behind live", lag))
			}
		}
	}
}

func parseSize(s string) (w, h int, err error) {
	ws, hs, ok := strings.Cut(s, "x")
	if !ok {
		return 0, 0, fmt.Errorf("want WxH, got %q", s)
	}
	if w, err = strconv.Atoi(ws); err != nil {
		return 0, 0, err
	}
	if h, err = strconv.Atoi(hs); err != nil {
		return 0, 0, err
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("invalid size %q", s)
	}
	return w, h, nil
}
