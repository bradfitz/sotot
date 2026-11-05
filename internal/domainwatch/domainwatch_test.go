package domainwatch

import (
	"context"
	"fmt"
	"github.com/bradfitz/sotot/protocol"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRDAP(t *testing.T) {
	// Abbreviated from a real rdap.verisign.com response.
	b := []byte(`{"objectClassName":"domain","ldhName":"EXAMPLE.COM",
		"events":[{"eventAction":"registration","eventDate":"1995-08-14T04:00:00Z"},
			{"eventAction":"expiration","eventDate":"2027-08-13T04:00:00Z"},
			{"eventAction":"last update of RDAP database","eventDate":"2026-10-17T22:30:11Z"}],
		"entities":[{"objectClassName":"entity","roles":["registrar"],
			"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","RESERVED-Internet Assigned Numbers Authority"]]]}]}`)
	info, err := parseRDAP(b)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Expires.Equal(time.Date(2027, 8, 13, 4, 0, 0, 0, time.UTC)) || info.Registrar != "RESERVED-Internet Assigned Numbers Authority" {
		t.Fatalf("%+v", info)
	}
}

func TestParseWhois(t *testing.T) {
	tests := []struct {
		name, resp string
		want       time.Time
		wantErr    bool
	}{
		{"io", "Domain Name: nic.io\nRegistrar: Example Registrar\nRegistry Expiry Date: 2027-05-01T00:00:02Z\n", time.Date(2027, 5, 1, 0, 0, 2, 0, time.UTC), false},
		{"co fractional", "Registry Expiry Date: 2027-10-04T23:59:59.0Z\n", time.Date(2027, 10, 4, 23, 59, 59, 0, time.UTC), false},
		{"paid-till", "domain: EXAMPLE.RU\npaid-till:     2027-03-02T21:00:00Z\n", time.Date(2027, 3, 2, 21, 0, 0, 0, time.UTC), false},
		{"date only", "Expiry date:  2027-12-01\n", time.Date(2027, 12, 1, 0, 0, 0, 0, time.UTC), false},
		{"not found", "No match for domain \"NOPE.IO\".\n", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := parseWhois(tt.resp)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !info.Expires.Equal(tt.want) {
				t.Fatalf("expires %v; want %v", info.Expires, tt.want)
			}
		})
	}
}

func TestRegistryFallsBackToWhois(t *testing.T) {
	boot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"services":[[["com"],["http://rdap.invalid/","https://rdap.example/com/v1/"]]]}`)
	}))
	defer boot.Close()
	var queries []string
	rc := &registryClient{
		bootstrapURL: boot.URL,
		ianaWhois:    "iana:43",
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			c1, c2 := net.Pipe()
			go func() {
				defer c2.Close()
				buf := make([]byte, 256)
				n, _ := c2.Read(buf)
				q := strings.TrimSpace(string(buf[:n]))
				queries = append(queries, addr+" "+q)
				switch addr {
				case "iana:43":
					io.WriteString(c2, "domain: IO\nwhois:        whois.nic.io\n")
				case "whois.nic.io:43":
					io.WriteString(c2, "Registry Expiry Date: 2027-05-01T00:00:02Z\n")
				}
			}()
			return c1, nil
		},
	}
	info, err := rc.Lookup(context.Background(), "nic.io")
	if err != nil {
		t.Fatal(err)
	}
	if info.Via != "whois" || info.Expires.Year() != 2027 {
		t.Fatalf("%+v", info)
	}
	if want := "[iana:43 io whois.nic.io:43 nic.io]"; fmt.Sprint(queries) != want {
		t.Fatalf("queries %v; want %v", queries, want)
	}
	if base, _ := rc.rdapBase(context.Background(), "com"); base != "https://rdap.example/com/v1/" {
		t.Fatalf("com RDAP base %q; want the https one", base)
	}
}

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func days(n int) time.Time { return now.Add(time.Duration(n) * 24 * time.Hour) }

func regDomain(name string, exp time.Time, autoRenew bool) *domain {
	return &domain{
		Reg:      Registration{Domain: name, Expires: exp, AutoRenew: autoRenew},
		Registry: &registryInfo{Expires: exp, Via: "rdap"},
	}
}

func titles(ts []protocol.Thing) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Title)
	}
	return strings.Join(s, " | ")
}

func TestPolicy(t *testing.T) {
	p := DefaultPolicy
	tests := []struct {
		name string
		d    *domain
		want string
	}{
		{"far off, manual", regDomain("a.com", days(200), false), ""},
		{"within horizon, manual", regDomain("a.com", days(60), false), "Renew a.com (auto-renew is off)"},
		{"auto-renew, far", regDomain("a.com", days(60), true), ""},
		{"auto-renew, soon", regDomain("a.com", days(10), true), "a.com auto-renews soon at Gandi"},
		{"expired despite auto-renew", regDomain("a.com", days(-1), true), "a.com has expired"},
		{"registrar has no expiry, registry does", func() *domain {
			d := regDomain("b.io", days(30), false)
			d.Reg.Expires = time.Time{}
			return d
		}(), "Renew b.io (auto-renew is off)"},
		{"registrar and registry disagree", func() *domain {
			d := regDomain("a.com", days(400), true)
			d.Registry.Expires = days(35)
			return d
		}(), "Check a.com: Gandi and the registry disagree on expiry"},
		{"hold", func() *domain {
			d := regDomain("a.com", days(400), true)
			d.Reg.Problem = "clientHold"
			return d
		}(), "Check a.com: clientHold"},
		{"lookup failing briefly", &domain{Reg: Registration{Domain: "c.io"}, RegistryErr: fmt.Errorf("timeout"), RegistryErrSince: now.Add(-time.Hour)}, ""},
		{"lookup failing long", &domain{Reg: Registration{Domain: "c.io"}, RegistryErr: fmt.Errorf("timeout"), RegistryErrSince: days(-4)}, "Can't determine when c.io expires"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := p.things("Gandi", []*domain{tt.d}, nil, time.Time{}, now)
			if got := titles(ts); got != tt.want {
				t.Fatalf("got %q; want %q", got, tt.want)
			}
			for _, th := range ts {
				if err := th.Validate(); err != nil {
					t.Fatalf("invalid thing: %v", err)
				}
			}
		})
	}
}

func TestListErrorThing(t *testing.T) {
	ts := DefaultPolicy.things("Porkbun", nil, fmt.Errorf("403 Forbidden"), days(-1), now)
	if len(ts) != 1 || ts[0].Kind != "source_error" || !strings.Contains(ts[0].Body, "403") {
		t.Fatalf("%+v", ts)
	}
}

func TestNextLookup(t *testing.T) {
	for _, tt := range []struct {
		left time.Duration
		want time.Duration
	}{
		{365 * 24 * time.Hour, 7 * 24 * time.Hour},
		{60 * 24 * time.Hour, 24 * time.Hour},
		{20 * 24 * time.Hour, 6 * time.Hour},
		{2 * 24 * time.Hour, time.Hour},
		{-time.Hour, time.Hour},
	} {
		d := &domain{Registry: &registryInfo{Expires: now.Add(tt.left)}}
		if got := nextLookup(d, now).Sub(now); got != tt.want {
			t.Errorf("expiry in %v: next lookup in %v; want %v", tt.left, got, tt.want)
		}
	}
}

func TestSummary(t *testing.T) {
	w := &watcher{reg: fakeRegistrar("Gandi"), policy: DefaultPolicy, domains: map[string]*domain{
		"a.com": regDomain("a.com", days(400), true),
		"b.com": regDomain("b.com", days(300), false),
	}}
	if got := w.summary(now); got != "2 domains" {
		t.Errorf("summary = %q; want 2 domains", got)
	}
	w.domains["c.com"] = regDomain("c.com", days(20), false) // needs renewing
	if got := w.summary(now); got != "3 domains, 1 to check" {
		t.Errorf("summary = %q; want 3 domains, 1 to check", got)
	}
}

type fakeRegistrar string

func (f fakeRegistrar) Name() string                                 { return string(f) }
func (f fakeRegistrar) List(context.Context) ([]Registration, error) { return nil, nil }
