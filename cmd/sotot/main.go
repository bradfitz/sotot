// Command sotot is the Stay On Top Of Things daemon: it collects things
// needing attention from sources and serves a read-only dashboard.
//
// Usage:
//
//	sotot [run] [flags]                 run the daemon
//	sotot new-identity FILE             create a source listener identity
//	sotot address [flags] SOURCE-ID     print a source's tailcat address
//
// Each source connects to its own tailcat listener, whose identity file is
// named in the config. The address printed by "sotot address" is a secret
// capability: give it only to that source.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/bradfitz/sotot/internal/dashboard"
	"github.com/bradfitz/sotot/internal/ingest"
	"github.com/bradfitz/sotot/internal/state"
	"github.com/bradfitz/sotot/web"
	"tailscale.com/tsnet"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = runCmd(args)
	case "new-identity":
		err = newIdentityCmd(args)
	case "address":
		err = addressCmd(args)
	case "help":
		fmt.Fprintln(os.Stderr, "usage: sotot [run|new-identity|address] [flags]")
		return
	default:
		err = fmt.Errorf("unknown command %q (want run, new-identity, or address)", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "sotot")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "sotot")
	}
	return "sotot-state"
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "sotot.json", "configuration file")
	stateDir := fs.String("state-dir", defaultStateDir(), "directory for cached source data and tsnet state")
	listen := fs.String("listen", "127.0.0.1:8080", "dashboard listen address")
	useTsnet := fs.Bool("tsnet", false, "serve the dashboard on a Tailscale tailnet via tsnet instead of a host TCP listener")
	internalListen := fs.String("internal-listen", "", "also serve the dashboard on this plain TCP address, e.g. :8080 for other programs in a cluster (sotot-cast); unlike --listen, works alongside --tsnet")
	tsnetHostname := fs.String("tsnet-hostname", "sotot", "tsnet mode: tailnet host name")
	tz := fs.String("tz", "", "time zone for displaying times (default local)")
	verboseTailcat := fs.Bool("verbose-tailcat", false, "log tailcat transport details")
	fs.Parse(args)

	loc := time.Local
	if *tz != "" {
		var err error
		if loc, err = time.LoadLocation(*tz); err != nil {
			return fmt.Errorf("--tz: %w", err)
		}
	}
	srcs, displays, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	release, err := state.LockDir(*stateDir)
	if err != nil {
		return err
	}
	defer release()

	var scfg []state.SourceConfig
	for _, s := range srcs {
		scfg = append(scfg, state.SourceConfig{ID: s.ID, Name: s.Name})
	}
	st, err := state.New(state.Config{Sources: scfg, Displays: displays, CacheDir: filepath.Join(*stateDir, "cache")})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("flushing cache: %v", err)
		}
	}()

	tailcatLogf := func(string, ...any) {}
	if *verboseTailcat {
		tailcatLogf = log.New(os.Stderr, "tailcat: ", log.LstdFlags|log.Lmsgprefix).Printf
	}
	listeners, err := startListeners(srcs, st, tailcatLogf)
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	if err != nil {
		return err
	}

	ds := &dashboard.Server{Store: st, Static: web.Static(), Location: loc}
	if err := ds.Init(); err != nil {
		return err
	}
	var ln net.Listener
	if *useTsnet {
		host, _, err := net.SplitHostPort(*listen)
		if err != nil {
			return fmt.Errorf("--listen: %w", err)
		}
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return errors.New("--tsnet can't listen on a loopback address; use e.g. --listen=:80")
		}
		ts := &tsnet.Server{
			Hostname: *tsnetHostname,
			Dir:      filepath.Join(*stateDir, "tsnet"),
			Logf:     func(string, ...any) {},
			UserLogf: log.Printf,
		}
		defer ts.Close()
		if ln, err = ts.Listen("tcp", *listen); err != nil {
			return err
		}
		log.Printf("dashboard on tailnet as %s, %s", *tsnetHostname, *listen)
	} else {
		if ln, err = net.Listen("tcp", *listen); err != nil {
			return err
		}
		log.Printf("dashboard at http://%s/", ln.Addr())
	}
	hs := &http.Server{Handler: ds, ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	if *internalListen != "" {
		iln, err := net.Listen("tcp", *internalListen)
		if err != nil {
			return fmt.Errorf("--internal-listen: %w", err)
		}
		log.Printf("dashboard also at http://%s/ (internal)", iln.Addr())
		go hs.Serve(iln)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Printf("shutting down")
	hs.Close()
	return nil
}

// startListeners starts a tailcat listener for every configured identity,
// in parallel since each takes a moment to reach its relay.
func startListeners(srcs []loadedSource, st *state.Store, tailcatLogf func(string, ...any)) ([]*ingest.Listener, error) {
	type result struct {
		l   *ingest.Listener
		err error
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ls []*ingest.Listener
	var errs []error
	for _, s := range srcs {
		for i, id := range s.identities {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := &ingest.Handler{Store: st, SourceID: s.ID}
				l, err := ingest.StartListener(id, h, tailcatLogf)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, fmt.Errorf("source %s, listener %s: %w", s.ID, filepath.Base(s.paths[i]), err))
					return
				}
				ls = append(ls, l)
				log.Printf("source %s: listening (%s)", s.ID, filepath.Base(s.paths[i]))
			}()
		}
	}
	wg.Wait()
	return ls, errors.Join(errs...)
}

func newIdentityCmd(args []string) error {
	fs := flag.NewFlagSet("new-identity", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sotot new-identity FILE\n\nCreates a new source listener identity, pinned to the nearest DERP relay.")
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite (that would revoke its address)", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := ingest.NewIdentity(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := ingest.WriteIdentity(path, id); err != nil {
		return err
	}
	r := id.Key.Public.Region[0]
	fmt.Printf("wrote %s (relay region %d, %s)\n", path, r.RegionID, r.RegionName)
	return nil
}

func addressCmd(args []string) error {
	fs := flag.NewFlagSet("address", flag.ExitOnError)
	configPath := fs.String("config", "sotot.json", "configuration file")
	listener := fs.Int("listener", 0, "which of the source's listeners, counting from 0")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sotot address [--config=FILE] [--listener=N] SOURCE-ID\n\nPrints the secret tailcat address a source uses to connect.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	srcs, _, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		if s.ID != fs.Arg(0) {
			continue
		}
		if *listener < 0 || *listener >= len(s.identities) {
			return fmt.Errorf("source %s has %d listener(s)", s.ID, len(s.identities))
		}
		fmt.Println(s.identities[*listener].Addr())
		return nil
	}
	return fmt.Errorf("no source %q in %s", fs.Arg(0), *configPath)
}
