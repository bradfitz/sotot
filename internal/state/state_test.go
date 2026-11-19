package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bradfitz/sotot/protocol"
)

// fakeClock is a manually advanced clock for Config.Now and AfterFunc.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// Advance moves time forward, running timers that come due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

func newTestStore(t *testing.T, clock *fakeClock, cacheDir string) *Store {
	t.Helper()
	s, err := New(Config{
		Sources:    []SourceConfig{{ID: "bills", Name: "Bills"}, {ID: "chat", Name: "Chat"}},
		CacheDir:   cacheDir,
		Now:        clock.Now,
		AfterFunc:  clock.AfterFunc,
		FlushDelay: time.Millisecond,
		Logf:       t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func snapshot(seq int64, validFor time.Duration, things ...protocol.Thing) *protocol.Snapshot {
	if things == nil {
		things = []protocol.Thing{}
	}
	return &protocol.Snapshot{Type: protocol.TypeSnapshot, Seq: seq, ValidForSeconds: protocol.Seconds(validFor), Things: things}
}

func thing(id string) protocol.Thing { return protocol.Thing{ID: id, Title: "Title " + id} }

func sourceView(t *testing.T, s *Store, id string) SourceView {
	t.Helper()
	for _, sv := range s.View().Sources {
		if sv.ID == id {
			return sv
		}
	}
	t.Fatalf("no source %q", id)
	return SourceView{}
}

func thingIDs(sv SourceView) string {
	var ids []string
	for _, t := range sv.Things {
		ids = append(ids, t.ID)
	}
	return strings.Join(ids, ",")
}

func mustApply(t *testing.T, s *Store, id string, gen uint64, msg any) protocol.Ack {
	t.Helper()
	ack, err := s.Apply(id, gen, msg)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return ack
}

func TestStatusLifecycle(t *testing.T) {
	clock := newFakeClock()
	s := newTestStore(t, clock, "")
	defer s.Close()

	if got := sourceView(t, s, "bills").Status; got != Never {
		t.Fatalf("initial status = %v; want never", got)
	}
	gen, err := s.StartSession("bills", func() {})
	if err != nil {
		t.Fatal(err)
	}
	sv := sourceView(t, s, "bills")
	if sv.Status != Never || !sv.Connecting {
		t.Fatalf("awaiting snapshot: status %v connecting %v; want never, connecting", sv.Status, sv.Connecting)
	}

	// An accepted empty snapshot is fresh data, not "never reported".
	ack := mustApply(t, s, "bills", gen, snapshot(1, time.Hour))
	if !ack.ExpiresAt.Equal(clock.Now().Add(time.Hour)) || ack.Seq != 1 {
		t.Errorf("ack = %+v", ack)
	}
	if sv := sourceView(t, s, "bills"); sv.Status != Connected || sv.Connecting || !sv.HasSnapshot {
		t.Fatalf("after snapshot: %+v", sv)
	}

	// A live session that stops renewing its data goes stale too: the
	// source may be stuck.
	sub, _ := s.Subscribe()
	defer sub.Close()
	clock.Advance(2 * time.Hour)
	if sv := sourceView(t, s, "bills"); sv.Status != Stale || !sv.Live {
		t.Fatalf("live past expiry: status %v, live %v; want stale, live", sv.Status, sv.Live)
	}
	select {
	case <-sub.C():
	default:
		t.Fatal("expiry of a live source didn't notify subscribers")
	}
	// Renewing makes it current again.
	mustApply(t, s, "bills", gen, &protocol.Refresh{Type: protocol.TypeRefresh, Seq: 2, ValidForSeconds: protocol.Seconds(time.Hour)})
	if got := sourceView(t, s, "bills").Status; got != Connected {
		t.Fatalf("after refresh: status %v; want connected", got)
	}
	clock.Advance(2 * time.Hour)

	// After disconnecting, the stored expiry applies immediately; there's
	// no extra grace period starting at the disconnect.
	s.EndSession("bills", gen)
	if got := sourceView(t, s, "bills").Status; got != Stale {
		t.Fatalf("disconnected past expiry: status %v; want stale", got)
	}
}

func TestTimedExpiryNotifies(t *testing.T) {
	clock := newFakeClock()
	s := newTestStore(t, clock, "")
	defer s.Close()
	gen, _ := s.StartSession("bills", func() {})
	mustApply(t, s, "bills", gen, snapshot(1, time.Hour, thing("electric")))
	s.EndSession("bills", gen)
	if got := sourceView(t, s, "bills").Status; got != Fresh {
		t.Fatalf("status %v; want fresh", got)
	}

	sub, _ := s.Subscribe()
	defer sub.Close()
	clock.Advance(59 * time.Minute)
	select {
	case <-sub.C():
		t.Fatal("notified before expiry")
	default:
	}
	clock.Advance(time.Minute)
	select {
	case <-sub.C():
	default:
		t.Fatal("no notification at expiry")
	}
	sv := sourceView(t, s, "bills")
	if sv.Status != Stale || thingIDs(sv) != "electric" {
		t.Fatalf("after expiry: status %v things %q; want stale things kept visible", sv.Status, thingIDs(sv))
	}
}

func TestDeltaAndRefresh(t *testing.T) {
	clock := newFakeClock()
	s := newTestStore(t, clock, "")
	defer s.Close()
	gen, _ := s.StartSession("chat", func() {})

	if _, err := s.Apply("chat", gen, &protocol.Delta{Type: "delta", Seq: 1}); err == nil {
		t.Fatal("delta before snapshot accepted")
	}
	ack := mustApply(t, s, "chat", gen, snapshot(1, time.Hour, thing("a"), thing("b")))
	expires := ack.ExpiresAt

	clock.Advance(10 * time.Minute)
	upd := thing("b")
	upd.Title = "updated"
	ack = mustApply(t, s, "chat", gen, &protocol.Delta{Type: "delta", Seq: 2,
		Upsert: []protocol.Thing{upd, thing("c")}, Remove: []string{"a", "never-existed"}})
	if !ack.ExpiresAt.Equal(expires) {
		t.Errorf("delta without validity changed expiry to %v; want %v", ack.ExpiresAt, expires)
	}
	sv := sourceView(t, s, "chat")
	if thingIDs(sv) != "b,c" || sv.Things[0].Title != "updated" {
		t.Fatalf("after delta: %+v", sv.Things)
	}

	sum := "2 unread"
	mustApply(t, s, "chat", gen, &protocol.Delta{Type: "delta", Seq: 3, Summary: &sum})
	if got := sourceView(t, s, "chat").Summary; got != "2 unread" {
		t.Fatalf("summary %q after delta", got)
	}
	mustApply(t, s, "chat", gen, &protocol.Delta{Type: "delta", Seq: 4, Remove: []string{"zz"}})
	if got := sourceView(t, s, "chat").Summary; got != "2 unread" {
		t.Fatalf("delta without summary changed it to %q", got)
	}

	ack = mustApply(t, s, "chat", gen, &protocol.Refresh{Type: "refresh", Seq: 5, ValidForSeconds: protocol.Seconds(24 * time.Hour)})
	if want := clock.Now().Add(24 * time.Hour); !ack.ExpiresAt.Equal(want) {
		t.Errorf("refresh expiry %v; want %v", ack.ExpiresAt, want)
	}

	// A new snapshot replaces the whole set.
	mustApply(t, s, "chat", gen, snapshot(6, time.Hour, thing("z")))
	if got := thingIDs(sourceView(t, s, "chat")); got != "z" {
		t.Fatalf("after second snapshot: %q; want z", got)
	}
	if got := sourceView(t, s, "chat").Summary; got != "" {
		t.Fatalf("snapshot without summary left %q", got)
	}
}

func TestRejectedDeltaKeepsState(t *testing.T) {
	clock := newFakeClock()
	s := newTestStore(t, clock, "")
	defer s.Close()
	gen, _ := s.StartSession("chat", func() {})
	var many []protocol.Thing
	for i := 0; i < protocol.MaxThings; i++ {
		many = append(many, protocol.Thing{ID: fmt.Sprint("t", i), Title: "x"})
	}
	mustApply(t, s, "chat", gen, snapshot(1, time.Hour, many...))
	before := sourceView(t, s, "chat")
	_, err := s.Apply("chat", gen, &protocol.Delta{Type: "delta", Seq: 2,
		Upsert: []protocol.Thing{thing("one-too-many")}, Remove: []string{many[0].ID + "-not-there"}})
	if err == nil {
		t.Fatal("delta exceeding MaxThings accepted")
	}
	after := sourceView(t, s, "chat")
	if len(after.Things) != len(before.Things) {
		t.Fatalf("rejected delta changed state: %d things, was %d", len(after.Things), len(before.Things))
	}
}

func TestSessionTakeover(t *testing.T) {
	clock := newFakeClock()
	s := newTestStore(t, clock, "")
	defer s.Close()

	oldCanceled := false
	oldGen, _ := s.StartSession("chat", func() { oldCanceled = true })
	mustApply(t, s, "chat", oldGen, snapshot(1, time.Hour, thing("old")))

	newGen, _ := s.StartSession("chat", func() {})
	// Until the new session's snapshot, the old one stays authoritative.
	mustApply(t, s, "chat", oldGen, &protocol.Delta{Type: "delta", Seq: 2, Upsert: []protocol.Thing{thing("old2")}})
	if sv := sourceView(t, s, "chat"); !sv.Connecting || sv.Status != Connected {
		t.Fatalf("during takeover: %+v", sv)
	}
	if oldCanceled {
		t.Fatal("old session canceled before new snapshot")
	}

	mustApply(t, s, "chat", newGen, snapshot(1, time.Hour, thing("new")))
	if !oldCanceled {
		t.Fatal("old session not canceled after new snapshot")
	}
	if got := thingIDs(sourceView(t, s, "chat")); got != "new" {
		t.Fatalf("after takeover: %q; want new", got)
	}
	// The old session's queued messages are fenced off...
	if _, err := s.Apply("chat", oldGen, &protocol.Delta{Type: "delta", Seq: 3, Upsert: []protocol.Thing{thing("zombie")}}); err != ErrSuperseded {
		t.Fatalf("old session delta: err %v; want ErrSuperseded", err)
	}
	// ...and its disconnect doesn't mark the new session offline.
	s.EndSession("chat", oldGen)
	if got := sourceView(t, s, "chat").Status; got != Connected {
		t.Fatalf("after old disconnect: %v; want connected", got)
	}
}

func TestCacheRestartNoExtension(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	s := newTestStore(t, clock, dir)
	gen, _ := s.StartSession("bills", func() {})
	mustApply(t, s, "bills", gen, snapshot(1, time.Hour, thing("electric")))
	// Persist while connected: the cache must not record liveness.
	s.Close()

	clock.Advance(30 * time.Minute)
	s2 := newTestStore(t, clock, dir)
	sv := sourceView(t, s2, "bills")
	if sv.Status != Fresh || thingIDs(sv) != "electric" {
		t.Fatalf("after restart: status %v things %q; want fresh electric", sv.Status, thingIDs(sv))
	}
	if want := time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC); !sv.ExpiresAt.Equal(want) {
		t.Fatalf("expiry %v after restart; want unchanged %v", sv.ExpiresAt, want)
	}
	// The restored expiry still fires.
	clock.Advance(30 * time.Minute)
	if got := sourceView(t, s2, "bills").Status; got != Stale {
		t.Fatalf("status %v; want stale", got)
	}
	if got := sourceView(t, s2, "chat").Status; got != Never {
		t.Fatalf("uncached source status %v; want never", got)
	}
	s2.Close()
}

func TestCorruptCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bills.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	s := newTestStore(t, clock, dir)
	defer s.Close()
	sv := sourceView(t, s, "bills")
	if sv.Status != Never || !strings.Contains(sv.CacheError, "corrupt") {
		t.Fatalf("corrupt cache: %+v", sv)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "bills.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("corrupt file not preserved: %v", matches)
	}
	// Other sources are unaffected.
	if sv := sourceView(t, s, "chat"); sv.CacheError != "" {
		t.Fatalf("chat: %+v", sv)
	}
}

func TestCacheWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	clock := newFakeClock()
	s := newTestStore(t, clock, dir)
	defer s.Close()
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)

	gen, _ := s.StartSession("bills", func() {})
	mustApply(t, s, "bills", gen, snapshot(1, time.Hour, thing("electric")))
	deadline := time.Now().Add(5 * time.Second)
	for sourceView(t, s, "bills").CacheError == "" {
		if time.Now().After(deadline) {
			t.Fatal("no cache error reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Real-time operation keeps working.
	if sv := sourceView(t, s, "bills"); sv.Status != Connected || thingIDs(sv) != "electric" {
		t.Fatalf("after write failure: %+v", sv)
	}
}

func TestDisplays(t *testing.T) {
	dir := t.TempDir()
	clock := newFakeClock()
	newStore := func() *Store {
		s, err := New(Config{
			Displays: []DisplayConfig{{ID: "crow", Name: "Office TV"}},
			CacheDir: dir, Now: clock.Now, AfterFunc: clock.AfterFunc, Logf: t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := newStore()
	if d := s.View().Displays; len(d) != 1 || !d[0].Enabled || d[0].Name != "Office TV" {
		t.Fatalf("initial displays: %+v", d)
	}
	sub, _ := s.Subscribe()
	if err := s.SetDisplayEnabled("crow", false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.C():
	default:
		t.Fatal("switching a display didn't notify")
	}
	if err := s.SetDisplayStatus("crow", "Off"); err != nil {
		t.Fatal(err)
	}
	if d := s.View().Displays[0]; d.Enabled || d.Status != "Off" || d.StatusAt == nil {
		t.Fatalf("after switch: %+v", d)
	}
	if err := s.SetDisplayEnabled("nope", true); err != ErrUnknownDisplay {
		t.Fatalf("unknown display: %v", err)
	}
	s.Close()

	// The switch survives a restart; the status doesn't.
	s = newStore()
	defer s.Close()
	if d := s.View().Displays[0]; d.Enabled || d.Status != "" {
		t.Fatalf("after restart: %+v", d)
	}
}

func TestDisplayAutoOff(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock() // 2026-10-17 12:00 UTC: 5 AM in Los Angeles
	s, err := New(Config{
		Displays:  []DisplayConfig{{ID: "crow", AutoOff: "17:00"}, {ID: "garage"}},
		Location:  la,
		CacheDir:  t.TempDir(),
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	enabled := func() string {
		var out []string
		for _, d := range s.View().Displays {
			out = append(out, fmt.Sprintf("%s=%v", d.ID, d.Enabled))
		}
		return strings.Join(out, " ")
	}
	clock.Advance(12*time.Hour - time.Minute) // 4:59 PM
	if got := enabled(); got != "crow=true garage=true" {
		t.Fatalf("before 5 PM: %s", got)
	}
	clock.Advance(time.Minute) // 5 PM
	if got := enabled(); got != "crow=false garage=true" {
		t.Fatalf("at 5 PM: %s", got)
	}
	// Switched back on, it stays on until 5 PM the next day.
	s.SetDisplayEnabled("crow", true)
	clock.Advance(23 * time.Hour)
	if got := enabled(); got != "crow=true garage=true" {
		t.Fatalf("next day before 5 PM: %s", got)
	}
	clock.Advance(time.Hour)
	if got := enabled(); got != "crow=false garage=true" {
		t.Fatalf("next day at 5 PM: %s", got)
	}

	if _, err := New(Config{Displays: []DisplayConfig{{ID: "x", AutoOff: "5pm"}}}); err == nil {
		t.Error("bad auto_off accepted")
	}
}

func TestNextTimeOfDay(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	for _, tt := range []struct{ now, want string }{
		{"2026-11-14 09:00", "2026-11-14 17:00"},
		{"2026-11-14 17:00", "2026-11-15 17:00"},
		{"2026-11-14 18:00", "2026-11-15 17:00"},
		{"2026-10-31 18:00", "2026-11-01 17:00"}, // across the end of DST
	} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", tt.now, la)
		if got := nextTimeOfDay(now, 17*60).Format("2006-01-02 15:04"); got != tt.want {
			t.Errorf("after %s: %s; want %s", tt.now, got, tt.want)
		}
	}
}
