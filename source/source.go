// Package source is the Go client for writing sotot sources: programs
// that watch something and publish the things needing attention to a
// sotot daemon.
//
// A Client holds the source's desired set of things. The integration
// updates that set with Set, Upsert, and Remove whenever it learns
// something, and the Client keeps sotot in sync: a full snapshot on each
// (re)connection and minimal deltas after that. Because the Client works
// from the whole desired set rather than a queue of events, a reconnect
// can't lose changes.
//
// Long-running sources call Run. Sources that run periodically (from
// cron, say) call PublishOnce, which returns after sotot acknowledges the
// snapshot.
package source

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/coder/websocket"
	"github.com/tailscale/tailcat"
)

// Client publishes things to a sotot daemon.
type Client struct {
	// Addr is the tailcat address of this source's sotot listener, as
	// printed by "sotot address". It's a secret: anyone with it can
	// publish as this source.
	Addr string

	// Logf, if non-nil, receives connection status logging.
	Logf func(format string, args ...any)

	// TailcatLogf, if non-nil, receives tailcat's verbose transport
	// logging, which is otherwise discarded.
	TailcatLogf func(format string, args ...any)

	// Dial, if non-nil, overrides how to reach sotot, instead of via
	// tailcat to Addr. It's for tests.
	Dial func(ctx context.Context) (net.Conn, error)

	// PingInterval and PongTimeout control how the client checks that
	// sotot is still there; zero means 30s and 60s.
	PingInterval, PongTimeout time.Duration

	mu       sync.Mutex
	things   map[string]protocol.Thing
	summary  string
	validFor time.Duration
	renew    bool          // whether to renew validity in the next message
	wake     chan struct{} // capacity 1; signaled on changes
	tc       *tailcat.Client
}

// NewClientFromEnv returns a client for the address in $SOTOT_ADDR, or in
// the file named by $SOTOT_ADDR_FILE. The address is a secret, so sources
// should take it from the environment rather than a command-line flag,
// which other users can see.
func NewClientFromEnv() (*Client, error) {
	addr := strings.TrimSpace(os.Getenv("SOTOT_ADDR"))
	if f := os.Getenv("SOTOT_ADDR_FILE"); addr == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading $SOTOT_ADDR_FILE: %w", err)
		}
		addr = strings.TrimSpace(string(b))
	}
	if addr == "" {
		return nil, errors.New("set $SOTOT_ADDR or $SOTOT_ADDR_FILE to this source's address (from: sotot address SOURCE-ID)")
	}
	return &Client{Addr: addr}, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (c *Client) initLocked() {
	if c.things == nil {
		c.things = map[string]protocol.Thing{}
		c.wake = make(chan struct{}, 1)
	}
}

func (c *Client) changedLocked() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// SetValidFor sets how long sotot should consider this source's data
// fresh after it disconnects, counted from when sotot receives it. It
// renews the validity of the current data on the next message.
//
// Choose it from the source's own schedule: a source that runs daily
// might use a little over a day. The default is zero: the data becomes
// stale as soon as the source disconnects.
func (c *Client) SetValidFor(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.validFor = d
	c.renew = true
	c.changedLocked()
}

// SetSummary sets a very short description of the source's state, such
// as "15 domains", which the dashboard shows instead of "Connected". The empty
// string clears it.
func (c *Client) SetSummary(s string) error {
	if err := protocol.ValidateSummary(s); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.summary = s
	c.changedLocked()
	return nil
}

// Refresh renews the validity of the current data, without changing it.
// Call it when the source has confirmed its data is still accurate.
func (c *Client) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.renew = true
	c.changedLocked()
}

// Set replaces the entire set of things.
func (c *Client) Set(things []protocol.Thing) error {
	m := make(map[string]protocol.Thing, len(things))
	for _, t := range things {
		if err := t.Validate(); err != nil {
			return err
		}
		if _, dup := m[t.ID]; dup {
			return fmt.Errorf("duplicate thing id %q", t.ID)
		}
		m[t.ID] = t
	}
	if len(m) > protocol.MaxThings {
		return fmt.Errorf("%d things; max is %d", len(m), protocol.MaxThings)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.things = m
	c.changedLocked()
	return nil
}

// Upsert adds or replaces things.
func (c *Client) Upsert(things ...protocol.Thing) error {
	for _, t := range things {
		if err := t.Validate(); err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	n := len(c.things)
	for _, t := range things {
		if _, ok := c.things[t.ID]; !ok {
			n++
		}
	}
	if n > protocol.MaxThings {
		return fmt.Errorf("%d things; max is %d", n, protocol.MaxThings)
	}
	for _, t := range things {
		c.things[t.ID] = t
	}
	c.changedLocked()
	return nil
}

// Remove removes things by ID. Removing an unknown ID is harmless.
func (c *Client) Remove(ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	for _, id := range ids {
		delete(c.things, id)
	}
	c.changedLocked()
}

// Run keeps sotot in sync with the client's things until ctx is done,
// reconnecting with exponential backoff as needed.
func (c *Client) Run(ctx context.Context) error {
	const minBackoff, maxBackoff = time.Second, time.Minute
	backoff := minBackoff
	for {
		err := c.session(ctx, func() { backoff = minBackoff }, false)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Start the next attempt with a fresh tailcat client. A client
		// introduces itself to the server only once, so after the daemon
		// restarts, an old client's dials go unanswered forever.
		c.resetTransport()
		// Full jitter, so many sources don't reconnect in lockstep.
		d := time.Duration(rand.Int64N(int64(backoff))) + backoff/2
		c.logf("source: %v; reconnecting in %v", err, d.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// PublishOnce connects, publishes the current things as a snapshot, waits
// for sotot to acknowledge it, and disconnects. Closing the socket alone
// isn't proof of publication; the ack is.
func (c *Client) PublishOnce(ctx context.Context) error {
	return c.session(ctx, nil, true)
}

// resetTransport discards the tailcat client, so the next dial makes a
// new one.
func (c *Client) resetTransport() {
	c.mu.Lock()
	tc := c.tc
	c.tc = nil
	c.mu.Unlock()
	if tc != nil {
		go tc.Close() // may block briefly; don't delay the reconnect
	}
}

// Close releases the tailcat transport. Call it after Run or PublishOnce
// returns.
func (c *Client) Close() error {
	c.mu.Lock()
	tc := c.tc
	c.tc = nil
	c.mu.Unlock()
	if tc == nil {
		return nil
	}
	return tc.Close()
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.Dial != nil {
		return c.Dial(ctx)
	}
	c.mu.Lock()
	if c.tc == nil {
		if c.Addr == "" {
			c.mu.Unlock()
			return nil, errors.New("source: no Addr set")
		}
		c.tc = tailcat.NewClient(tailcat.Addr(c.Addr))
		c.tc.Logf = c.TailcatLogf
		if c.tc.Logf == nil {
			c.tc.Logf = func(string, ...any) {}
		}
	}
	tc := c.tc
	c.mu.Unlock()
	return tc.DialTCPPort(ctx, protocol.TunnelPort)
}

// session runs one connection. established is called once sotot has
// acknowledged the initial snapshot. If once, it returns after that ack.
func (c *Client) session(ctx context.Context, established func(), once bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
	}}
	// The host name is synthetic: the transport dials the tailcat address
	// directly and never resolves it.
	conn, _, err := websocket.Dial(ctx, "ws://sotot.invalid"+protocol.Path, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer conn.CloseNow()

	// Read acks and errors. Reading also answers sotot's liveness pings.
	acks := make(chan protocol.Ack, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var env struct {
				Type string `json:"type"`
			}
			json.Unmarshal(b, &env)
			switch env.Type {
			case protocol.TypeAck:
				var a protocol.Ack
				if err := json.Unmarshal(b, &a); err != nil {
					readErr <- fmt.Errorf("bad ack: %w", err)
					return
				}
				select {
				case acks <- a:
				case <-ctx.Done():
					return
				}
			case protocol.TypeError:
				var e protocol.Error
				json.Unmarshal(b, &e)
				readErr <- fmt.Errorf("sotot rejected seq %d: %s", e.Seq, e.Message)
				return
			default:
				readErr <- fmt.Errorf("unexpected message type %q from sotot", env.Type)
				return
			}
		}
	}()

	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	var seq int64
	sent := map[string]protocol.Thing{} // what sotot has, as of our last send
	var sentSummary string
	sendSnapshot := func() error {
		c.mu.Lock()
		c.initLocked()
		m := &protocol.Snapshot{Type: protocol.TypeSnapshot, ValidForSeconds: protocol.Seconds(c.validFor), Things: []protocol.Thing{}, Summary: c.summary}
		sentSummary = c.summary
		sent = make(map[string]protocol.Thing, len(c.things))
		for id, t := range c.things {
			m.Things = append(m.Things, t)
			sent[id] = t
		}
		c.renew = false
		c.mu.Unlock()
		seq++
		m.Seq = seq
		return send(m)
	}

	if err := sendSnapshot(); err != nil {
		return err
	}
	snapshotTimeout := time.NewTimer(protocol.SnapshotDeadline)
	defer snapshotTimeout.Stop()
	select {
	case a := <-acks:
		if a.Seq != 1 {
			return fmt.Errorf("ack for seq %d; want 1", a.Seq)
		}
		if once {
			c.logf("source: published snapshot; fresh until %v", a.ExpiresAt.Local().Format(time.DateTime))
			return closeCleanly(conn, c)
		}
		c.logf("source: connected; snapshot accepted")
	case err := <-readErr:
		return err
	case <-snapshotTimeout.C:
		return errors.New("no ack for snapshot")
	case <-ctx.Done():
		return ctx.Err()
	}
	if established != nil {
		established()
	}

	// Check that sotot is still there. A daemon that restarts can vanish
	// without its close ever arriving, leaving reads waiting forever.
	go func() {
		t := time.NewTicker(cmp.Or(c.PingInterval, 30*time.Second))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, cmp.Or(c.PongTimeout, 60*time.Second))
			err := conn.Ping(pctx)
			pcancel()
			if err != nil && ctx.Err() == nil {
				select {
				case readErr <- fmt.Errorf("sotot stopped answering pings: %w", err):
				default:
				}
				return
			}
		}
	}()

	c.mu.Lock()
	c.initLocked()
	wake := c.wake
	c.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusGoingAway, "")
			return ctx.Err()
		case err := <-readErr:
			return err
		case a := <-acks:
			if a.Seq > seq {
				return fmt.Errorf("ack for unsent seq %d", a.Seq)
			}
			continue
		case <-wake:
		}

		c.mu.Lock()
		var upsert []protocol.Thing
		var remove []string
		for id, t := range c.things {
			if old, ok := sent[id]; !ok || !reflect.DeepEqual(old, t) {
				upsert = append(upsert, t)
			}
		}
		for id := range sent {
			if _, ok := c.things[id]; !ok {
				remove = append(remove, id)
			}
		}
		renew, validFor, summary := c.renew, c.validFor, c.summary
		c.renew = false
		c.mu.Unlock()

		var err error
		switch {
		case len(upsert)+len(remove) > protocol.MaxThings:
			err = sendSnapshot() // too big for one delta
		case len(upsert)+len(remove) > 0 || summary != sentSummary:
			seq++
			m := &protocol.Delta{Type: protocol.TypeDelta, Seq: seq, Upsert: upsert, Remove: remove}
			if renew {
				m.ValidForSeconds = protocol.Seconds(validFor)
			}
			if summary != sentSummary {
				m.Summary = &summary
				sentSummary = summary
			}
			for _, t := range upsert {
				sent[t.ID] = t
			}
			for _, id := range remove {
				delete(sent, id)
			}
			err = send(m)
		case renew:
			seq++
			err = send(&protocol.Refresh{Type: protocol.TypeRefresh, Seq: seq, ValidForSeconds: protocol.Seconds(validFor)})
		}
		if err != nil {
			return err
		}
	}
}

// closeCleanly finishes the WebSocket closing handshake and gives the
// tailcat transport a bounded chance to deliver the final packets, so a
// process exiting right after doesn't strand sotot's side.
func closeCleanly(conn *websocket.Conn, c *Client) error {
	conn.Close(websocket.StatusNormalClosure, "")
	c.mu.Lock()
	tc := c.tc
	c.mu.Unlock()
	if tc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tc.DrainTCP(ctx) // best effort
	}
	return nil
}
