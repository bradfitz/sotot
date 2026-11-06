// Package present turns dashboard state into what's shown on screen:
// ordered entries with human-readable times, and source status labels.
// The browser dashboard and the TV view share it.
//
// The ordering is deliberately simple: by the priority sources assign
// (urgent, high, normal, low), then things with a time, earliest first, then the
// rest. sotot doesn't rank urgency itself.
package present

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
)

// Entry is one thing as displayed.
type Entry struct {
	Thing        protocol.Thing
	SourceID     string
	SourceName   string
	SourceStatus state.Status // the source's freshness; things from stale sources are flagged
	When         *When        // nil if the thing has no time
}

// When describes a thing's most relevant time.
type When struct {
	Label    string    // e.g. "due Fri Oct 23, 5:00 PM"
	Relative string    // e.g. "in 6 days", "2h ago", "today"
	At       time.Time // for ordering; for a date, the start of that day in its zone
	Past     bool
}

// Entries returns the things in v needing attention at now, ordered for
// display, with times shown in loc. Upcoming things (see Upcoming) are
// left out.
func Entries(v state.View, now time.Time, loc *time.Location) []Entry {
	var es []Entry
	for _, sv := range v.Sources {
		for _, t := range sv.Things {
			if t.Upcoming(now) {
				continue
			}
			es = append(es, Entry{
				Thing:        t,
				SourceID:     sv.ID,
				SourceName:   sv.Name,
				SourceStatus: sv.Status,
				When:         when(t, now, loc),
			})
		}
	}
	slices.SortStableFunc(es, func(a, b Entry) int {
		if c := cmp.Compare(priorityRank(a.Thing.Priority), priorityRank(b.Thing.Priority)); c != 0 {
			return c
		}
		switch {
		case a.When != nil && b.When != nil:
			if c := a.When.At.Compare(b.When.At); c != 0 {
				return c
			}
		case a.When != nil:
			return -1
		case b.When != nil:
			return 1
		}
		return cmp.Or(
			cmp.Compare(a.Thing.Title, b.Thing.Title),
			cmp.Compare(a.SourceID, b.SourceID),
			cmp.Compare(a.Thing.ID, b.Thing.ID),
		)
	})
	return es
}

// SourceEntries are some of one source's entries.
type SourceEntries struct {
	SourceID, SourceName string
	Entries              []Entry
}

// Upcoming returns, for each source in v with any, its next n things that
// don't need attention yet (their AttentionAt is after now), soonest
// first by their shown time, with times shown in loc.
func Upcoming(v state.View, now time.Time, loc *time.Location, n int) []SourceEntries {
	var out []SourceEntries
	for _, sv := range v.Sources {
		var es []Entry
		for _, t := range sv.Things {
			if !t.Upcoming(now) {
				continue
			}
			es = append(es, Entry{
				Thing:        t,
				SourceID:     sv.ID,
				SourceName:   sv.Name,
				SourceStatus: sv.Status,
				When:         when(t, now, loc),
			})
		}
		if len(es) == 0 {
			continue
		}
		slices.SortStableFunc(es, func(a, b Entry) int {
			// Every upcoming thing has a time: at least its AttentionAt.
			return cmp.Or(a.When.At.Compare(b.When.At), cmp.Compare(a.Thing.Title, b.Thing.Title), cmp.Compare(a.Thing.ID, b.Thing.ID))
		})
		out = append(out, SourceEntries{SourceID: sv.ID, SourceName: sv.Name, Entries: es[:min(n, len(es))]})
	}
	return out
}

// priorityRank orders priorities for display: urgent, high, normal, low.
func priorityRank(p string) int {
	switch p {
	case protocol.PriorityUrgent:
		return -1
	case protocol.PriorityHigh:
		return 0
	case protocol.PriorityLow:
		return 2
	}
	return 1
}

// when picks the time to show: a deadline, else an event start, else a
// date, else when attention should begin.
func when(t protocol.Thing, now time.Time, loc *time.Location) *When {
	switch {
	case t.DueAt != nil:
		return instant("due", *t.DueAt, now, loc)
	case t.StartsAt != nil:
		w := instant("starts", *t.StartsAt, now, loc)
		if t.EndsAt != nil && w.Past && now.Before(*t.EndsAt) {
			w.Label = "now, until " + t.EndsAt.In(loc).Format(time.Kitchen)
			w.Relative = "ends " + relative(*t.EndsAt, now)
		}
		return w
	case t.Date != "":
		return date(t, now)
	case t.AttentionAt != nil:
		w := instant("from", *t.AttentionAt, now, loc)
		if w.Past {
			w.Label = "since " + strings.TrimPrefix(w.Label, "from ")
		}
		return w
	}
	return nil
}

func instant(verb string, at, now time.Time, loc *time.Location) *When {
	at = at.In(loc)
	n := now.In(loc)
	var layout string
	switch {
	case sameDay(at, n):
		layout = "3:04 PM"
	case at.Year() == n.Year():
		layout = "Mon Jan 2, 3:04 PM"
	default:
		layout = "Mon Jan 2 2006, 3:04 PM"
	}
	return &When{
		Label:    verb + " " + at.Format(layout),
		Relative: relative(at, now),
		At:       at,
		Past:     !at.After(now),
	}
}

func date(t protocol.Thing, now time.Time) *When {
	loc, err := time.LoadLocation(t.TimeZone)
	if err != nil {
		loc = time.UTC // validated on input; can't happen
	}
	d, err := time.ParseInLocation(time.DateOnly, t.Date, loc)
	if err != nil {
		return nil
	}
	today := now.In(loc)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	days := int(d.Sub(today).Round(24*time.Hour) / (24 * time.Hour))
	layout := "Mon Jan 2"
	if d.Year() != today.Year() {
		layout = "Mon Jan 2 2006"
	}
	w := &When{Label: d.Format(layout), At: d, Past: days < 0}
	switch {
	case days == 0:
		w.Relative = "today"
	case days == 1:
		w.Relative = "tomorrow"
	case days == -1:
		w.Relative = "yesterday"
	case days > 1:
		w.Relative = fmt.Sprintf("in %d days", days)
	default:
		w.Relative = fmt.Sprintf("%d days ago", -days)
	}
	return w
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// relative formats the distance from now to t, like "in 3h" or "5m ago".
func relative(t, now time.Time) string {
	d := t.Sub(now)
	future := d > 0
	if !future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		s = fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		s = fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// StatusLabel describes a source's status in words, so it's never
// conveyed by color alone.
func StatusLabel(sv state.SourceView) string {
	var s string
	switch sv.Status {
	case state.Connected:
		// A live source's own summary ("15 domains") says more than
		// "Connected", in less space.
		s = cmp.Or(sv.Summary, "Connected")
	case state.Fresh:
		s = "Fresh, disconnected"
	case state.Stale:
		s = "Stale"
	case state.Never:
		s = "Never reported"
	default:
		s = string(sv.Status)
	}
	if sv.Connecting {
		s += " · connecting…"
	}
	return s
}

// StatusDetail is a longer explanation of a source's status, such as
// when its data expires.
func StatusDetail(sv state.SourceView, now time.Time) string {
	switch sv.Status {
	case state.Fresh:
		if sv.ExpiresAt != nil {
			return "data valid for " + strings.TrimPrefix(relative(*sv.ExpiresAt, now), "in ")
		}
	case state.Stale:
		if sv.ExpiresAt != nil {
			return "data expired " + relative(*sv.ExpiresAt, now)
		}
	case state.Connected:
		if sv.LastMessageAt != nil {
			return "updated " + relative(*sv.LastMessageAt, now)
		}
	}
	return ""
}

// Ago describes how long ago t was, like "5m ago" or "now".
func Ago(t, now time.Time) string { return relative(t, now) }
