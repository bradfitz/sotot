// Package domainwatch is the shared core of the sotot sources that watch
// the domains in a registrar account (cmd/sotot-gandi, cmd/sotot-porkbun):
// listing the account's domains, checking each one's expiry with its
// registry by RDAP or whois, deciding what needs attention, and
// publishing it.
//
// A registrar-specific command implements Registrar and calls Main.
package domainwatch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Registration is what a registrar says about one of the account's domains.
type Registration struct {
	Domain    string    // lowercase, without a trailing dot
	Expires   time.Time // zero if the registrar didn't say
	AutoRenew bool
	// Problem, if non-empty, is a status needing attention, such as a hold.
	Problem string
	URL     string // where to manage it
}

// Registrar lists the domains in an account.
type Registrar interface {
	// Name is the registrar's display name, such as "Gandi".
	Name() string
	List(context.Context) ([]Registration, error)
}

// HTTPClient is the client registrars and registry lookups should use.
var HTTPClient = &http.Client{Timeout: 60 * time.Second}

// ReadBody reads and closes res's body, bounded in size, returning an
// error with a snippet of the body if the status isn't 200.
func ReadBody(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", res.Status, truncate(strings.TrimSpace(string(b)), 200))
	}
	return b, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
