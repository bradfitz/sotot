package state

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// displaysFile, in the cache directory, holds the display switches. Its
// leading underscore keeps it from colliding with any source's file.
const displaysFile = "_displays.json"

// display is a configured display's state.
type display struct {
	cfg      DisplayConfig
	enabled  bool   // persisted
	status   string // reported by its driver; not persisted
	statusAt time.Time
}

// ErrUnknownDisplay is returned for a display ID that's not configured.
var ErrUnknownDisplay = errors.New("unknown display")

func (s *Store) displayLocked(id string) *display {
	for _, d := range s.displays {
		if d.cfg.ID == id {
			return d
		}
	}
	return nil
}

// SetDisplayEnabled switches a display on or off, persisting the choice.
func (s *Store) SetDisplayEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.displayLocked(id)
	if d == nil {
		return ErrUnknownDisplay
	}
	if d.enabled == enabled {
		return nil
	}
	d.enabled = enabled
	s.changedLocked(nil, false)
	return s.saveDisplaysLocked()
}

// SetDisplayStatus records what a display's driver says it's doing.
func (s *Store) SetDisplayStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.displayLocked(id)
	if d == nil {
		return ErrUnknownDisplay
	}
	d.statusAt = s.now()
	if d.status != status {
		d.status = status
		s.changedLocked(nil, false)
	}
	return nil
}

func (s *Store) loadDisplays() {
	if s.displaysFile == "" {
		return
	}
	b, err := os.ReadFile(s.displaysFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logf("state: reading display switches: %v", err)
		}
		return
	}
	var saved map[string]bool // display ID -> enabled
	if err := json.Unmarshal(b, &saved); err != nil {
		s.logf("state: reading display switches: %v", err)
		return
	}
	for _, d := range s.displays {
		if on, ok := saved[d.cfg.ID]; ok {
			d.enabled = on
		}
	}
}

func (s *Store) saveDisplaysLocked() error {
	if s.displaysFile == "" {
		return nil
	}
	saved := map[string]bool{}
	for _, d := range s.displays {
		saved[d.cfg.ID] = d.enabled
	}
	b, _ := json.Marshal(saved)
	tmp := s.displaysFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.displaysFile)
}
