// Command sotot-porkbun is a sotot source watching the domains in a
// Porkbun account: expiries that need a manual renewal, upcoming
// automatic renewals, non-active domains, and disagreements with the
// registry.
//
// Environment:
//
//	PORKBUN_API_KEY      a Porkbun API key ("pk1_..."), or PORKBUN_API_KEY_FILE
//	PORKBUN_SECRET_KEY   its secret key ("sk1_..."), or PORKBUN_SECRET_KEY_FILE
//	SOTOT_ADDR           this source's address (from "sotot address"), or
//	                     SOTOT_ADDR_FILE naming a file holding it
//
// Porkbun only lists domains with API access enabled (per domain, in its
// web UI). Run with --dry-run to print what it would publish without
// connecting to sotot.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/sotot/internal/domainwatch"
)

func main() {
	domainwatch.Main(func() (domainwatch.Registrar, error) {
		key, err := domainwatch.Secret("PORKBUN_API_KEY")
		if err != nil {
			return nil, err
		}
		secret, err := domainwatch.Secret("PORKBUN_SECRET_KEY")
		if err != nil {
			return nil, err
		}
		if key == "" || secret == "" {
			return nil, errors.New("set $PORKBUN_API_KEY and $PORKBUN_SECRET_KEY (or their _FILE variants)")
		}
		return &porkbun{apiKey: key, secretKey: secret}, nil
	})
}

// porkbun lists domains with the Porkbun v3 API and an API key pair.
type porkbun struct {
	apiKey, secretKey string
	base              string // API base URL; "https://api.porkbun.com/api/json/v3" if empty
}

func (p *porkbun) Name() string { return "Porkbun" }

// flexInt decodes a JSON number or numeric string; Porkbun has used both.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	*f = flexInt(n)
	return err
}

func (p *porkbun) List(ctx context.Context) ([]domainwatch.Registration, error) {
	base := p.base
	if base == "" {
		base = "https://api.porkbun.com/api/json/v3"
	}
	var regs []domainwatch.Registration
	for start := 0; ; {
		body, _ := json.Marshal(map[string]any{"apikey": p.apiKey, "secretapikey": p.secretKey, "start": start})
		req, err := http.NewRequestWithContext(ctx, "POST", base+"/domain/listAll", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := domainwatch.HTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		b, err := domainwatch.ReadBody(res)
		if err != nil {
			return nil, fmt.Errorf("porkbun: %w", err)
		}
		var resp struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Domains []struct {
				Domain     string  `json:"domain"`
				Status     string  `json:"status"`
				ExpireDate string  `json:"expireDate"` // "2027-01-15 10:00:00", zone unstated
				AutoRenew  flexInt `json:"autoRenew"`
			} `json:"domains"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return nil, fmt.Errorf("porkbun: decoding domain list: %w", err)
		}
		if resp.Status != "SUCCESS" {
			return nil, fmt.Errorf("porkbun: %s: %s", resp.Status, resp.Message)
		}
		for _, d := range resp.Domains {
			r := domainwatch.Registration{
				Domain:    strings.ToLower(d.Domain),
				AutoRenew: d.AutoRenew != 0,
				URL:       "https://porkbun.com/account/domainsSpeedy",
			}
			// The zone isn't documented; treat it as UTC. The registry
			// lookup is authoritative anyway, and differences of hours
			// don't matter for renewal reminders.
			if t, err := time.Parse(time.DateTime, d.ExpireDate); err == nil {
				r.Expires = t
			}
			if d.Status != "" && !strings.EqualFold(d.Status, "ACTIVE") {
				r.Problem = "status " + d.Status
			}
			regs = append(regs, r)
		}
		if len(resp.Domains) < 1000 { // the API's page size
			return regs, nil
		}
		start += len(resp.Domains)
	}
}
