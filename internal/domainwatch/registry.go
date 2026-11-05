package domainwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// registryInfo is what a domain's registry says, via RDAP or whois.
type registryInfo struct {
	Expires   time.Time
	Registrar string // sponsoring registrar's name, if given
	Via       string // "rdap" or "whois"
}

// registryClient looks up domains at their registries: RDAP where the
// TLD has it (per IANA's bootstrap registry), else port-43 whois.
type registryClient struct {
	// Overridable for tests.
	bootstrapURL string // IANA RDAP DNS bootstrap file
	ianaWhois    string // host:port of the IANA whois server
	dial         func(ctx context.Context, addr string) (net.Conn, error)

	mu          sync.Mutex
	rdapBases   map[string]string // TLD -> RDAP base URL
	bootFetched time.Time
	whoisHosts  map[string]string // TLD -> whois host:port, or "" if none
}

func (rc *registryClient) Lookup(ctx context.Context, domain string) (registryInfo, error) {
	tld := domain[strings.LastIndex(domain, ".")+1:]
	base, err := rc.rdapBase(ctx, tld)
	if err != nil {
		return registryInfo{}, err
	}
	if base != "" {
		return rc.rdap(ctx, base, domain)
	}
	return rc.whois(ctx, tld, domain)
}

func (rc *registryClient) rdapBase(ctx context.Context, tld string) (string, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.rdapBases == nil || time.Since(rc.bootFetched) > 24*time.Hour {
		u := rc.bootstrapURL
		if u == "" {
			u = "https://data.iana.org/rdap/dns.json"
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return "", err
		}
		res, err := HTTPClient.Do(req)
		if err != nil {
			if rc.rdapBases != nil {
				return rc.rdapBases[tld], nil // use the stale copy
			}
			return "", fmt.Errorf("fetching RDAP bootstrap: %w", err)
		}
		b, err := ReadBody(res)
		if err != nil {
			return "", fmt.Errorf("fetching RDAP bootstrap: %w", err)
		}
		var boot struct {
			Services [][][]string `json:"services"`
		}
		if err := json.Unmarshal(b, &boot); err != nil {
			return "", fmt.Errorf("decoding RDAP bootstrap: %w", err)
		}
		m := map[string]string{}
		for _, svc := range boot.Services {
			if len(svc) != 2 || len(svc[1]) == 0 {
				continue
			}
			url := svc[1][0]
			for _, u := range svc[1] { // prefer https
				if strings.HasPrefix(u, "https://") {
					url = u
					break
				}
			}
			for _, t := range svc[0] {
				m[strings.ToLower(t)] = strings.TrimSuffix(url, "/") + "/"
			}
		}
		rc.rdapBases, rc.bootFetched = m, time.Now()
	}
	return rc.rdapBases[tld], nil
}

func (rc *registryClient) rdap(ctx context.Context, base, domain string) (registryInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"domain/"+domain, nil)
	if err != nil {
		return registryInfo{}, err
	}
	req.Header.Set("Accept", "application/rdap+json")
	res, err := HTTPClient.Do(req)
	if err != nil {
		return registryInfo{}, err
	}
	b, err := ReadBody(res)
	if err != nil {
		return registryInfo{}, fmt.Errorf("rdap: %w", err)
	}
	return parseRDAP(b)
}

func parseRDAP(b []byte) (registryInfo, error) {
	var d struct {
		Events []struct {
			Action string    `json:"eventAction"`
			Date   time.Time `json:"eventDate"`
		} `json:"events"`
		Entities []struct {
			Roles      []string          `json:"roles"`
			VCardArray []json.RawMessage `json:"vcardArray"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return registryInfo{}, fmt.Errorf("rdap: %w", err)
	}
	info := registryInfo{Via: "rdap"}
	for _, e := range d.Events {
		if e.Action == "expiration" {
			info.Expires = e.Date
		}
	}
	for _, e := range d.Entities {
		for _, role := range e.Roles {
			if role == "registrar" && len(e.VCardArray) == 2 {
				info.Registrar = vcardFN(e.VCardArray[1])
			}
		}
	}
	if info.Expires.IsZero() {
		return info, errors.New("rdap: no expiration event")
	}
	return info, nil
}

// vcardFN returns the "fn" (formatted name) of a jCard property list.
func vcardFN(props json.RawMessage) string {
	var ps [][]any
	if json.Unmarshal(props, &ps) != nil {
		return ""
	}
	for _, p := range ps {
		if len(p) >= 4 && p[0] == "fn" {
			if s, ok := p[3].(string); ok {
				return s
			}
		}
	}
	return ""
}

func (rc *registryClient) dialer() func(context.Context, string) (net.Conn, error) {
	if rc.dial != nil {
		return rc.dial
	}
	var d net.Dialer
	return func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
}

// whoisQuery sends a whois query and returns the response.
func (rc *registryClient) whoisQuery(ctx context.Context, server, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := rc.dialer()(ctx, server)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "%s\r\n", query); err != nil {
		return "", err
	}
	var sb strings.Builder
	sc := bufio.NewScanner(c)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() && sb.Len() < 1<<20 {
		sb.WriteString(sc.Text())
		sb.WriteByte('\n')
	}
	return sb.String(), sc.Err()
}

func (rc *registryClient) whoisServer(ctx context.Context, tld string) (string, error) {
	rc.mu.Lock()
	host, ok := rc.whoisHosts[tld]
	rc.mu.Unlock()
	if ok {
		return host, nil
	}
	iana := rc.ianaWhois
	if iana == "" {
		iana = "whois.iana.org:43"
	}
	resp, err := rc.whoisQuery(ctx, iana, tld)
	if err != nil {
		return "", fmt.Errorf("asking IANA for .%s whois server: %w", tld, err)
	}
	host = ""
	for _, line := range strings.Split(resp, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "whois" {
			if v = strings.TrimSpace(v); v != "" {
				host = net.JoinHostPort(v, "43")
			}
		}
	}
	rc.mu.Lock()
	if rc.whoisHosts == nil {
		rc.whoisHosts = map[string]string{}
	}
	rc.whoisHosts[tld] = host
	rc.mu.Unlock()
	return host, nil
}

func (rc *registryClient) whois(ctx context.Context, tld, domain string) (registryInfo, error) {
	server, err := rc.whoisServer(ctx, tld)
	if err != nil {
		return registryInfo{}, err
	}
	if server == "" {
		return registryInfo{}, fmt.Errorf("no RDAP or whois service for .%s", tld)
	}
	resp, err := rc.whoisQuery(ctx, server, domain)
	if err != nil {
		return registryInfo{}, fmt.Errorf("whois %s: %w", server, err)
	}
	return parseWhois(resp)
}

// whoisExpiryKeys are the field names registries use for the expiry date,
// most specific first.
var whoisExpiryKeys = []string{
	"registry expiry date",
	"registrar registration expiration date",
	"expiry date",
	"expiration date",
	"expiration time",
	"expires on",
	"expires",
	"paid-till",
	"renewal date",
}

var whoisDateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.0Z",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"02-Jan-2006",
	"2006.01.02",
	"2006/01/02",
}

var spaceRx = regexp.MustCompile(`\s+`)

func parseWhois(resp string) (registryInfo, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(resp, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = spaceRx.ReplaceAllString(strings.ToLower(strings.TrimSpace(k)), " ")
		if _, dup := fields[k]; !dup {
			fields[k] = strings.TrimSpace(v)
		}
	}
	info := registryInfo{Via: "whois", Registrar: fields["registrar"]}
	for _, k := range whoisExpiryKeys {
		v := fields[k]
		if v == "" {
			continue
		}
		v = strings.Fields(v)[0] // drop trailing zone names like "(UTC)"
		for _, layout := range whoisDateLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				info.Expires = t
				return info, nil
			}
		}
		return info, fmt.Errorf("whois: unparseable %s %q", k, v)
	}
	if strings.Contains(strings.ToLower(resp), "no match") || strings.Contains(strings.ToLower(resp), "not found") {
		return info, errors.New("whois: domain not found")
	}
	return info, errors.New("whois: no expiry date in response")
}
