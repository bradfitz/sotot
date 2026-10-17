// Package ingest accepts source connections: one tailcat listener per
// configured source, each serving the source WebSocket protocol for only
// that source.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
	"github.com/coder/websocket"
)

// Handler serves the source WebSocket protocol for one source. The
// source's identity comes from which listener the connection arrived on;
// nothing in the messages can select another source.
type Handler struct {
	Store    *state.Store
	SourceID string
	Logf     func(format string, args ...any) // nil means log.Printf

	// Liveness settings; zero means the defaults of 30s, 90s, and
	// protocol.SnapshotDeadline.
	PingInterval     time.Duration
	PongTimeout      time.Duration
	SnapshotDeadline time.Duration

	mu       sync.Mutex
	sessions map[*session]bool
	closed   bool
}

type session struct {
	cancel context.CancelCauseFunc
}

var (
	errListenerClosed = errors.New("listener closed")
	errNoSnapshot     = errors.New("no snapshot within deadline")
	errPongTimeout    = errors.New("no pong within timeout")
)

func (h *Handler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// CloseSessions closes all current sessions and rejects new ones, such as
// when the listener is removed.
func (h *Handler) CloseSessions() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.sessions {
		s.cancel(errListenerClosed)
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != protocol.Path {
		// Only ingestion is exposed here: never the dashboard or other
		// sources' data.
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	sess := &session{cancel: cancel}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "listener closed", http.StatusServiceUnavailable)
		return
	}
	if h.sessions == nil {
		h.sessions = map[*session]bool{}
	}
	h.sessions[sess] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.sessions, sess)
		h.mu.Unlock()
	}()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept wrote the HTTP error
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.MaxMessageBytes)

	gen, err := h.Store.StartSession(h.SourceID, func() { cancel(state.ErrSuperseded) })
	if err != nil {
		c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer h.Store.EndSession(h.SourceID, gen)
	h.logf("ingest: %s: session %d connected", h.SourceID, gen)

	// Ending the session for any reason (superseded, liveness, deadline,
	// listener removal, or a bad message) reports why to the source and
	// closes. Canceling a Read's context would close the connection
	// abruptly, so reads use an uncanceled context and this unblocks them.
	var finishOnce sync.Once
	finish := func(err error) {
		finishOnce.Do(func() {
			h.logf("ingest: %s: session %d ended: %v", h.SourceID, gen, err)
			status := websocket.StatusNormalClosure
			var perr *protocolError
			switch {
			case errors.As(err, &perr):
				writeError(c, perr.seq, perr.err.Error())
				status = websocket.StatusPolicyViolation
			case errors.Is(err, state.ErrSuperseded), errors.Is(err, errNoSnapshot), errors.Is(err, errPongTimeout):
				writeError(c, 0, err.Error())
				status = websocket.StatusPolicyViolation
			case errors.Is(err, errListenerClosed):
				status = websocket.StatusGoingAway
			}
			c.Close(status, truncate(errString(err), 120))
		})
	}
	stop := context.AfterFunc(ctx, func() { finish(context.Cause(ctx)) })
	err = h.serve(ctx, cancel, c, gen)
	if stop() {
		finish(err)
	} else {
		finish(context.Cause(ctx)) // waits for the AfterFunc's finish
	}
}

// protocolError is a rejected message, reported to the source before
// closing.
type protocolError struct {
	seq int64
	err error
}

func (e *protocolError) Error() string { return e.err.Error() }

// serve reads and applies messages until the connection fails or a
// message is rejected. cancel ends the session with a cause.
func (h *Handler) serve(ctx context.Context, cancel context.CancelCauseFunc, c *websocket.Conn, gen uint64) error {
	rctx := context.WithoutCancel(ctx)

	// Liveness: the source must answer pings, which assert that its
	// process and connection are alive. They don't renew data validity.
	go func() {
		t := time.NewTicker(cmpDur(h.PingInterval, 30*time.Second))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, cmpDur(h.PongTimeout, 90*time.Second))
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				if ctx.Err() == nil {
					cancel(errPongTimeout)
				}
				return
			}
			h.Store.Pong(h.SourceID, gen)
		}
	}()

	gotSnapshot := make(chan struct{})
	go func() {
		select {
		case <-gotSnapshot:
		case <-ctx.Done():
		case <-time.After(cmpDur(h.SnapshotDeadline, protocol.SnapshotDeadline)):
			cancel(errNoSnapshot)
		}
	}()

	var lastSeq int64
	for {
		typ, b, err := c.Read(rctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if st := websocket.CloseStatus(err); st != -1 {
				return fmt.Errorf("closed by source (%v)", st)
			}
			return err
		}
		if typ != websocket.MessageText {
			return &protocolError{lastSeq + 1, errors.New("binary messages aren't allowed")}
		}
		msg, err := protocol.ParseSourceMessage(b)
		if err != nil {
			return &protocolError{lastSeq + 1, err}
		}
		seq := messageSeq(msg)
		if seq != lastSeq+1 {
			return &protocolError{seq, fmt.Errorf("got seq %d; want %d", seq, lastSeq+1)}
		}
		if _, ok := msg.(*protocol.Snapshot); !ok && lastSeq == 0 {
			return &protocolError{seq, errors.New("first message must be a snapshot")}
		}
		ack, err := h.Store.Apply(h.SourceID, gen, msg)
		if errors.Is(err, state.ErrSuperseded) {
			return err
		}
		if err != nil {
			return &protocolError{seq, err}
		}
		if lastSeq == 0 {
			close(gotSnapshot)
		}
		lastSeq = seq
		if err := writeJSON(rctx, c, ack); err != nil {
			return err
		}
	}
}

func messageSeq(msg any) int64 {
	switch m := msg.(type) {
	case *protocol.Snapshot:
		return m.Seq
	case *protocol.Delta:
		return m.Seq
	case *protocol.Refresh:
		return m.Seq
	}
	return 0
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

// writeError sends a bounded error message, best effort, ahead of closing.
func writeError(c *websocket.Conn, seq int64, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	writeJSON(ctx, c, protocol.Error{Type: protocol.TypeError, Seq: seq, Message: truncate(msg, protocol.MaxErrorBytes)})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a UTF-8 boundary.
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func cmpDur(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
