package present

import (
	"strings"
	"testing"
	"time"

	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/protocol"
)

func TestEntriesOrder(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	v := state.View{Sources: []state.SourceView{{ID: "s", Name: "S", Things: []protocol.Thing{
		{ID: "soon", Title: "soon", DueAt: at(time.Hour)},
		{ID: "later-high", Title: "later-high", DueAt: at(48 * time.Hour), Priority: protocol.PriorityHigh},
		{ID: "untimed", Title: "untimed"},
		{ID: "soonest-low", Title: "soonest-low", DueAt: at(time.Minute), Priority: protocol.PriorityLow},
		{ID: "later", Title: "later", DueAt: at(24 * time.Hour)},
		{ID: "urgent", Title: "urgent", Priority: protocol.PriorityUrgent},
	}}}}
	var got []string
	for _, e := range Entries(v, now, time.UTC) {
		got = append(got, e.Thing.ID)
	}
	if want := "urgent later-high soon later untimed soonest-low"; strings.Join(got, " ") != want {
		t.Fatalf("order %q; want %q", strings.Join(got, " "), want)
	}
}

func TestUpcoming(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	day := 24 * time.Hour
	v := state.View{Sources: []state.SourceView{
		{ID: "domains", Name: "Domains", Things: []protocol.Thing{
			{ID: "now", Title: "now", DueAt: at(10 * day)},
			{ID: "d4", Title: "d4", DueAt: at(400 * day), AttentionAt: at(310 * day)},
			{ID: "d1", Title: "d1", DueAt: at(100 * day), AttentionAt: at(10 * day)},
			{ID: "d3", Title: "d3", DueAt: at(300 * day), AttentionAt: at(210 * day)},
			{ID: "d2", Title: "d2", DueAt: at(200 * day), AttentionAt: at(110 * day)},
			{ID: "begun", Title: "begun", DueAt: at(50 * day), AttentionAt: at(-day)},
		}},
		{ID: "chat", Name: "Chat", Things: []protocol.Thing{{ID: "m", Title: "m", AttentionAt: at(-time.Hour)}}},
	}}
	var got []string
	for _, e := range Entries(v, now, time.UTC) {
		got = append(got, e.Thing.ID)
	}
	if want := "m now begun"; strings.Join(got, " ") != want {
		t.Errorf("entries %q; want %q", strings.Join(got, " "), want)
	}
	up := Upcoming(v, now, time.UTC, 3)
	if len(up) != 1 || up[0].SourceName != "Domains" {
		t.Fatalf("upcoming %+v", up)
	}
	got = nil
	for _, e := range up[0].Entries {
		got = append(got, e.Thing.ID)
	}
	if want := "d1 d2 d3"; strings.Join(got, " ") != want {
		t.Errorf("upcoming %q; want %q", strings.Join(got, " "), want)
	}
	// Once its time comes, an upcoming thing needs attention.
	if es := Entries(v, now.Add(11*day), time.UTC); len(es) != 4 {
		t.Errorf("%d entries 11 days later; want 4", len(es))
	}
}
