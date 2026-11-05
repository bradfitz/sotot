package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
	"github.com/bradfitz/sotot/source"
	"github.com/coder/websocket"
)

// testLogf returns a logf for t that drops messages once the test is
// over, since sessions can end (and log) after it completes, which would
// otherwise panic.
func testLogf(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() { // registered first, so runs last
		mu.Lock()
		defer mu.Unlock()
		done = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Logf(format, args...)
		}
	}
}

func newTestServer(t *testing.T, h *Handler) (*state.Store, *httptest.Server) {
	t.Helper()
	logf := testLogf(t)
	st, err := state.New(state.Config{Sources: []state.SourceConfig{{ID: "chat", Name: "Chat"}}, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.Store, h.SourceID, h.Logf = st, "chat", logf
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	t.Cleanup(h.CloseSessions)
	return st, ts
}

func dialer(ts *httptest.Server) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
	}
}

func chatView(st *state.Store) state.SourceView { return st.View().Sources[0] }

func ids(sv state.SourceView) string {
	var s []string
	for _, t := range sv.Things {
		s = append(s, t.ID)
	}
	return strings.Join(s, ",")
}

// waitFor polls cond until it's true or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientRun(t *testing.T) {
	st, ts := newTestServer(t, &Handler{})
	c := &source.Client{Dial: dialer(ts), Logf: testLogf(t)}
	c.SetValidFor(time.Hour)
	c.Set([]protocol.Thing{{ID: "alice", Title: "Reply to Alice"}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	waitFor(t, "snapshot", func() bool { sv := chatView(st); return sv.Status == state.Connected && ids(sv) == "alice" })

	c.Upsert(protocol.Thing{ID: "bob", Title: "Reply to Bob"})
	waitFor(t, "upsert", func() bool { return ids(chatView(st)) == "alice,bob" })
	c.Remove("alice")
	waitFor(t, "remove", func() bool { return ids(chatView(st)) == "bob" })
	if err := c.SetSummary("1 unread"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "summary", func() bool { return chatView(st).Summary == "1 unread" })

	cancel()
	<-done
	waitFor(t, "disconnect", func() bool { return chatView(st).Status == state.Fresh })
}

func TestPublishOnce(t *testing.T) {
	st, ts := newTestServer(t, &Handler{})
	c := &source.Client{Dial: dialer(ts), Logf: testLogf(t)}
	c.SetValidFor(30 * 24 * time.Hour)
	c.Set([]protocol.Thing{{ID: "example.net", Title: "Renew example.net", Kind: "renewal", Date: "2027-06-12", TimeZone: "America/Los_Angeles"}})
	if err := c.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The ack arrived, so the data is already applied.
	if got := ids(chatView(st)); got != "example.net" {
		t.Fatalf("things = %q", got)
	}
	waitFor(t, "disconnect", func() bool { return chatView(st).Status == state.Fresh })
}

// rawConn is a hand-driven source connection for testing misbehavior.
type rawConn struct {
	t *testing.T
	c *websocket.Conn
}

func dialRaw(t *testing.T, ts *httptest.Server) *rawConn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+protocol.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &rawConn{t, c}
}

func (r *rawConn) send(s string) {
	r.t.Helper()
	if err := r.c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		r.t.Fatal(err)
	}
}

// recv returns the next message's type and raw JSON, or "" and the close
// reason if the connection closed.
func (r *rawConn) recv() (typ string, raw string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, b, err := r.c.Read(ctx)
	if err != nil {
		return "", err.Error()
	}
	var env struct{ Type string }
	json.Unmarshal(b, &env)
	return env.Type, string(b)
}

func (r *rawConn) expect(wantType, wantSubstr string) {
	r.t.Helper()
	typ, raw := r.recv()
	if typ != wantType || !strings.Contains(raw, wantSubstr) {
		r.t.Fatalf("got %q %s; want %q containing %q", typ, raw, wantType, wantSubstr)
	}
}

func TestProtocolErrorsKeepState(t *testing.T) {
	st, ts := newTestServer(t, &Handler{})
	r := dialRaw(t, ts)
	r.send(`{"type":"snapshot","seq":1,"valid_for_seconds":3600,"things":[{"id":"a","title":"A"}]}`)
	r.expect("ack", `"seq":1`)
	r.send(`{"type":"delta","seq":3,"upsert":[{"id":"b","title":"B"}]}`) // skips seq 2
	r.expect("error", "want 2")
	r.expect("", "status = StatusPolicyViolation")
	if got := ids(chatView(st)); got != "a" {
		t.Fatalf("rejected delta changed things to %q", got)
	}

	r = dialRaw(t, ts)
	r.send(`{"type":"delta","seq":1,"upsert":[{"id":"b","title":"B"}]}`)
	r.expect("error", "first message must be a snapshot")

	r = dialRaw(t, ts)
	r.send(`{"type":"snapshot","seq":1,"valid_for_seconds":3600,"things":[{"id":"x"}]}`) // no title
	r.expect("error", "missing title")
	if got := ids(chatView(st)); got != "a" {
		t.Fatalf("invalid snapshot changed things to %q", got)
	}
}

func TestSnapshotDeadline(t *testing.T) {
	_, ts := newTestServer(t, &Handler{SnapshotDeadline: 50 * time.Millisecond})
	r := dialRaw(t, ts)
	r.expect("error", "no snapshot within deadline")
}

func TestPongTimeout(t *testing.T) {
	st, ts := newTestServer(t, &Handler{PingInterval: 20 * time.Millisecond, PongTimeout: 50 * time.Millisecond})
	r := dialRaw(t, ts)
	r.send(`{"type":"snapshot","seq":1,"valid_for_seconds":0,"things":[]}`)
	// Don't read, so pings go unanswered.
	waitFor(t, "connected", func() bool { return chatView(st).Status == state.Connected })
	waitFor(t, "liveness timeout", func() bool { return chatView(st).Status == state.Stale })
}

func TestTakeover(t *testing.T) {
	st, ts := newTestServer(t, &Handler{})
	old := dialRaw(t, ts)
	old.send(`{"type":"snapshot","seq":1,"valid_for_seconds":3600,"things":[{"id":"old","title":"Old"}]}`)
	old.expect("ack", "")

	nu := dialRaw(t, ts)
	waitFor(t, "connecting", func() bool { return chatView(st).Connecting })
	nu.send(`{"type":"snapshot","seq":1,"valid_for_seconds":3600,"things":[{"id":"new","title":"New"}]}`)
	nu.expect("ack", "")
	old.expect("error", "superseded")
	if sv := chatView(st); sv.Status != state.Connected || ids(sv) != "new" {
		t.Fatalf("after takeover: %+v", sv)
	}
}

func TestOnlyIngestPath(t *testing.T) {
	_, ts := newTestServer(t, &Handler{})
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("GET / = %v; want 404", res.Status)
	}
}

func TestConnListener(t *testing.T) {
	cl := newConnListener(1)
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hi") })}
	go hs.Serve(cl)

	// Simulate tailcat delivering a connection.
	client, server := net.Pipe()
	handled := make(chan struct{})
	go func() { cl.handle(server); close(handled) }()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }}}
	res, err := hc.Get("http://x/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "hi" {
		t.Fatalf("body %q", b)
	}

	// Closing everything unblocks the tailcat handler and Accept.
	hs.Close()
	cl.Close()
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked after close")
	}
	if _, err := cl.Accept(); err != errClosed {
		t.Fatalf("Accept after Close: %v", err)
	}
}

// TestClientNoticesVanishedDaemon checks that a source reconnects when
// the daemon disappears without its close arriving, as happens when a
// restarting daemon tears down its tailcat tunnels.
func TestClientNoticesVanishedDaemon(t *testing.T) {
	st, ts := newTestServer(t, &Handler{})
	var mu sync.Mutex
	var conns []net.Conn
	blackhole := false
	// A proxy that can silently stop forwarding, without closing.
	dial := func(ctx context.Context) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if blackhole {
			return nil, errors.New("unreachable")
		}
		client, proxy := net.Pipe()
		var d net.Dialer
		up, err := d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		conns = append(conns, up, proxy)
		go io.Copy(up, proxy)
		go io.Copy(proxy, up)
		return client, nil
	}
	c := &source.Client{Dial: dial, Logf: testLogf(t), PingInterval: 50 * time.Millisecond, PongTimeout: 100 * time.Millisecond}
	c.Set([]protocol.Thing{{ID: "a", Title: "A"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, "first session", func() bool { return chatView(st).Status == state.Connected })

	// The network path vanishes: nothing is closed, packets just stop.
	mu.Lock()
	blackhole = true
	for _, nc := range conns {
		if tc, ok := nc.(*net.TCPConn); ok {
			tc.CloseRead() // the daemon's side goes quiet
		}
	}
	h := conns
	conns = nil
	mu.Unlock()
	for _, nc := range h {
		if p, ok := nc.(interface{ SetDeadline(time.Time) error }); ok {
			p.SetDeadline(time.Now()) // stall the proxy's copies
		}
	}
	// The daemon notices on its side and marks the source disconnected...
	// (here, by the test server's own pings being stalled too)
	mu.Lock()
	blackhole = false
	mu.Unlock()
	// ...and the client must notice too, and reconnect.
	before := chatView(st).LastConnectedAt
	waitFor(t, "reconnect", func() bool {
		sv := chatView(st)
		return sv.LastConnectedAt != nil && before != nil && sv.LastConnectedAt.After(*before)
	})
}
