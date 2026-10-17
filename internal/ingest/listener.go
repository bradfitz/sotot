package ingest

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bradfitz/sotot/protocol"
	"github.com/tailscale/tailcat"
)

// Listener is a running tailcat listener for one source.
type Listener struct {
	SourceID string
	Identity *Identity

	srv     *tailcat.Server
	conns   *connListener
	hs      *http.Server
	handler *Handler
}

// StartListener starts a tailcat server with identity id, serving h on
// protocol.TunnelPort inside the tunnel. tailcatLogf receives tailcat's
// own (verbose) logging.
func StartListener(id *Identity, h *Handler, tailcatLogf func(string, ...any)) (*Listener, error) {
	cl := newConnListener(64)
	srv := &tailcat.Server{
		Key:          id.Key.Private,
		PresharedKey: id.Key.Public.PresharedKey,
		Region:       id.Key.Public.Region[0],
		Logf:         tailcatLogf,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != protocol.TunnelPort {
				return nil // RST
			}
			return cl.handle
		},
	}
	if err := srv.Start(); err != nil {
		return nil, err
	}
	l := &Listener{
		SourceID: h.SourceID,
		Identity: id,
		srv:      srv,
		conns:    cl,
		handler:  h,
		hs: &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
	go l.hs.Serve(cl)
	return l, nil
}

// Addr returns the listener's tailcat address. It's a secret.
func (l *Listener) Addr() tailcat.Addr { return l.srv.TailcatAddr() }

// Close stops accepting connections and closes active sessions.
func (l *Listener) Close() error {
	l.handler.CloseSessions()
	l.hs.Close() // closes the connListener and non-upgraded conns
	l.conns.Close()
	return l.srv.Close()
}

// connListener adapts tailcat's per-connection OnTCP callbacks to a
// net.Listener for http.Server. Accepted connections are queued, up to a
// bound; beyond it they're closed.
type connListener struct {
	ch        chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newConnListener(queue int) *connListener {
	return &connListener{ch: make(chan net.Conn, queue), done: make(chan struct{})}
}

var errClosed = errors.New("listener closed")

// handle is the tailcat OnTCP handler. It blocks until the connection
// closes, in case tailcat cleans up the connection when it returns.
func (l *connListener) handle(c net.Conn) {
	nc := &notifyConn{Conn: c, closed: make(chan struct{})}
	select {
	case l.ch <- nc:
	case <-l.done:
		c.Close()
		return
	default:
		c.Close() // queue full
		return
	}
	// On shutdown, http.Server closes the connections it tracks,
	// Handler.CloseSessions closes upgraded WebSockets, and Close closes
	// any never accepted.
	<-nc.closed
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, errClosed
	}
}

func (l *connListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		// Close connections queued but never accepted.
		for {
			select {
			case c := <-l.ch:
				c.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *connListener) Addr() net.Addr { return tailcatAddr{} }

type tailcatAddr struct{}

func (tailcatAddr) Network() string { return "tailcat" }
func (tailcatAddr) String() string  { return "tailcat" }

// notifyConn is a net.Conn that reports when it's closed.
type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	return err
}
