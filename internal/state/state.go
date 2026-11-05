// Package state holds sotot's in-memory view of each source: its things,
// whether it has reported, its cache expiry, and its connection status.
// It's the running authority; the per-source JSON cache files on disk
// only seed it at startup.
package state

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/bradfitz/sotot/protocol"
)

// Status is a source's freshness, as shown on the dashboard.
type Status string

const (
	// Connected: a live authoritative session with an accepted snapshot.
	Connected Status = "connected"
	// Fresh: no live session, but the cached snapshot hasn't expired.
	Fresh Status = "fresh"
	// Stale: no live session and the cached snapshot has expired.
	Stale Status = "stale"
	// Never: no snapshot has ever been accepted (or the cache was lost).
	Never Status = "never"
)

var sourceIDRx = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidSourceID reports whether id is a valid source ID. Source IDs are
// used as file names, so they're restricted.
func ValidSourceID(id string) bool { return sourceIDRx.MatchString(id) }

// SourceConfig is the configured identity of a source.
type SourceConfig struct {
	ID   string // immutable; see ValidSourceID
	Name string // display name
}

// DisplayConfig is a configured display, such as a TV driven by
// sotot-cast, which can be switched on and off from the dashboard.
type DisplayConfig struct {
	ID   string `json:"id"` // see ValidSourceID
	Name string `json:"name"`
}

// Config configures a Store.
type Config struct {
	Sources  []SourceConfig
	Displays []DisplayConfig

	// CacheDir, if non-empty, is where per-source JSON cache files are
	// read at startup and written after changes.
	CacheDir string

	// Now and AfterFunc default to time.Now and time.AfterFunc; tests
	// replace them with a fake clock.
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) (stop func() bool)

	// FlushDelay is the longest a change waits before being written to
	// the cache. Zero means one second.
	FlushDelay time.Duration

	Logf func(format string, args ...any)
}

// Store is the state of all configured sources.
type Store struct {
	now       func() time.Time
	afterFunc func(time.Duration, func()) func() bool
	logf      func(string, ...any)
	instance  string
	cache     *cache // nil if no CacheDir

	mu       sync.Mutex
	order    []string // source IDs in configuration order
	sources  map[string]*source
	displays []*display // in configuration order
	nextGen  uint64
	revision uint64
	subs     map[*Subscription]bool
	closed   bool

	displaysFile string // where display switches persist; "" if none
}

type source struct {
	cfg SourceConfig

	things       map[string]protocol.Thing
	summary      string // the source's own short status, like "15 domains"
	hasSnapshot  bool
	expiresAt    time.Time // zero if never had a snapshot
	dataRevision uint64

	lastConnectedAt    time.Time
	lastDisconnectedAt time.Time
	lastMessageAt      time.Time
	lastPongAt         time.Time

	active       uint64 // authoritative session generation, or 0
	activeCancel func()
	pending      map[uint64]func() // sessions awaiting their first snapshot

	stopExpiry func() bool
	loadErr    string // problem reading this source's cache at startup
	cacheErr   string // most recent problem writing its cache
}

// New returns a Store for the configured sources, seeded from the cache.
// Every source starts disconnected: restarting never extends validity.
func New(cfg Config) (*Store, error) {
	s := &Store{
		now:       cfg.Now,
		afterFunc: cfg.AfterFunc,
		logf:      cfg.Logf,
		sources:   map[string]*source{},
		subs:      map[*Subscription]bool{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.afterFunc == nil {
		s.afterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	var b [8]byte
	rand.Read(b[:])
	s.instance = hex.EncodeToString(b[:])

	for _, sc := range cfg.Sources {
		if !ValidSourceID(sc.ID) {
			return nil, fmt.Errorf("invalid source ID %q", sc.ID)
		}
		if s.sources[sc.ID] != nil {
			return nil, fmt.Errorf("duplicate source ID %q", sc.ID)
		}
		s.order = append(s.order, sc.ID)
		s.sources[sc.ID] = &source{cfg: sc, things: map[string]protocol.Thing{}, pending: map[uint64]func(){}}
	}

	seenDisplay := map[string]bool{}
	for _, dc := range cfg.Displays {
		if !ValidSourceID(dc.ID) {
			return nil, fmt.Errorf("invalid display ID %q", dc.ID)
		}
		if seenDisplay[dc.ID] {
			return nil, fmt.Errorf("duplicate display ID %q", dc.ID)
		}
		seenDisplay[dc.ID] = true
		s.displays = append(s.displays, &display{cfg: dc, enabled: true})
	}

	if cfg.CacheDir != "" {
		s.displaysFile = filepath.Join(cfg.CacheDir, displaysFile)
		s.loadDisplays()
		c, err := openCache(cfg.CacheDir, s, cmp.Or(cfg.FlushDelay, time.Second))
		if err != nil {
			return nil, err
		}
		s.cache = c
		for _, id := range s.order {
			src := s.sources[id]
			rec, err := c.load(id)
			if err != nil {
				src.loadErr = err.Error()
				s.logf("state: source %s: %v", id, err)
				continue
			}
			if rec != nil {
				src.restore(rec)
			}
		}
		go c.run()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.sources {
		s.scheduleExpiryLocked(src)
	}
	return s, nil
}

// Close stops timers and flushes pending cache writes.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	for _, src := range s.sources {
		if src.stopExpiry != nil {
			src.stopExpiry()
		}
	}
	s.mu.Unlock()
	if s.cache != nil {
		return s.cache.close()
	}
	return nil
}

// Errors returned to ingestion sessions.
var (
	ErrUnknownSource = errors.New("unknown source")
	// ErrSuperseded means a newer session for the same source has
	// become authoritative; the old session must stop.
	ErrSuperseded = errors.New("session superseded by a newer connection")
)

// StartSession registers a new connection from a source. The session
// doesn't become authoritative until its first snapshot is applied; until
// then any existing session continues. cancel is called if the session
// is later superseded.
func (s *Store) StartSession(sourceID string, cancel func()) (gen uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[sourceID]
	if src == nil {
		return 0, ErrUnknownSource
	}
	s.nextGen++
	gen = s.nextGen
	src.pending[gen] = cancel
	src.lastConnectedAt = s.now()
	s.changedLocked(src, false)
	return gen, nil
}

// EndSession records that a session's connection closed. Closing a
// superseded session doesn't affect the current one.
func (s *Store) EndSession(sourceID string, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[sourceID]
	if src == nil {
		return
	}
	if _, ok := src.pending[gen]; ok {
		delete(src.pending, gen)
		s.changedLocked(src, false)
		return
	}
	if src.active == gen {
		src.active, src.activeCancel = 0, nil
		src.lastDisconnectedAt = s.now()
		s.scheduleExpiryLocked(src)
		s.changedLocked(src, true)
	}
}

// Pong records that a session answered a liveness ping.
func (s *Store) Pong(sourceID string, gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[sourceID]; src != nil && (src.active == gen || src.pending[gen] != nil) {
		src.lastPongAt = s.now()
	}
}

// Apply validates and applies a data message (a *protocol.Snapshot,
// *protocol.Delta, or *protocol.Refresh) from session gen, all or
// nothing, returning the ack to send.
//
// A snapshot from a pending session makes it authoritative, superseding
// (and canceling) the previous session.
func (s *Store) Apply(sourceID string, gen uint64, msg any) (protocol.Ack, error) {
	var cancelOld func()
	defer func() {
		if cancelOld != nil {
			cancelOld()
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[sourceID]
	if src == nil {
		return protocol.Ack{}, ErrUnknownSource
	}
	_, isPending := src.pending[gen]
	if !isPending && src.active != gen {
		return protocol.Ack{}, ErrSuperseded
	}
	now := s.now()
	var seq int64
	var validFor *int64
	switch m := msg.(type) {
	case *protocol.Snapshot:
		seq, validFor = m.Seq, m.ValidForSeconds
		things := make(map[string]protocol.Thing, len(m.Things))
		for _, t := range m.Things {
			things[t.ID] = t
		}
		src.things = things
		src.summary = m.Summary
		src.hasSnapshot = true
		if isPending {
			cancelOld = src.activeCancel
			src.active, src.activeCancel = gen, src.pending[gen]
			delete(src.pending, gen)
		}
	case *protocol.Delta:
		if isPending {
			return protocol.Ack{}, errors.New("first message must be a snapshot")
		}
		seq, validFor = m.Seq, m.ValidForSeconds
		n := len(src.things)
		for _, t := range m.Upsert {
			if _, ok := src.things[t.ID]; !ok {
				n++
			}
		}
		for _, id := range m.Remove {
			if _, ok := src.things[id]; ok {
				n--
			}
		}
		if n > protocol.MaxThings {
			return protocol.Ack{}, fmt.Errorf("delta would leave %d things; max is %d", n, protocol.MaxThings)
		}
		for _, t := range m.Upsert {
			src.things[t.ID] = t
		}
		for _, id := range m.Remove {
			delete(src.things, id)
		}
		if m.Summary != nil {
			src.summary = *m.Summary
		}
	case *protocol.Refresh:
		if isPending {
			return protocol.Ack{}, errors.New("first message must be a snapshot")
		}
		seq, validFor = m.Seq, m.ValidForSeconds
	default:
		return protocol.Ack{}, fmt.Errorf("unexpected message type %T", msg)
	}
	if validFor != nil {
		src.expiresAt = now.Add(time.Duration(*validFor) * time.Second)
	}
	src.lastMessageAt = now
	src.dataRevision++
	src.loadErr = "" // fresh data supersedes a lost cache
	s.scheduleExpiryLocked(src)
	s.changedLocked(src, true)
	return protocol.Ack{
		Type:       protocol.TypeAck,
		Seq:        seq,
		ReceivedAt: now.UTC(),
		ExpiresAt:  src.expiresAt.UTC(),
	}, nil
}

// scheduleExpiryLocked arranges for subscribers to hear about the status
// change when a disconnected source's data expires.
func (s *Store) scheduleExpiryLocked(src *source) {
	if src.stopExpiry != nil {
		src.stopExpiry()
		src.stopExpiry = nil
	}
	if s.closed || !src.hasSnapshot || src.active != 0 {
		return
	}
	d := src.expiresAt.Sub(s.now())
	if d <= 0 {
		return
	}
	src.stopExpiry = s.afterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.changedLocked(src, false)
	})
}

// changedLocked bumps the revision and notifies subscribers. If persist,
// the source's cache record is queued for writing.
func (s *Store) changedLocked(src *source, persist bool) {
	s.revision++
	if persist && s.cache != nil {
		s.cache.markDirty(src.cfg.ID)
	}
	for sub := range s.subs {
		select {
		case sub.c <- struct{}{}:
		default: // already pending; the subscriber will read the latest View
		}
	}
}

func (s *Store) setCacheErr(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[id]
	if src == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if src.cacheErr != msg {
		src.cacheErr = msg
		s.changedLocked(src, false)
	}
}

// Subscription notifies about changes.
type Subscription struct {
	s *Store
	c chan struct{}
}

// C returns a channel that receives a value after the state changes.
// Notifications coalesce: a slow reader just sees the latest View, so
// subscribers never block ingestion.
func (sub *Subscription) C() <-chan struct{} { return sub.c }

// Close stops notifications.
func (sub *Subscription) Close() {
	sub.s.mu.Lock()
	defer sub.s.mu.Unlock()
	delete(sub.s.subs, sub)
}

// Subscribe returns a subscription to state changes along with the
// current View, captured atomically so no change falls between them.
func (s *Store) Subscribe() (*Subscription, View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := &Subscription{s: s, c: make(chan struct{}, 1)}
	s.subs[sub] = true
	return sub, s.viewLocked()
}

// View is a consistent snapshot of the dashboard state.
type View struct {
	// Instance identifies this daemon process; Revision increases with
	// every change within it.
	Instance string        `json:"instance"`
	Revision uint64        `json:"revision"`
	Now      time.Time     `json:"now"`
	Sources  []SourceView  `json:"sources"`
	Displays []DisplayView `json:"displays,omitempty"`
}

// DisplayView is the state of a display.
type DisplayView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Enabled is whether the display should show the dashboard, as set
	// from the dashboard.
	Enabled bool `json:"enabled"`
	// Status is what the display's driver last reported doing, like
	// "Casting" or "Waiting: TV is in use by Netflix".
	Status   string     `json:"status,omitempty"`
	StatusAt *time.Time `json:"status_at,omitempty"`
}

// SourceView is the dashboard state of one source.
type SourceView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	// Connecting is whether a new session is awaiting its first
	// snapshot. The status reflects the previous data until then.
	Connecting  bool       `json:"connecting,omitempty"`
	HasSnapshot bool       `json:"has_snapshot"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// Summary is the source's own very short status, like "15 domains".
	Summary string `json:"summary,omitempty"`

	LastConnectedAt    *time.Time `json:"last_connected_at,omitempty"`
	LastDisconnectedAt *time.Time `json:"last_disconnected_at,omitempty"`
	LastMessageAt      *time.Time `json:"last_message_at,omitempty"`

	// Problems reading or writing this source's cache, if any.
	CacheError string `json:"cache_error,omitempty"`

	Things []protocol.Thing `json:"things"` // sorted by ID
}

// View returns the current state.
func (s *Store) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}

func (s *Store) viewLocked() View {
	now := s.now()
	v := View{Instance: s.instance, Revision: s.revision, Now: now.UTC()}
	for _, id := range s.order {
		v.Sources = append(v.Sources, s.sources[id].view(now))
	}
	for _, d := range s.displays {
		v.Displays = append(v.Displays, DisplayView{
			ID:       d.cfg.ID,
			Name:     cmp.Or(d.cfg.Name, d.cfg.ID),
			Enabled:  d.enabled,
			Status:   d.status,
			StatusAt: timePtr(d.statusAt),
		})
	}
	return v
}

func (src *source) status(now time.Time) Status {
	switch {
	case src.active != 0:
		return Connected
	case !src.hasSnapshot:
		return Never
	case now.Before(src.expiresAt):
		return Fresh
	default:
		return Stale
	}
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
}

func (src *source) view(now time.Time) SourceView {
	v := SourceView{
		ID:                 src.cfg.ID,
		Name:               cmp.Or(src.cfg.Name, src.cfg.ID),
		Status:             src.status(now),
		Connecting:         len(src.pending) > 0,
		HasSnapshot:        src.hasSnapshot,
		ExpiresAt:          timePtr(src.expiresAt),
		Summary:            src.summary,
		LastConnectedAt:    timePtr(src.lastConnectedAt),
		LastDisconnectedAt: timePtr(src.lastDisconnectedAt),
		LastMessageAt:      timePtr(src.lastMessageAt),
		CacheError:         cmp.Or(src.loadErr, src.cacheErr),
		Things:             make([]protocol.Thing, 0, len(src.things)),
	}
	for _, t := range src.things {
		v.Things = append(v.Things, t)
	}
	slices.SortFunc(v.Things, func(a, b protocol.Thing) int { return cmp.Compare(a.ID, b.ID) })
	return v
}
