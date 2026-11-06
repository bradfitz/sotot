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
