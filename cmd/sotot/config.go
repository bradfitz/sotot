package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bradfitz/sotot/internal/ingest"
	"github.com/bradfitz/sotot/internal/state"
)

// Config is the JSON configuration file.
type Config struct {
	Version int            `json:"version"`
	Sources []SourceConfig `json:"sources"`
	// Displays are TVs (driven by sotot-cast) that can be switched on
	// and off from the dashboard.
	Displays []state.DisplayConfig `json:"displays,omitempty"`
}

// SourceConfig configures one source.
type SourceConfig struct {
	// ID is the source's immutable identity; cached data is keyed by it.
	ID string `json:"id"`
	// Name is the display name; it may change freely.
	Name string `json:"name"`
	// Listeners are tailcat listeners the source can connect to. One is
	// normal; two allow moving the source to a new address.
	Listeners []ListenerConfig `json:"listeners"`
}

// ListenerConfig configures one tailcat listener for a source.
type ListenerConfig struct {
	// IdentityFile holds the listener's private key material, relative
	// to the configuration file. Create one with "sotot new-identity".
	IdentityFile string `json:"identity_file"`
}

// loadedSource is a validated source with its listener identities.
type loadedSource struct {
	SourceConfig
	identities []*ingest.Identity
	paths      []string
}

func loadConfig(path string) ([]loadedSource, []state.DisplayConfig, error) {
	srcs, cfg, err := loadSources(path)
	if err != nil {
		return nil, nil, err
	}
	return srcs, cfg.Displays, nil
}

func loadSources(path string) ([]loadedSource, *Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Version != 1 {
		return nil, nil, fmt.Errorf("%s: unsupported config version %d", path, cfg.Version)
	}
	dir := filepath.Dir(path)
	var srcs []loadedSource
	seen := map[string]bool{}
	seenFile := map[string]string{}
	for _, sc := range cfg.Sources {
		if !state.ValidSourceID(sc.ID) {
			return nil, nil, fmt.Errorf("%s: invalid source id %q (want [a-z0-9][a-z0-9_-]{0,63})", path, sc.ID)
		}
		if seen[sc.ID] {
			return nil, nil, fmt.Errorf("%s: duplicate source id %q", path, sc.ID)
		}
		seen[sc.ID] = true
		if len(sc.Listeners) == 0 {
			return nil, nil, fmt.Errorf("%s: source %q has no listeners", path, sc.ID)
		}
		ls := loadedSource{SourceConfig: sc}
		for _, lc := range sc.Listeners {
			if lc.IdentityFile == "" {
				return nil, nil, fmt.Errorf("%s: source %q: listener without identity_file", path, sc.ID)
			}
			p := lc.IdentityFile
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			if other, ok := seenFile[p]; ok {
				// Sharing would let one source publish as another.
				return nil, nil, fmt.Errorf("%s: identity file %s used by both %q and %q", path, lc.IdentityFile, other, sc.ID)
			}
			seenFile[p] = sc.ID
			id, err := ingest.LoadIdentity(p)
			if err != nil {
				return nil, nil, fmt.Errorf("source %q: %w", sc.ID, err)
			}
			ls.identities = append(ls.identities, id)
			ls.paths = append(ls.paths, p)
		}
		srcs = append(srcs, ls)
	}
	if len(srcs) == 0 {
		return nil, nil, errors.New("no sources configured")
	}
	return srcs, &cfg, nil
}
