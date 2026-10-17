package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
	"github.com/coder/websocket"
)

func newTestServer(t *testing.T) (*state.Store, *httptest.Server) {
	t.Helper()
	st, err := state.New(state.Config{Sources: []state.SourceConfig{{ID: "chat", Name: "Chat <&>"}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ds := &Server{Store: st, Static: fstest.MapFS{"style.css": {Data: []byte("body{}")}}, Location: time.UTC, Logf: t.Logf}
	if err := ds.Init(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ds)
	t.Cleanup(ts.Close)
	return st, ts
}

func publish(t *testing.T, st *state.Store, things ...protocol.Thing) {
	t.Helper()
	gen, err := st.StartSession("chat", func() {})
	if err != nil {
		t.Fatal(err)
	}
	if things == nil {
		things = []protocol.Thing{}
	}
	if _, err := st.Apply("chat", gen, &protocol.Snapshot{Type: "snapshot", Seq: 1, ValidForSeconds: protocol.Seconds(time.Hour), Things: things}); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestIndexEscapesSourceText(t *testing.T) {
	st, ts := newTestServer(t)
	publish(t, st, protocol.Thing{ID: "x", Title: `<script>alert("hi")</script>`, URL: "https://example.com/?a=1&b=2"})
	page := get(t, ts.URL+"/")
	if strings.Contains(page, "<script>alert") {
		t.Fatal("thing title not escaped")
	}
	for _, want := range []string{"&lt;script&gt;alert", "Chat &lt;&amp;&gt;", `href="https://example.com/?a=1&amp;b=2"`, "Connected"} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The Wasm client isn't in this test's static files.
	if !strings.Contains(page, "go generate ./web") {
		t.Error("page doesn't explain that live updates are unavailable")
	}
}

func TestEmptyIsNotReassuringWhenStale(t *testing.T) {
	_, ts := newTestServer(t)
	page := get(t, ts.URL+"/")
	if strings.Contains(page, "Nothing needs attention") || !strings.Contains(page, "never reported") {
		t.Fatal("empty never-reported dashboard implies nothing needs attention")
	}
}

func TestJSONStream(t *testing.T) {
	st, ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)
	next := func() state.View {
		t.Helper()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var v state.View
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := next()
	if v.Sources[0].Status != state.Never {
		t.Fatalf("initial status %v", v.Sources[0].Status)
	}
	publish(t, st, protocol.Thing{ID: "a", Title: "A"})
	// Updates may coalesce; read until the change shows up.
	for v.Sources[0].Status != state.Connected || len(v.Sources[0].Things) != 1 {
		prev := v.Revision
		v = next()
		if v.Revision <= prev {
			t.Fatalf("revision went from %d to %d", prev, v.Revision)
		}
	}
}

func TestViewStreamRejectsCrossOrigin(t *testing.T) {
	_, ts := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("cross-origin WebSocket accepted")
	}
	if res != nil && res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %v; want 403", res.Status)
	}
}
