package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/sotot/internal/cast"
)

// controller decides whether this display should be casting, per its
// switch on the sotot dashboard, and reports back what it's doing.
type controller struct {
	feed      *feed  // nil without --dashboard
	id        string // display ID; "" means always on, no reports
	statusURL string // dashboard endpoint for status reports
	changed   <-chan struct{}

	mu     sync.Mutex
	status string
}

func newController(fd *feed, id, dashboardURL string) *controller {
	c := &controller{feed: fd, id: id}
	if fd != nil {
		c.changed = fd.subscribe()
	}
	if fd != nil && id != "" {
		c.statusURL = strings.TrimSuffix(dashboardURL, "/") + "/api/v1/displays/" + url.PathEscape(id) + "/status"
	}
	return c
}

// wantOn reports whether the display should be casting now.
func (c *controller) wantOn() bool {
	if c.id == "" || c.feed == nil {
		return true
	}
	st := c.feed.state()
	if !st.HaveView {
		return false // don't know yet
	}
	for _, d := range st.View.Displays {
		if d.ID == c.id {
			return d.Enabled
		}
	}
	log.Printf("display %q isn't in sotot's config; staying off", c.id)
	return false
}

// waitChange waits until the dashboard state changes, d passes (if d >
// 0), or ctx is done.
func (c *controller) waitChange(ctx context.Context, d time.Duration) {
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
	case <-c.changed: // nil without a feed: waits for the timeout
	case <-timeout:
	}
}

// report tells the dashboard what the display is doing, if it changed.
func (c *controller) report(status string) {
	c.mu.Lock()
	same := c.status == status
	c.status = status
	c.mu.Unlock()
	if !same {
		log.Printf("display: %s", status)
		go c.send(status)
	}
}

// heartbeat resends the status periodically, so the dashboard can tell
// a live report from a stale one.
func (c *controller) heartbeat(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		status := c.status
		c.mu.Unlock()
		if status != "" {
			c.send(status)
		}
	}
}

func (c *controller) send(status string) {
	if c.statusURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"status": status})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", c.statusURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("display: reporting status: %v", err)
		return
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		log.Printf("display: reporting status: %v", res.Status)
	}
}

// tvInUse returns the name of the app the TV is busy with, or "" if it's
// free: showing its idle screen, nothing, or a leftover session of ours.
func tvInUse(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, err := cast.Dial(ctx, addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	apps, err := c.Apps(ctx)
	if err != nil {
		return "", err
	}
	return busyApp(apps), nil
}

// busyApp returns the name of an app among apps that means someone is
// using the TV, or "" if none does.
func busyApp(apps []cast.App) string {
	for _, a := range apps {
		if a.IsIdleScreen {
			continue
		}
		// Our own stream from before a restart. (The Default Media
		// Receiver can also be someone else's photo or video, which
		// wouldn't be titled "sotot".)
		if a.AppID == cast.DefaultMediaReceiver && strings.Contains(a.StatusText, "sotot") {
			continue
		}
		if a.DisplayName != "" {
			return a.DisplayName
		}
		return "another app"
	}
	return ""
}
