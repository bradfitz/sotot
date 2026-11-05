// Command sotot-demo-live is a fake sotot source for testing: it stays
// connected and publishes a changing set of bills to pay, messages and
// emails to answer, PRs to review, and meetings.
//
// It reads the source's tailcat address (from "sotot address") from
// --addr-file or $SOTOT_ADDR.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/bradfitz/sotot/source"
)

var (
	addrFile = flag.String("addr-file", "", "file containing the source's tailcat address (default $SOTOT_ADDR)")
	interval = flag.Duration("interval", 20*time.Second, "how often something changes")
	validFor = flag.Duration("valid-for", 10*time.Minute, "how long data stays fresh after disconnecting")
	zone     = flag.String("tz", "America/Los_Angeles", "time zone for date-only bills")
)

func readAddr() string {
	if *addrFile != "" {
		b, err := os.ReadFile(*addrFile)
		if err != nil {
			log.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if a := os.Getenv("SOTOT_ADDR"); a != "" {
		return a
	}
	log.Fatal("need --addr-file or $SOTOT_ADDR (from: sotot address SOURCE-ID)")
	return ""
}

func main() {
	flag.Parse()
	if _, err := time.LoadLocation(*zone); err != nil {
		log.Fatalf("--tz: %v", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	c := &source.Client{Addr: readAddr()}
	defer c.Close()
	c.SetValidFor(*validFor)
	w := newWorld(time.Now())
	if err := c.Set(w.things()); err != nil {
		log.Fatal(err)
	}
	go func() {
		t := time.NewTicker(*interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			log.Printf("demo: %s", w.step(time.Now()))
			if err := c.Set(w.things()); err != nil {
				log.Printf("demo: %v", err)
			}
			// The demo always knows its complete state, so each update
			// vouches for all of it.
			c.Refresh()
		}
	}()
	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

// world is the fake state of the user's obligations.
type world struct {
	bills    []bill
	messages []message
	prs      []pr
	meetings []meeting
	nextID   int
}

type bill struct {
	id, payee string
	amount    float64
	due       time.Time // date-only bills use just the date
	dateOnly  bool
}

type message struct {
	id, from, service, kind, text string
	at                            time.Time
}

type pr struct {
	id, repo, title string
	num             int
	at              time.Time
}

type meeting struct {
	id, title  string
	start, end time.Time
}

var (
	people   = []string{"Alice", "Bob", "Mom", "Priya", "Jordan", "Sam", "Wei", "Grandpa"}
	services = []string{"Signal", "Telegram", "iMessage", "WhatsApp"}
	chatter  = []string{
		"Are we still on for Saturday?",
		"Can you send me that recipe?",
		"Did you see the game last night?",
		"Call me when you get a chance.",
		"Running 10 minutes late!",
		"What do you want for your birthday?",
	}
	emailSubjects = []string{
		"Lease renewal: please sign by Friday",
		"Your order has a problem",
		"Re: school field trip permission slip",
		"Quarterly planning doc review",
		"Invitation: offsite logistics",
	}
	prTitles = []string{
		"netcheck: retry STUN on transient errors",
		"derp: reduce allocations in packet forwarding",
		"docs: clarify tailcat address secrecy",
		"cmd/sotot: add SIGHUP reload",
		"state: coalesce cache writes",
	}
	payees = []struct {
		name   string
		amount float64
	}{
		{"PG&E electric", 142.17}, {"Comcast internet", 79.99}, {"Water district", 63.40},
		{"Visa card", 1204.55}, {"Car insurance", 188.00}, {"Property tax installment", 3912.88},
	}
)

func pick[T any](xs []T) T { return xs[rand.IntN(len(xs))] }

func (w *world) id(prefix string) string {
	w.nextID++
	return fmt.Sprintf("%s:%d", prefix, w.nextID)
}

func newWorld(now time.Time) *world {
	w := &world{}
	for i, p := range payees[:3] {
		w.bills = append(w.bills, bill{
			id: w.id("bill"), payee: p.name, amount: p.amount,
			due: now.AddDate(0, 0, 2+5*i).Truncate(time.Hour), dateOnly: i != 1,
		})
	}
	w.messages = append(w.messages,
		message{id: w.id("msg"), from: "Alice", service: "Signal", kind: "message", text: "Are we still on for Saturday?", at: now.Add(-20 * time.Minute)},
		message{id: w.id("email"), from: "Landlord", service: "Gmail", kind: "email", text: "Lease renewal: please sign by Friday", at: now.Add(-2 * time.Hour)},
	)
	w.prs = append(w.prs, pr{id: w.id("pr"), repo: "tailscale/tailscale", title: pick(prTitles), num: 12345, at: now.Add(-26 * time.Hour)})
	w.meetings = append(w.meetings, meeting{id: w.id("mtg"), title: "1:1 with Jordan", start: now.Add(45 * time.Minute).Truncate(15 * time.Minute), end: now.Add(75 * time.Minute).Truncate(15 * time.Minute)})
	return w
}

// step makes one random change and describes it.
func (w *world) step(now time.Time) string {
	switch rand.IntN(6) {
	case 0, 1:
		if rand.IntN(2) == 0 {
			m := message{id: w.id("msg"), from: pick(people), service: pick(services), kind: "message", text: pick(chatter), at: now}
			w.messages = append(w.messages, m)
			return fmt.Sprintf("new %s message from %s", m.service, m.from)
		}
		m := message{id: w.id("email"), from: pick(people), service: "Gmail", kind: "email", text: pick(emailSubjects), at: now}
		w.messages = append(w.messages, m)
		return fmt.Sprintf("new email from %s", m.from)
	case 2:
		if len(w.messages) > 0 {
			i := rand.IntN(len(w.messages))
			m := w.messages[i]
			w.messages = append(w.messages[:i], w.messages[i+1:]...)
			return fmt.Sprintf("replied to %s", m.from)
		}
	case 3:
		if len(w.bills) > 0 && rand.IntN(2) == 0 {
			b := w.bills[0]
			w.bills = w.bills[1:]
			// Next month's bill arrives.
			p := pick(payees)
			w.bills = append(w.bills, bill{id: w.id("bill"), payee: p.name, amount: p.amount, due: now.AddDate(0, 0, 10+rand.IntN(20)), dateOnly: true})
			return fmt.Sprintf("paid %s ($%.2f)", b.payee, b.amount)
		}
	case 4:
		if len(w.prs) > 1 || (len(w.prs) == 1 && rand.IntN(2) == 0) {
			p := w.prs[0]
			w.prs = w.prs[1:]
			return fmt.Sprintf("reviewed %s#%d", p.repo, p.num)
		}
		p := pr{id: w.id("pr"), repo: "tailscale/tailscale", title: pick(prTitles), num: 12346 + w.nextID, at: now}
		w.prs = append(w.prs, p)
		return fmt.Sprintf("review requested on %s#%d", p.repo, p.num)
	}
	// Meetings: drop ended ones and keep one upcoming.
	var keep []meeting
	for _, m := range w.meetings {
		if m.end.After(now) {
			keep = append(keep, m)
		}
	}
	w.meetings = keep
	if len(w.meetings) == 0 {
		start := now.Add(time.Duration(30+rand.IntN(120)) * time.Minute).Truncate(15 * time.Minute)
		w.meetings = append(w.meetings, meeting{id: w.id("mtg"), title: "Sync with " + pick(people), start: start, end: start.Add(30 * time.Minute)})
		return "meeting scheduled"
	}
	return "no change"
}

func (w *world) things() []protocol.Thing {
	var ts []protocol.Thing
	for _, b := range w.bills {
		t := protocol.Thing{
			ID:    b.id,
			Title: fmt.Sprintf("Pay %s ($%.2f)", b.payee, b.amount),
			Kind:  "bill",
			URL:   "https://example.com/bills/" + strings.ReplaceAll(b.id, ":", "-"),
			Tags:  []string{"money"},
		}
		if b.dateOnly {
			t.Date, t.TimeZone = b.due.Format(time.DateOnly), *zone
		} else {
			due := time.Date(b.due.Year(), b.due.Month(), b.due.Day(), 17, 0, 0, 0, time.Local)
			t.DueAt = &due
		}
		ts = append(ts, t)
	}
	for _, m := range w.messages {
		at := m.at
		t := protocol.Thing{ID: m.id, Kind: m.kind, AttentionAt: &at, Tags: []string{strings.ToLower(m.service)}}
		if m.kind == "email" {
			t.Title = fmt.Sprintf("Answer email from %s: %s", m.from, m.text)
			t.URL = "https://mail.example.com/" + m.id
		} else {
			t.Title = fmt.Sprintf("Reply to %s (%s)", m.from, m.service)
			t.Body = fmt.Sprintf("%q", m.text)
		}
		ts = append(ts, t)
	}
	for _, p := range w.prs {
		at := p.at
		ts = append(ts, protocol.Thing{
			ID:          p.id,
			Title:       fmt.Sprintf("Review %s#%d: %s", p.repo, p.num, p.title),
			Kind:        "pull_request",
			URL:         fmt.Sprintf("https://github.com/%s/pull/%d", p.repo, p.num),
			AttentionAt: &at,
			Tags:        []string{"github"},
		})
	}
	for _, m := range w.meetings {
		start, end := m.start, m.end
		ts = append(ts, protocol.Thing{ID: m.id, Title: m.title, Kind: "meeting", StartsAt: &start, EndsAt: &end, Tags: []string{"calendar"}})
	}
	return ts
}
