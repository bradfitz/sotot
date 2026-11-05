// Command sotot-gandi is a sotot source watching the domains in a Gandi
// account: expiries that need a manual renewal, upcoming automatic
// renewals, holds, and disagreements with the registry.
//
// Environment:
//
//	GANDI_TOKEN   a Gandi personal access token with read access to
//	              domains ("See and renew domain names"), or
//	              GANDI_TOKEN_FILE naming a file holding it
//	SOTOT_ADDR    this source's address (from "sotot address"), or
//	              SOTOT_ADDR_FILE naming a file holding it
//
// Run with --dry-run to print what it would publish without connecting
// to sotot.
package main

import (
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
		tok, err := domainwatch.Secret("GANDI_TOKEN")
		if err != nil {
			return nil, err
		}
		if tok == "" {
			return nil, errors.New("set $GANDI_TOKEN (or $GANDI_TOKEN_FILE) to a Gandi personal access token")
		}
		return &gandi{token: tok}, nil
	})
}

// gandi lists domains with the Gandi v5 API and a personal access token.
type gandi struct {
	token string
	base  string // API base URL; "https://api.gandi.net" if empty
}

func (g *gandi) Name() string { return "Gandi" }

func (g *gandi) List(ctx context.Context) ([]domainwatch.Registration, error) {
	base := g.base
	if base == "" {
		base = "https://api.gandi.net"
	}
	var regs []domainwatch.Registration
	for page := 1; ; page++ {
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v5/domain/domains?per_page=100&page=%d", base, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+g.token)
		res, err := domainwatch.HTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		b, err := domainwatch.ReadBody(res)
		if err != nil {
			return nil, fmt.Errorf("gandi: %w", err)
		}
		var ds []struct {
			FQDN  string `json:"fqdn"`
			Dates struct {
				RegistryEndsAt *time.Time `json:"registry_ends_at"`
			} `json:"dates"`
			AutoRenew bool     `json:"autorenew"` // a bool in the list call
			Status    []string `json:"status"`
		}
		if err := json.Unmarshal(b, &ds); err != nil {
			return nil, fmt.Errorf("gandi: decoding domain list: %w", err)
		}
		for _, d := range ds {
			r := domainwatch.Registration{
				Domain:    strings.ToLower(d.FQDN),
				AutoRenew: d.AutoRenew,
				URL:       "https://admin.gandi.net/domain/",
			}
			if d.Dates.RegistryEndsAt != nil {
				r.Expires = *d.Dates.RegistryEndsAt
			}
			for _, s := range d.Status {
				if s == "clientHold" || s == "serverHold" || s == "pendingTransfer" {
					r.Problem = s
				}
			}
			regs = append(regs, r)
		}
		total, err := strconv.Atoi(res.Header.Get("Total-Count"))
		if len(ds) == 0 || (err == nil && len(regs) >= total) {
			return regs, nil
		}
		if page > 1000 {
			return nil, fmt.Errorf("gandi: too many pages")
		}
	}
}
