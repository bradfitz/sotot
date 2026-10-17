package ingest

import (
	"path/filepath"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

func TestIdentityRoundTrip(t *testing.T) {
	k := tailcat.NewPrivateKey()
	k.Public.Region = []*tailcfg.DERPRegion{{
		RegionID:   900,
		RegionCode: "test",
		Nodes:      []*tailcfg.DERPNode{{Name: "900a", RegionID: 900, HostName: "derp.example.com"}},
	}}
	id := &Identity{Version: identityVersion, Key: k}
	path := filepath.Join(t.TempDir(), "id.json")
	if err := WriteIdentity(path, id); err != nil {
		t.Fatal(err)
	}
	if err := WriteIdentity(path, id); err == nil {
		t.Fatal("WriteIdentity overwrote an existing file")
	}
	got, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr() != id.Addr() {
		t.Fatalf("address changed across round trip:\n got %s\nwant %s", got.Addr(), id.Addr())
	}
	if _, err := tailcat.ParseAddr(got.Addr()); err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}
}
