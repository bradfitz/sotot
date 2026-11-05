// Command sotot-gmail is a sotot source for email needing attention: by
// default, Gmail threads in the inbox labeled "todo". Each matching
// thread is a thing; it goes away when the thread loses the label or
// leaves the inbox.
//
// It reads Gmail with the gmail.metadata scope (labels and headers, never
// message bodies; no changes), as you, via an OAuth client
// you create in your own Google Cloud project. See README.md for setting
// that up, then run once, interactively:
//
//	sotot-gmail login --client=client_secret_....json --token=gmail-token.json
//
// After that, run it with:
//
//	SOTOT_GMAIL_TOKEN_FILE=gmail-token.json SOTOT_ADDR_FILE=... sotot-gmail
//
// Use --dry-run to check access without connecting to sotot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/bradfitz/sotot/source"
)

var (
	tokenFile = flag.String("token", os.Getenv("SOTOT_GMAIL_TOKEN_FILE"), "token file written by `sotot-gmail login` (default $SOTOT_GMAIL_TOKEN_FILE)")
	labels    = flag.String("labels", "INBOX,todo", "comma-separated Gmail labels a thread must all have to need attention")
	interval  = flag.Duration("interval", 10*time.Minute, "most time between full searches, even if Gmail reports no changes")
	checkInt  = flag.Duration("check-interval", 15*time.Second, "how often to check (cheaply) whether the mailbox changed, and search if so")
	account   = flag.Int("account", 0, "which signed-in Gmail account links open in (the N in mail.google.com/mail/u/N)")
	dryRun    = flag.Bool("dry-run", false, "search once, print what would be published as JSON, and exit")
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "login" {
		if err := loginCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "debug" {
		if err := debugCmd(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	flag.Parse()
	if *tokenFile == "" {
		log.Fatal("need --token or $SOTOT_GMAIL_TOKEN_FILE (from: sotot-gmail login)")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	gm, err := newGmailFromTokenFile(ctx, *tokenFile)
	if err != nil {
		log.Fatal(err)
	}
	var labelList []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labelList = append(labelList, l)
		}
	}
	if len(labelList) == 0 {
		log.Fatal("--labels is empty")
	}
	w := &watcher{gm: gm, labels: labelList, account: *account, cache: map[string]threadInfo{}}

	if *dryRun {
		ts, sum, err := w.poll(ctx)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("summary: %s", sum)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(ts)
		return
	}

	c, err := source.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	c.SetValidFor(5**interval + 10*time.Minute)
	if err := c.Set([]protocol.Thing{}); err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := c.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	}()

	var last []protocol.Thing
	var lastSum string
	var failing error
	var failingSince time.Time
	t := time.NewTicker(*checkInt)
	defer t.Stop()
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		// Checking the mailbox's history ID costs 1 quota unit; a full
		// search costs 10 plus 40 per changed thread. So search only
		// when something in the mailbox changed (any change: archiving
		// a thread can touch only messages without the label), or as a
		// safety net every --interval.
		hid, need := w.needSearch(ctx, *interval)
		if !need {
			continue
		}
		ts, sum, err := w.poll(ctx)
		if err != nil {
			log.Printf("gmail: %v", err)
			if failing == nil {
				failingSince = time.Now()
			}
			failing = err
			ts, sum = last, lastSum
		} else {
			last, lastSum, failing = ts, sum, nil
			w.searched(hid)
			c.Refresh() // the search vouches for all of it
		}
		if failing != nil {
			if th, ok := errorThing(failing, failingSince, 3**interval); ok {
				ts = append(append([]protocol.Thing(nil), ts...), th)
			}
		}
		if err := c.SetSummary(sum); err != nil {
			log.Printf("summary: %v", err)
		}
		if err := c.Set(ts); err != nil {
			log.Printf("publishing: %v", err)
		}
	}
}

// errorThing returns a thing describing a failure to read Gmail, if it's
// worth showing: at once if access was revoked or expired (it won't fix
// itself), else once it has persisted for grace.
func errorThing(err error, since time.Time, grace time.Duration) (protocol.Thing, bool) {
	if errors.Is(err, errReauth) {
		return protocol.Thing{
			ID:          "gmail-reauth",
			Title:       "sotot can no longer read Gmail: run sotot-gmail login",
			Body:        err.Error() + "\n\nGoogle revokes access when your password changes, when you remove the app's access, or after six months unused.",
			Kind:        "source_error",
			AttentionAt: &since,
		}, true
	}
	if time.Since(since) < grace {
		return protocol.Thing{}, false
	}
	return protocol.Thing{
		ID:          "gmail-error",
		Title:       "Can't read Gmail",
		Body:        err.Error() + "\n\nUsing the last results, if any.",
		Kind:        "source_error",
		AttentionAt: &since,
	}, true
}

// watcher searches Gmail and turns matching threads into things.
type watcher struct {
	gm      *gmailClient
	labels  []string
	account int
	cache   map[string]threadInfo // by thread ID; reused while historyId is unchanged

	lastHistory string    // mailbox history ID as of the last successful search
	lastSearch  time.Time // when that was
}

// needSearch reports whether the mailbox has changed since the last
// successful search, or that was more than maxAge ago, returning the
// mailbox's current history ID to pass to searched afterward. If the
// check itself fails, it reports a search is needed, so the search
// reports the problem.
func (w *watcher) needSearch(ctx context.Context, maxAge time.Duration) (historyID string, need bool) {
	hid, err := w.gm.historyID(ctx)
	if err != nil {
		return "", true
	}
	return hid, hid != w.lastHistory || time.Since(w.lastSearch) >= maxAge
}

// searched records a successful search that started when the mailbox's
// history ID was historyID. (Taken before searching, so a change during
// the search is caught by the next check.)
func (w *watcher) searched(historyID string) {
	w.lastHistory, w.lastSearch = historyID, time.Now()
}

// threadInfo is what's shown for a thread.
type threadInfo struct {
	historyID string
	subject   string
	from      string
	last      time.Time // newest message
}

func (w *watcher) poll(ctx context.Context) ([]protocol.Thing, string, error) {
	threads, err := w.gm.threadsWithLabels(ctx, w.labels, 500)
	if err != nil {
		return nil, "", err
	}
	ts := []protocol.Thing{}
	seen := map[string]bool{}
	for _, th := range threads {
		seen[th.ID] = true
		info, ok := w.cache[th.ID]
		if !ok || info.historyID != th.HistoryID {
			info, err = w.gm.threadInfo(ctx, th.ID)
			if err != nil {
				return nil, "", err
			}
			info.historyID = th.HistoryID
			w.cache[th.ID] = info
		}
		ts = append(ts, w.thing(th.ID, info))
	}
	for id := range w.cache {
		if !seen[id] {
			delete(w.cache, id)
		}
	}
	sum := "nothing to do"
	if len(ts) > 0 {
		sum = fmt.Sprintf("%d to do", len(ts))
	}
	return ts, sum, nil
}

func (w *watcher) thing(id string, info threadInfo) protocol.Thing {
	subject := strings.TrimSpace(info.subject)
	if subject == "" {
		subject = "(no subject)"
	}
	// The sender leads, since the TV shows only titles and cuts long ones
	// off at the end: "Pat Landlord: Lease renewal".
	title := subject
	if info.from != "" {
		title = info.from + ": " + subject
	}
	t := protocol.Thing{
		ID:    "thread:" + id,
		Title: truncate(title, 300),
		Kind:  "email",
		URL:   fmt.Sprintf("https://mail.google.com/mail/u/%d/#inbox/%s", w.account, id),
		Tags:  []string{"gmail"},
	}
	if !info.last.IsZero() {
		at := info.last
		t.AttentionAt = &at
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
