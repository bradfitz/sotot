package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/coder/websocket"
)

// feed follows a sotot dashboard's JSON stream.
type feed struct {
	url string // ws:// or wss:// URL of /api/v1/stream

	mu   sync.Mutex
	st   feedState
	subs []chan struct{} // each capacity 1; notified on changes
}

// feedState is what the feed knows about the dashboard.
type feedState struct {
	View      state.View
	HaveView  bool      // whether View has ever been received
	Connected bool      // whether the stream is currently live
	Since     time.Time // when Connected last changed
	Err       error     // why the stream is down, if it is
}

func newFeed(dashboardURL string) (*feed, error) {
	u, err := url.Parse(dashboardURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("dashboard URL %q: want http or https", dashboardURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/stream"
	return &feed{
		url: u.String(),
		st:  feedState{Since: time.Now()},
	}, nil
}

// subscribe returns a channel that receives a value after the feed's
// state changes. Notifications coalesce.
func (f *feed) subscribe() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := make(chan struct{}, 1)
	f.subs = append(f.subs, c)
	return c
}

func (f *feed) state() feedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *feed) update(fn func(*feedState)) {
	f.mu.Lock()
	was := f.st.Connected
	fn(&f.st)
	if f.st.Connected != was {
		f.st.Since = time.Now()
	}
	subs := f.subs
	f.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// run follows the stream until ctx is done, reconnecting as needed.
func (f *feed) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := f.once(ctx, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		log.Printf("dashboard: %v; reconnecting in %v", err, backoff)
		f.update(func(s *feedState) { s.Connected, s.Err = false, err })
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// silenceLimit is how long without a message (sotot sends at least every
// 30s) before the stream is presumed dead.
const silenceLimit = 75 * time.Second

func (f *feed) once(ctx context.Context, connected func()) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dctx, f.url, nil)
	cancel()
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(64 << 20)
	for {
		rctx, cancel := context.WithTimeout(ctx, silenceLimit)
		_, b, err := c.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		var m struct {
			Type string `json:"type"`
			state.View
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("bad message: %w", err)
		}
		if m.Type != "view" {
			continue
		}
		f.update(func(s *feedState) {
			if !s.Connected {
				log.Printf("dashboard: connected to %s", f.url)
				connected()
			}
			s.View, s.HaveView, s.Connected, s.Err = m.View, true, true, nil
		})
	}
}
