package cast

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// DefaultMediaReceiver is the app ID of Google's stock receiver, which can
// display images and play audio/video URLs without a registered custom app.
const DefaultMediaReceiver = "CC1AD845"

const (
	nsConnection = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat  = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver   = "urn:x-cast:com.google.cast.receiver"
	nsMedia      = "urn:x-cast:com.google.cast.media"

	senderID   = "sender-0"
	receiverID = "receiver-0"

	maxMessageSize = 1 << 20
	pingInterval   = 5 * time.Second
	readTimeout    = 30 * time.Second
	requestTimeout = 15 * time.Second
)

// ErrSessionEnded is returned by Load after the launched receiver app has
// stopped, such as when another sender casts something else.
var ErrSessionEnded = errors.New("cast: receiver session ended")

// Conn is a connection to a Cast device's control channel.
type Conn struct {
	// Logf, if non-nil, receives unsolicited media status messages,
	// such as player state changes. Set it before calling Launch.
	Logf func(format string, args ...any)
	// LogRaw makes Logf receive the raw JSON of unsolicited media
	// messages instead of a summary.
	LogRaw bool
	// OnMediaStatus, if non-nil, is called from the read loop with each
	// media status received, solicited or not. It must not block.
	OnMediaStatus func(MediaStatus)

	tc   *tls.Conn
	done chan struct{} // closed when the read loop exits

	wmu sync.Mutex // serializes writes to tc

	mu      sync.Mutex
	err     error // why done was closed
	nextID  int
	waiters map[int]chan json.RawMessage
	session *Session // current launched app, or nil
}

// Session is a running receiver application.
type Session struct {
	AppID       string
	SessionID   string
	TransportID string
	ended       chan struct{}
}

// Ended returns a channel that's closed when the session stops.
func (s *Session) Ended() <-chan struct{} { return s.ended }

// Dial connects to the Cast device at addr (host:port, normally port 8009).
func Dial(ctx context.Context, addr string) (*Conn, error) {
	d := &tls.Dialer{Config: &tls.Config{
		// Cast devices present certificates chained to Google's device
		// CA, not to anything in the system roots. Device authentication
		// is a separate challenge we don't need for display.
		InsecureSkipVerify: true,
	}}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Conn{
		tc:      nc.(*tls.Conn),
		done:    make(chan struct{}),
		waiters: map[int]chan json.RawMessage{},
	}
	if err := c.send(receiverID, nsConnection, map[string]any{"type": "CONNECT"}); err != nil {
		nc.Close()
		return nil, err
	}
	go c.readLoop()
	go c.pingLoop()
	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.closeWithError(errors.New("cast: connection closed"))
	return nil
}

// Done returns a channel that's closed when the connection fails or is closed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended, once Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) closeWithError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	c.tc.Close()
	close(c.done)
	c.endSessionLocked()
}

func (c *Conn) endSessionLocked() {
	if c.session != nil {
		close(c.session.ended)
		c.session = nil
	}
}

func (c *Conn) pingLoop() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if err := c.send(receiverID, nsHeartbeat, map[string]any{"type": "PING"}); err != nil {
				c.closeWithError(err)
				return
			}
		}
	}
}

type envelope struct {
	Type      string `json:"type"`
	RequestID int    `json:"requestId"`
}

type receiverStatus struct {
	Status struct {
		Applications []struct {
			AppID       string `json:"appId"`
			SessionID   string `json:"sessionId"`
			TransportID string `json:"transportId"`
		} `json:"applications"`
	} `json:"status"`
}

func (c *Conn) readLoop() {
	for {
		c.tc.SetReadDeadline(time.Now().Add(readTimeout))
		m, err := readMessage(c.tc)
		if err != nil {
			c.closeWithError(fmt.Errorf("cast: read: %w", err))
			return
		}
		var env envelope
		if err := json.Unmarshal([]byte(m.payload), &env); err != nil {
			continue // binary or unexpected payload; ignore
		}
		switch {
		case m.namespace == nsHeartbeat && env.Type == "PING":
			go c.send(m.sourceID, nsHeartbeat, map[string]any{"type": "PONG"})
			continue
		case m.namespace == nsConnection && env.Type == "CLOSE":
			c.mu.Lock()
			if m.sourceID == receiverID {
				c.mu.Unlock()
				c.closeWithError(errors.New("cast: device closed connection"))
				return
			}
			if c.session != nil && m.sourceID == c.session.TransportID {
				c.endSessionLocked()
			}
			c.mu.Unlock()
			continue
		case m.namespace == nsReceiver && env.Type == "RECEIVER_STATUS":
			c.checkSession([]byte(m.payload))
		}
		if m.namespace == nsMedia && env.Type == "MEDIA_STATUS" && c.OnMediaStatus != nil {
			for _, st := range parseMediaStatus([]byte(m.payload)) {
				c.OnMediaStatus(st)
			}
		}
		if env.RequestID == 0 && m.namespace == nsMedia && c.Logf != nil {
			if c.LogRaw {
				c.Logf("cast: media: %s", m.payload)
			} else {
				c.Logf("cast: media status: %s", summarizeMediaStatus([]byte(m.payload)))
			}
		}
		if env.RequestID != 0 {
			c.mu.Lock()
			ch := c.waiters[env.RequestID]
			delete(c.waiters, env.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- json.RawMessage(m.payload)
			}
		}
	}
}

// MediaStatus is the state of a media session in the receiver.
type MediaStatus struct {
	MediaSessionID int     `json:"mediaSessionId"`
	PlayerState    string  `json:"playerState"` // IDLE, BUFFERING, PLAYING, PAUSED
	IdleReason     string  `json:"idleReason"`  // when IDLE: FINISHED, ERROR, ...
	CurrentTime    float64 `json:"currentTime"` // playback position, seconds
	PlaybackRate   float64 `json:"playbackRate"`
}

func parseMediaStatus(payload []byte) []MediaStatus {
	var ms struct {
		Status []MediaStatus `json:"status"`
	}
	json.Unmarshal(payload, &ms)
	return ms.Status
}

// summarizeMediaStatus returns the interesting parts of a MEDIA_STATUS
// message, or the raw message if it's something else.
func summarizeMediaStatus(payload []byte) string {
	var env envelope
	if json.Unmarshal(payload, &env) != nil || env.Type != "MEDIA_STATUS" {
		return string(payload)
	}
	var parts []string
	for _, st := range parseMediaStatus(payload) {
		p := fmt.Sprintf("%s t=%.1fs", st.PlayerState, st.CurrentTime)
		if st.IdleReason != "" {
			p += " idleReason=" + st.IdleReason
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "no media"
	}
	return strings.Join(parts, "; ")
}

// checkSession ends the current session if a receiver status shows that
// its app is no longer running.
func (c *Conn) checkSession(payload []byte) {
	var rs receiverStatus
	if json.Unmarshal(payload, &rs) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return
	}
	for _, a := range rs.Status.Applications {
		if a.SessionID == c.session.SessionID {
			return
		}
	}
	c.endSessionLocked()
}

// request sends a message with a new requestId and waits for the reply
// carrying the same ID.
func (c *Conn) request(ctx context.Context, dst, ns string, msg map[string]any) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.waiters[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	msg["requestId"] = id
	if err := c.send(dst, ns, msg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	select {
	case r := <-ch:
		return r, nil
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		return nil, fmt.Errorf("cast: %s %v: %w", ns, msg["type"], ctx.Err())
	}
}

// Launch starts appID on the device, or joins it if already running, and
// opens a virtual connection to it.
func (c *Conn) Launch(ctx context.Context, appID string) (*Session, error) {
	r, err := c.request(ctx, receiverID, nsReceiver, map[string]any{"type": "LAUNCH", "appId": appID})
	if err != nil {
		return nil, err
	}
	var env envelope
	json.Unmarshal(r, &env)
	if env.Type != "RECEIVER_STATUS" {
		return nil, fmt.Errorf("cast: launch %s: %s", appID, r)
	}
	var rs receiverStatus
	if err := json.Unmarshal(r, &rs); err != nil {
		return nil, err
	}
	for _, a := range rs.Status.Applications {
		if a.AppID != appID {
			continue
		}
		s := &Session{
			AppID:       a.AppID,
			SessionID:   a.SessionID,
			TransportID: a.TransportID,
			ended:       make(chan struct{}),
		}
		if err := c.send(s.TransportID, nsConnection, map[string]any{"type": "CONNECT"}); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.endSessionLocked()
		c.session = s
		c.mu.Unlock()
		return s, nil
	}
	return nil, fmt.Errorf("cast: launch %s: app not in receiver status: %s", appID, r)
}

// Stop stops the session's app, returning the device to its idle screen.
func (c *Conn) Stop(ctx context.Context, s *Session) error {
	_, err := c.request(ctx, receiverID, nsReceiver, map[string]any{"type": "STOP", "sessionId": s.SessionID})
	return err
}

// Media describes a URL for the receiver to load.
type Media struct {
	URL         string
	ContentType string // e.g. "image/png"
	StreamType  string // "NONE" (default), "BUFFERED", or "LIVE"
	Title       string

	// HLSSegmentFormat and HLSVideoSegmentFormat describe HLS segments,
	// such as "ts" and "mpeg2_ts". Without them the receiver assumes
	// segments are packed audio and plays video streams as a black screen.
	HLSSegmentFormat      string
	HLSVideoSegmentFormat string
}

// Load tells the session's media receiver to fetch and show m. It returns
// the media session ID the receiver assigned, which identifies status
// updates about m as opposed to earlier media.
func (c *Conn) Load(ctx context.Context, s *Session, m Media) (mediaSessionID int, err error) {
	select {
	case <-s.ended:
		return 0, ErrSessionEnded
	default:
	}
	streamType := cmp.Or(m.StreamType, "NONE")
	metadataType := 0 // GENERIC
	if strings.HasPrefix(m.ContentType, "image/") {
		metadataType = 4 // PHOTO
	}
	media := map[string]any{
		"contentId":   m.URL,
		"contentType": m.ContentType,
		"streamType":  streamType,
		"metadata": map[string]any{
			"metadataType": metadataType,
			"title":        m.Title,
		},
	}
	if m.HLSSegmentFormat != "" {
		media["hlsSegmentFormat"] = m.HLSSegmentFormat
	}
	if m.HLSVideoSegmentFormat != "" {
		media["hlsVideoSegmentFormat"] = m.HLSVideoSegmentFormat
	}
	r, err := c.request(ctx, s.TransportID, nsMedia, map[string]any{
		"type":      "LOAD",
		"sessionId": s.SessionID,
		"autoplay":  true,
		"media":     media,
	})
	if err != nil {
		return 0, err
	}
	var env envelope
	json.Unmarshal(r, &env)
	if env.Type != "MEDIA_STATUS" {
		return 0, fmt.Errorf("cast: load %s: %s", m.URL, r)
	}
	for _, st := range parseMediaStatus(r) {
		return st.MediaSessionID, nil
	}
	return 0, fmt.Errorf("cast: load %s: no media status in reply: %s", m.URL, r)
}

// SetPlaybackRate sets the playback speed of the given media session,
// where 1 is normal speed.
func (c *Conn) SetPlaybackRate(ctx context.Context, s *Session, mediaSessionID int, rate float64) error {
	r, err := c.request(ctx, s.TransportID, nsMedia, map[string]any{
		"type":           "SET_PLAYBACK_RATE",
		"mediaSessionId": mediaSessionID,
		"playbackRate":   rate,
	})
	if err != nil {
		return err
	}
	var env envelope
	json.Unmarshal(r, &env)
	if env.Type != "MEDIA_STATUS" {
		return fmt.Errorf("cast: set playback rate: %s", r)
	}
	return nil
}

// RequestMediaStatus asks the receiver for the media status. The reply is
// delivered to OnMediaStatus.
func (c *Conn) RequestMediaStatus(ctx context.Context, s *Session) error {
	_, err := c.request(ctx, s.TransportID, nsMedia, map[string]any{"type": "GET_STATUS"})
	return err
}

func (c *Conn) send(dst, ns string, msg map[string]any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b := encodeMessage(senderID, dst, ns, string(payload))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.tc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.tc.Write(b)
	return err
}

// message is the subset of the CastMessage protobuf that we use:
//
//	message CastMessage {
//	  required ProtocolVersion protocol_version = 1; // CASTV2_1_0 = 0
//	  required string source_id = 2;
//	  required string destination_id = 3;
//	  required string namespace = 4;
//	  required PayloadType payload_type = 5; // STRING = 0, BINARY = 1
//	  optional string payload_utf8 = 6;
//	  optional bytes payload_binary = 7;
//	}
//
// On the wire, each message is preceded by its 4-byte big-endian length.
type message struct {
	sourceID, destID, namespace, payload string
}

func encodeMessage(src, dst, ns, payload string) []byte {
	b := make([]byte, 4, 4+len(payload)+len(ns)+64)
	b = appendVarintField(b, 1, 0)
	b = appendStringField(b, 2, src)
	b = appendStringField(b, 3, dst)
	b = appendStringField(b, 4, ns)
	b = appendVarintField(b, 5, 0)
	b = appendStringField(b, 6, payload)
	binary.BigEndian.PutUint32(b, uint32(len(b)-4))
	return b
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|0)
	return binary.AppendUvarint(b, v)
}

func appendStringField(b []byte, field int, s string) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func readMessage(r io.Reader) (message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return message{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxMessageSize {
		return message{}, fmt.Errorf("message too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return message{}, err
	}
	return decodeMessage(b)
}

func decodeMessage(b []byte) (message, error) {
	var m message
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return m, errors.New("bad protobuf key")
		}
		b = b[n:]
		field, wireType := key>>3, key&7
		switch wireType {
		case 0:
			_, n := binary.Uvarint(b)
			if n <= 0 {
				return m, errors.New("bad protobuf varint")
			}
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return m, errors.New("bad protobuf length")
			}
			v := string(b[n : n+int(l)])
			b = b[n+int(l):]
			switch field {
			case 2:
				m.sourceID = v
			case 3:
				m.destID = v
			case 4:
				m.namespace = v
			case 6:
				m.payload = v
			}
		default:
			return m, fmt.Errorf("unsupported protobuf wire type %d", wireType)
		}
	}
	return m, nil
}

// LocalAddrFor returns the local IP address the host would use to reach
// the device at addr. The device must be able to reach us at that address
// to fetch media.
func LocalAddrFor(addr string) (net.IP, error) {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}
