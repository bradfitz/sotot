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
	st, err := state.New(state.Config{
		Sources:  []state.SourceConfig{{ID: "chat", Name: "Chat <&>"}},
		Displays: []state.DisplayConfig{{ID: "crow", Name: "Office TV"}},
		Logf:     t.Logf,
	})
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

func post(t *testing.T, u, contentType, body string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestDisplaySwitch(t *testing.T) {
	st, ts := newTestServer(t)
	form := "application/x-www-form-urlencoded"
	if !strings.Contains(get(t, ts.URL+"/"), "Turn off") {
		t.Fatal("page lacks the display's Turn off button")
	}
	// Another site can't flip it, nor can a request with no browser
	// provenance at all.
	if c := post(t, ts.URL+"/displays/crow", form, "enabled=false", map[string]string{"Origin": "https://evil.example"}); c != 403 {
		t.Errorf("cross-origin switch: %d", c)
	}
	if c := post(t, ts.URL+"/displays/crow", form, "enabled=false", map[string]string{"Sec-Fetch-Site": "cross-site"}); c != 403 {
		t.Errorf("cross-site switch: %d", c)
	}
	if c := post(t, ts.URL+"/displays/crow", form, "enabled=false", nil); c != 403 {
		t.Errorf("switch without browser headers: %d", c)
	}
	if !st.View().Displays[0].Enabled {
		t.Fatal("refused requests switched the display")
	}
	// The dashboard's own form works.
	if c := post(t, ts.URL+"/displays/crow", form, "enabled=false", map[string]string{"Sec-Fetch-Site": "same-origin"}); c != 303 {
		t.Errorf("same-origin switch: %d", c)
	}
	if st.View().Displays[0].Enabled {
		t.Fatal("display still on")
	}
	if c := post(t, ts.URL+"/displays/nope", form, "enabled=true", map[string]string{"Sec-Fetch-Site": "same-origin"}); c != 404 {
		t.Errorf("unknown display: %d", c)
	}

	// sotot-cast reports its status without browser headers.
	if c := post(t, ts.URL+"/api/v1/displays/crow/status", "application/json", `{"status":"Waiting: TV is in use by Netflix"}`, nil); c != 204 {
		t.Errorf("status report: %d", c)
	}
	if !strings.Contains(get(t, ts.URL+"/"), "Off · Waiting: TV is in use by Netflix") {
		t.Error("page lacks the reported status")
	}
	if c := post(t, ts.URL+"/api/v1/displays/crow/status", "application/json", `{"status":"x"}`, map[string]string{"Origin": "https://evil.example"}); c != 403 {
		t.Errorf("cross-origin status report: %d", c)
	}
}
