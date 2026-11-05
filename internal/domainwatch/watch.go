package domainwatch

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/bradfitz/sotot/source"
)

// Main runs a domain-watching sotot source for the registrar returned by
// newRegistrar, which is called after flags are parsed (so it can read
// credentials). It parses the common flags itself and exits on error.
//
// By default it runs forever, publishing to the sotot source address in
// $SOTOT_ADDR or the file named by $SOTOT_ADDR_FILE. With --dry-run it
// checks everything once, prints what it would publish, and exits.
func Main(newRegistrar func() (Registrar, error)) {
	var (
		ignore         = flag.String("ignore", "", "comma-separated domains not to watch")
		registrarEvery = flag.Duration("registrar-interval", 6*time.Hour, "how often to list the account's domains")
		lookupSpacing  = flag.Duration("lookup-spacing", 3*time.Second, "minimum time between registry lookups")
		horizonDays    = flag.Int("horizon-days", 90, "warn this many days ahead about domains that won't auto-renew")
		autoRenewDays  = flag.Int("autorenew-horizon-days", 14, "mention automatic renewals this many days ahead")
		dryRun         = flag.Bool("dry-run", false, "check everything once, print the things that would be published as JSON, and exit")
	)
	flag.Parse()
	reg, err := newRegistrar()
	if err != nil {
		log.Fatal(err)
	}
	w := &watcher{
		reg:           reg,
		ignore:        map[string]bool{},
		policy:        DefaultPolicy,
		rc:            &registryClient{},
		domains:       map[string]*domain{},
		next:          map[string]time.Time{},
		firstSeen:     map[string]time.Time{},
		lookupSpacing: *lookupSpacing,
	}
	w.policy.Horizon = time.Duration(*horizonDays) * 24 * time.Hour
	w.policy.AutoRenewHorizon = time.Duration(*autoRenewDays) * 24 * time.Hour
	for _, d := range strings.Split(*ignore, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			w.ignore[d] = true
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *dryRun {
		w.dryRun(ctx)
		return
	}

	c, err := source.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	// Data stays fresh for a while if this process dies; each successful
	// list renews it.
	c.SetValidFor(2*(*registrarEvery) + time.Hour)
	if err := c.Set([]protocol.Thing{}); err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := c.Run(ctx); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	}()
	w.publish = func(ts []protocol.Thing, summary string) {
		if err := c.SetSummary(summary); err != nil {
			log.Printf("publishing summary: %v", err)
		}
		if err := c.Set(ts); err != nil {
			log.Printf("publishing: %v", err)
		}
	}
	w.refresh = c.Refresh
	w.run(ctx, *registrarEvery)
}

// Secret returns the value of environment variable name, or the contents
// of the file named by name+"_FILE", trimmed; or "" if neither is set.
func Secret(name string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, nil
	}
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("reading $%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// watcher tracks the registrar's domains. It's used from one goroutine.
type watcher struct {
	reg           Registrar
	ignore        map[string]bool
	policy        Policy
	rc            *registryClient
	lookupSpacing time.Duration
	publish       func(things []protocol.Thing, summary string)
	refresh       func()

	domains      map[string]*domain   // by name
	next         map[string]time.Time // when to next look up each domain
	listErr      error                // why the latest list failed, if it did
	listErrSince time.Time
	firstSeen    map[string]time.Time // thing ID -> when first published
}

func (w *watcher) sortedNames() []string {
	var names []string
	for n := range w.domains {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// summary describes the domains' state in a few words: "15 domains" if
// none needs attention, else "15 domains, 1 to check".
func (w *watcher) summary(now time.Time) string {
	check := 0
	for _, d := range w.domains {
		if len(w.policy.domainThings(w.reg.Name(), d, now)) > 0 {
			check++
		}
	}
	s := fmt.Sprintf("%d domains", len(w.domains))
	if len(w.domains) == 1 {
		s = "1 domain"
	}
	if check > 0 {
		s += fmt.Sprintf(", %d to check", check)
	}
	return s
}

func (w *watcher) things(now time.Time) []protocol.Thing {
	var ds []*domain
	for _, n := range w.sortedNames() {
		ds = append(ds, w.domains[n])
	}
	ts := w.policy.things(w.reg.Name(), ds, w.listErr, w.listErrSince, now)
	// Stamp untimed things with when they first appeared, so they sort
	// sensibly and don't change on every pass.
	seen := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		seen[t.ID] = true
		if _, ok := w.firstSeen[t.ID]; !ok {
			w.firstSeen[t.ID] = now
		}
		if t.AttentionAt == nil && t.DueAt == nil {
			t.AttentionAt = timeP(w.firstSeen[t.ID])
		}
	}
	for id := range w.firstSeen {
		if !seen[id] {
			delete(w.firstSeen, id)
		}
	}
	return ts
}

// list refreshes the domain list, reporting whether it succeeded. On
// failure, the last known list stays.
func (w *watcher) list(ctx context.Context) bool {
	regs, err := w.reg.List(ctx)
	if err != nil {
		log.Printf("%s: %v", w.reg.Name(), err)
		if w.listErr == nil {
			w.listErrSince = time.Now()
		}
		w.listErr = err
		return false
	}
	w.listErr = nil
	log.Printf("%s: %d domains", w.reg.Name(), len(regs))
	seen := map[string]bool{}
	for _, r := range regs {
		if w.ignore[r.Domain] {
			continue
		}
		seen[r.Domain] = true
		if d := w.domains[r.Domain]; d != nil {
			d.Reg = r
		} else {
			w.domains[r.Domain] = &domain{Reg: r}
		}
	}
	for name := range w.domains {
		if !seen[name] {
			delete(w.domains, name) // transferred away or let go
			delete(w.next, name)
		}
	}
	return true
}

func (w *watcher) lookup(ctx context.Context, name string) {
	d := w.domains[name]
	if d == nil {
		return
	}
	info, err := w.rc.Lookup(ctx, name)
	now := time.Now()
	if err != nil {
		if d.RegistryErr == nil {
			d.RegistryErrSince = now
		}
		d.RegistryErr = err
		log.Printf("%s: registry lookup: %v", name, err)
	} else {
		d.Registry, d.RegistryErr = &info, nil
	}
	w.next[name] = nextLookup(d, now)
}

func (w *watcher) dryRun(ctx context.Context) {
	if !w.list(ctx) {
		log.Printf("(listing failed; showing the error as a thing)")
	}
	for i, name := range w.sortedNames() {
		if i > 0 {
			time.Sleep(w.lookupSpacing)
		}
		w.lookup(ctx, name)
	}
	for _, name := range w.sortedNames() {
		d := w.domains[name]
		exp, from := d.expires(w.reg.Name())
		ar := "off"
		if d.Reg.AutoRenew {
			ar = "on"
		}
		log.Printf("%-32s expires %s (per %s), auto-renew %s", name, exp.Format(time.DateOnly), from, ar)
	}
	log.Printf("summary: %s", w.summary(time.Now()))
	ts := w.things(time.Now())
	if ts == nil {
		ts = []protocol.Thing{} // print [], not null
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(ts)
}

func (w *watcher) run(ctx context.Context, listEvery time.Duration) {
	var lastPublished []protocol.Thing
	var lastSummary string
	published := false
	publishIfChanged := func() {
		now := time.Now()
		ts, sum := w.things(now), w.summary(now)
		if !published || sum != lastSummary || !thingsEqual(ts, lastPublished) {
			w.publish(ts, sum)
			lastPublished, lastSummary, published = ts, sum, true
			log.Printf("published %d things (%s)", len(ts), sum)
		}
	}

	if w.list(ctx) {
		w.refresh()
	}
	publishIfChanged()
	listTicker := time.NewTicker(listEvery)
	defer listTicker.Stop()
	tick := time.NewTicker(w.lookupSpacing)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-listTicker.C:
			if w.list(ctx) {
				w.refresh()
			}
		case <-tick.C:
			// One due lookup per tick, earliest first, to stay polite.
			var due string
			now := time.Now()
			for _, n := range w.sortedNames() {
				if t := w.next[n]; !t.After(now) && (due == "" || t.Before(w.next[due])) {
					due = n
				}
			}
			if due != "" {
				w.lookup(ctx, due)
			}
		}
		publishIfChanged()
	}
}

func thingsEqual(a, b []protocol.Thing) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
