package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bradfitz/sotot/protocol"
)

const cacheVersion = 1

// cacheRecord is the JSON form of one source's cache file. It holds the
// last known data, not session state: an active session is never
// persisted as evidence of a live source.
type cacheRecord struct {
	Version            int              `json:"version"`
	SourceID           string           `json:"source_id"`
	HasSnapshot        bool             `json:"has_snapshot"`
	ExpiresAt          *time.Time       `json:"expires_at,omitempty"`
	DataRevision       uint64           `json:"data_revision"`
	LastConnectedAt    *time.Time       `json:"last_connected_at,omitempty"`
	LastDisconnectedAt *time.Time       `json:"last_disconnected_at,omitempty"`
	LastMessageAt      *time.Time       `json:"last_message_at,omitempty"`
	Things             []protocol.Thing `json:"things"`
}

func (src *source) record() *cacheRecord {
	v := src.view(time.Time{})
	return &cacheRecord{
		Version:            cacheVersion,
		SourceID:           src.cfg.ID,
		HasSnapshot:        src.hasSnapshot,
		ExpiresAt:          timePtr(src.expiresAt),
		DataRevision:       src.dataRevision,
		LastConnectedAt:    timePtr(src.lastConnectedAt),
		LastDisconnectedAt: timePtr(src.lastDisconnectedAt),
		LastMessageAt:      timePtr(src.lastMessageAt),
		Things:             v.Things,
	}
}

func (src *source) restore(rec *cacheRecord) {
	src.hasSnapshot = rec.HasSnapshot
	if rec.ExpiresAt != nil {
		src.expiresAt = *rec.ExpiresAt // as written; a restart never extends it
	}
	src.dataRevision = rec.DataRevision
	if rec.LastConnectedAt != nil {
		src.lastConnectedAt = *rec.LastConnectedAt
	}
	if rec.LastDisconnectedAt != nil {
		src.lastDisconnectedAt = *rec.LastDisconnectedAt
	}
	if rec.LastMessageAt != nil {
		src.lastMessageAt = *rec.LastMessageAt
	}
	for _, t := range rec.Things {
		src.things[t.ID] = t
	}
}

// cache writes per-source records to dir, coalescing rapid changes.
type cache struct {
	dir        string
	store      *Store
	flushDelay time.Duration

	mu    sync.Mutex
	dirty map[string]bool
	wake  chan struct{} // capacity 1
	stop  chan struct{}
	done  chan struct{}
}

func openCache(dir string, s *Store, flushDelay time.Duration) (*cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &cache{
		dir:        dir,
		store:      s,
		flushDelay: flushDelay,
		dirty:      map[string]bool{},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}, nil
}

func (c *cache) path(id string) string { return filepath.Join(c.dir, id+".json") }

// load reads a source's record. A missing file returns (nil, nil). A
// corrupt one is renamed aside for diagnosis, so it's neither treated as
// valid data nor overwritten.
func (c *cache) load(id string) (*cacheRecord, error) {
	b, err := os.ReadFile(c.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading cache: %w", err)
	}
	rec, err := parseRecord(b, id)
	if err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", c.path(id), time.Now().Unix())
		if rerr := os.Rename(c.path(id), aside); rerr != nil {
			return nil, fmt.Errorf("corrupt cache (%v), and can't move it aside: %v", err, rerr)
		}
		return nil, fmt.Errorf("corrupt cache moved to %s: %v", filepath.Base(aside), err)
	}
	return rec, nil
}

func parseRecord(b []byte, id string) (*cacheRecord, error) {
	var rec cacheRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	if rec.Version != cacheVersion {
		return nil, fmt.Errorf("unsupported cache version %d", rec.Version)
	}
	if rec.SourceID != id {
		return nil, fmt.Errorf("cache is for source %q", rec.SourceID)
	}
	seen := map[string]bool{}
	for i := range rec.Things {
		t := &rec.Things[i]
		if err := t.Validate(); err != nil {
			return nil, err
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("duplicate thing id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return &rec, nil
}

func (c *cache) markDirty(id string) {
	c.mu.Lock()
	c.dirty[id] = true
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run writes dirty records, at most flushDelay after they change.
// Failed writes are retried.
func (c *cache) run() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			c.flush()
			return
		case <-c.wake:
		}
		select {
		case <-c.stop:
			c.flush()
			return
		case <-time.After(c.flushDelay):
		}
		if !c.flush() {
			// Retry failures after a pause, unless stopping.
			select {
			case <-c.stop:
				c.flush()
				return
			case <-time.After(5 * time.Second):
				c.markDirty("") // re-wake; "" is never a real ID
			}
		}
	}
}

// flush writes all dirty records and reports whether all succeeded.
func (c *cache) flush() bool {
	c.mu.Lock()
	ids := c.dirty
	c.dirty = map[string]bool{}
	c.mu.Unlock()
	ok := true
	for id := range ids {
		if id == "" {
			continue
		}
		c.store.mu.Lock()
		src := c.store.sources[id]
		var rec *cacheRecord
		if src != nil {
			rec = src.record()
		}
		c.store.mu.Unlock()
		if rec == nil {
			continue
		}
		err := c.write(id, rec)
		c.store.setCacheErr(id, err)
		if err != nil {
			c.store.logf("state: writing cache for %s: %v", id, err)
			ok = false
			c.mu.Lock()
			c.dirty[id] = true
			c.mu.Unlock()
		}
	}
	return ok
}

// write atomically replaces id's cache file with rec.
func (c *cache) write(id string, rec *cacheRecord) error {
	b, err := json.MarshalIndent(rec, "", "\t")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.dir, id+".json.tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path(id)); err != nil {
		return err
	}
	if d, err := os.Open(c.dir); err == nil {
		d.Sync() // best effort; not all systems support it
		d.Close()
	}
	return nil
}

func (c *cache) close() error {
	close(c.stop)
	<-c.done
	return nil
}
