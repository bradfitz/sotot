package domainwatch

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/bradfitz/sotot/protocol"
)

// domain is everything known about one domain.
type domain struct {
	Reg Registration // the registrar's view
	// Registry is the registry's view, if a lookup has succeeded.
	Registry *registryInfo
	// RegistryErr is the latest registry lookup error, and since when
	// lookups have been failing.
	RegistryErr      error
	RegistryErrSince time.Time
}

// expires returns the domain's best-known expiry and where it came from.
// The registry is authoritative; the registrar is the fallback.
func (d *domain) expires(registrar string) (time.Time, string) {
	if d.Registry != nil && !d.Registry.Expires.IsZero() {
		return d.Registry.Expires, "the registry"
	}
	if !d.Reg.Expires.IsZero() {
		return d.Reg.Expires, registrar
	}
	return time.Time{}, ""
}

// Policy says when a domain needs attention.
type Policy struct {
	// Horizon is how far ahead to warn about a domain that won't renew
	// itself.
	Horizon time.Duration
	// AutoRenewHorizon is how far ahead to mention an upcoming automatic
	// renewal, so there's time to check the payment method works.
	AutoRenewHorizon time.Duration
	// Mismatch is how much the registrar's and registry's expiries may
	// differ before that's worth checking.
	Mismatch time.Duration
	// LookupGrace is how long registry lookups may fail, for a domain
	// with no other known expiry, before saying so.
	LookupGrace time.Duration
}

// DefaultPolicy is the policy used unless flags change it.
var DefaultPolicy = Policy{
	Horizon:          90 * 24 * time.Hour,
	AutoRenewHorizon: 14 * 24 * time.Hour,
	Mismatch:         48 * time.Hour,
	LookupGrace:      72 * time.Hour,
}

func timeP(t time.Time) *time.Time { return &t }

// things returns the things needing attention among domains at
// registrar, and each domain's next renewal as an upcoming thing (with
// AttentionAt in the future) when it's further off. If listErr is non-nil, the registrar's API has been failing
// since listErrSince. Things without a natural time (problems,
// mismatches) have no AttentionAt; the caller stamps when it first saw
// them.
func (p Policy) things(registrar string, domains []*domain, listErr error, listErrSince, now time.Time) []protocol.Thing {
	var ts []protocol.Thing
	if listErr != nil {
		ts = append(ts, protocol.Thing{
			ID:          "list-error",
			Title:       fmt.Sprintf("Can't list %s domains", registrar),
			Body:        truncate(listErr.Error(), 500) + "\n\nUsing the last list fetched, if any.",
			Kind:        "source_error",
			AttentionAt: timeP(listErrSince),
		})
	}
	for _, d := range domains {
		ts = append(ts, p.domainThings(registrar, d, now)...)
	}
	slices.SortFunc(ts, func(a, b protocol.Thing) int { return cmp.Compare(a.ID, b.ID) })
	return ts
}

func (p Policy) domainThings(registrar string, d *domain, now time.Time) []protocol.Thing {
	var ts []protocol.Thing
	name := d.Reg.Domain
	id := "domain:" + name
	tags := []string{"domain"}

	if d.Reg.Problem != "" {
		ts = append(ts, protocol.Thing{
			ID:    id + ":problem",
			Title: fmt.Sprintf("Check %s: %s", name, d.Reg.Problem),
			Body:  fmt.Sprintf("%s reports %s for this domain.", registrar, d.Reg.Problem),
			Kind:  "domain_problem",
			URL:   d.Reg.URL,
			Tags:  tags,
		})
	}

	exp, from := d.expires(registrar)
	if exp.IsZero() {
		if d.RegistryErr != nil && now.Sub(d.RegistryErrSince) >= p.LookupGrace {
			ts = append(ts, protocol.Thing{
				ID:          id + ":unknown",
				Title:       fmt.Sprintf("Can't determine when %s expires", name),
				Body:        truncate(d.RegistryErr.Error(), 500),
				Kind:        "source_error",
				AttentionAt: timeP(d.RegistryErrSince),
				Tags:        tags,
			})
		}
		return ts
	}

	left := exp.Sub(now)
	date := exp.UTC().Format(time.DateOnly)
	switch {
	case left <= 0:
		body := fmt.Sprintf("Expired %s, per %s. Renew it at %s before it's deleted.", date, from, registrar)
		if d.Reg.AutoRenew {
			body = fmt.Sprintf("Expired %s, per %s, even though auto-renew is on. Check the payment method at %s.", date, from, registrar)
		}
		ts = append(ts, protocol.Thing{
			ID: id, Title: fmt.Sprintf("%s has expired", name), Body: body,
			Kind: "renewal", URL: d.Reg.URL, DueAt: timeP(exp), Tags: tags,
		})
	case !d.Reg.AutoRenew && left <= p.Horizon:
		ts = append(ts, protocol.Thing{
			ID:    id,
			Title: fmt.Sprintf("Renew %s (auto-renew is off)", name),
			Body:  fmt.Sprintf("Expires %s, per %s.", date, from),
			Kind:  "renewal", URL: d.Reg.URL, DueAt: timeP(exp), Tags: tags,
		})
	case d.Reg.AutoRenew && left <= p.AutoRenewHorizon:
		ts = append(ts, protocol.Thing{
			ID:    id,
			Title: fmt.Sprintf("%s auto-renews soon at %s", name, registrar),
			Body:  "Check that the payment method on file is still valid.",
			Kind:  "renewal", URL: d.Reg.URL, DueAt: timeP(exp), Tags: append(tags, "autorenew"),
		})
	case d.Reg.AutoRenew:
		// Upcoming: listed on the dashboard as what's next, until the
		// horizon, when it's the thing above.
		ts = append(ts, protocol.Thing{
			ID:    id,
			Title: fmt.Sprintf("%s auto-renews at %s", name, registrar),
			Body:  fmt.Sprintf("Expires %s, per %s.", date, from),
			Kind:  "renewal", URL: d.Reg.URL, DueAt: timeP(exp), Tags: append(tags, "autorenew"),
			AttentionAt: timeP(exp.Add(-p.AutoRenewHorizon)),
		})
	default:
		ts = append(ts, protocol.Thing{
			ID:    id,
			Title: fmt.Sprintf("Renew %s (auto-renew is off)", name),
			Body:  fmt.Sprintf("Expires %s, per %s.", date, from),
			Kind:  "renewal", URL: d.Reg.URL, DueAt: timeP(exp), Tags: tags,
			AttentionAt: timeP(exp.Add(-p.Horizon)),
		})
	}

	// The registrar may be wrong about, or out of sync with, the registry:
	// for example a renewal that was charged but never reached it.
	if d.Registry != nil && !d.Reg.Expires.IsZero() && !d.Registry.Expires.IsZero() {
		if diff := d.Reg.Expires.Sub(d.Registry.Expires); diff > p.Mismatch || diff < -p.Mismatch {
			ts = append(ts, protocol.Thing{
				ID:    id + ":mismatch",
				Title: fmt.Sprintf("Check %s: %s and the registry disagree on expiry", name, registrar),
				Body: fmt.Sprintf("%s says %s; the registry (via %s) says %s.",
					registrar, d.Reg.Expires.UTC().Format(time.DateOnly), d.Registry.Via, d.Registry.Expires.UTC().Format(time.DateOnly)),
				Kind: "domain_problem", URL: d.Reg.URL, Tags: tags,
			})
		}
	}
	return ts
}

// nextLookup returns when to next ask the registry about a domain:
// rarely when expiry is far off, often as it approaches or once it's
// past, so a renewal (or a failure to renew) shows up promptly.
func nextLookup(d *domain, now time.Time) time.Time {
	if d.Registry == nil {
		if d.RegistryErr != nil {
			// Back off failing lookups: 1h, 2h, 4h, ... up to a day.
			since := now.Sub(d.RegistryErrSince)
			return now.Add(min(max(since, time.Hour), 24*time.Hour))
		}
		return now
	}
	left := d.Registry.Expires.Sub(now)
	switch {
	case left > 120*24*time.Hour:
		return now.Add(7 * 24 * time.Hour)
	case left > 45*24*time.Hour:
		return now.Add(24 * time.Hour)
	case left > 7*24*time.Hour:
		return now.Add(6 * time.Hour)
	default:
		return now.Add(time.Hour)
	}
}
