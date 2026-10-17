package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tailscale/tailcat"
)

const identityVersion = 1

// Identity is the persistent material for one tailcat source listener:
// the node's private key, its pre-shared key, and its pinned DERP relay
// region. Restoring all of these keeps the listener's address stable
// across restarts.
//
// It's secret. Anyone holding it can impersonate the listener, and the
// address derived from it lets a client publish as that source.
type Identity struct {
	Version int                 `json:"version"`
	Key     *tailcat.PrivateKey `json:"tailcat"`
}

// NewIdentity creates a new identity, pinned to the DERP region with the
// lowest latency from here.
func NewIdentity(ctx context.Context) (*Identity, error) {
	dm, err := tailcat.FetchDERPMap(ctx, tailcat.ExpandForServer)
	if err != nil {
		return nil, fmt.Errorf("fetching DERP map: %w", err)
	}
	rid, err := tailcat.PickBestRegion(ctx, dm)
	if err != nil {
		return nil, fmt.Errorf("picking DERP region: %w", err)
	}
	region := dm.Regions[rid]
	if region == nil {
		return nil, errors.New("no reachable DERP region")
	}
	k := tailcat.NewPrivateKey()
	k.Public.Region = append(k.Public.Region, region)
	return &Identity{Version: identityVersion, Key: k}, nil
}

// Addr returns the tailcat address a source uses to reach the listener.
// It's a secret capability.
func (id *Identity) Addr() tailcat.Addr { return id.Key.Public.Addr() }

func (id *Identity) validate() error {
	if id.Version != identityVersion {
		return fmt.Errorf("unsupported identity version %d", id.Version)
	}
	if id.Key == nil || id.Key.Private.IsZero() {
		return errors.New("missing tailcat private key")
	}
	if id.Key.Private.Public() != id.Key.Public.ServerPublic.NodePublic {
		return errors.New("tailcat public key doesn't match private key")
	}
	if id.Key.Public.PresharedKey.IsZero() {
		return errors.New("missing pre-shared key")
	}
	if len(id.Key.Public.Region) != 1 || id.Key.Public.Region[0] == nil {
		return errors.New("identity must pin exactly one DERP region")
	}
	return nil
}

// LoadIdentity reads an identity file.
func LoadIdentity(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := id.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &id, nil
}

// WriteIdentity writes id to a new owner-only file at path. It refuses
// to overwrite an existing file, which would silently revoke that
// listener's address.
func WriteIdentity(path string, id *Identity) error {
	b, err := json.MarshalIndent(id, "", "\t")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}
