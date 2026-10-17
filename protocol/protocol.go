// Package protocol defines the sotot source protocol, version 1: the JSON
// messages a source sends to the sotot daemon over a WebSocket, and the
// daemon's replies.
//
// A source is a separate program that watches something (a GitHub
// account, a bills spreadsheet, a chat service) and publishes the things
// that need its user's attention. See docs/source-protocol.md for the
// full contract; this package is its Go form.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
	_ "time/tzdata" // so sources and the daemon agree on valid time zones
	"unicode/utf8"
)

// Path is the HTTP path of the source WebSocket endpoint, version 1.
const Path = "/sotot/v1/source"

// TunnelPort is the TCP port, inside the tailcat tunnel, on which the
// daemon serves Path.
const TunnelPort = 80

// Limits on messages and things. A message violating them is rejected.
const (
	MaxMessageBytes  = 8 << 20
	MaxThings        = 10000 // per source
	MaxIDBytes       = 256
	MaxTitleBytes    = 1024
	MaxBodyBytes     = 16 << 10
	MaxURLBytes      = 4096
	MaxKindBytes     = 64
	MaxTags          = 32
	MaxTagBytes      = 64
	MaxValidSeconds  = 10 * 366 * 24 * 60 * 60 // ten years
	MaxErrorBytes    = 1024
	SnapshotDeadline = 30 * time.Second // to send the first snapshot after connecting
)

// Thing is something that needs attention: a PR to review, a bill to pay,
// a message to answer. Its identity is the pair (source ID, ID).
type Thing struct {
	// ID is unique within the source and must stay the same across
	// reconnects and source restarts. It is opaque to sotot.
	ID string `json:"id"`

	// Title is a short human-readable description. Required.
	Title string `json:"title"`

	// Body is optional plain-text context. It is never rendered as HTML.
	Body string `json:"body,omitempty"`

	// URL is an optional http or https link to the thing itself or the
	// page for acting on it.
	URL string `json:"url,omitempty"`

	// Kind is an optional descriptive category such as "pull_request",
	// "meeting", "bill", or "message".
	Kind string `json:"kind,omitempty"`

	// AttentionAt is when the source thinks attention should begin.
	AttentionAt *time.Time `json:"attention_at,omitempty"`

	// StartsAt and EndsAt bound a timed event.
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`

	// DueAt is a deadline instant. For a deadline that's only a date,
	// use Date instead.
	DueAt *time.Time `json:"due_at,omitempty"`

	// Date, in YYYY-MM-DD form, is the day of a date-only obligation or
	// all-day event. It's not an instant: it's interpreted in TimeZone,
	// which is required with Date.
	Date     string `json:"date,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`

	// Tags are optional descriptive labels with no built-in meaning.
	Tags []string `json:"tags,omitempty"`
}

// Validate reports whether t is a valid thing.
func (t *Thing) Validate() error {
	if err := t.validate(); err != nil {
		if t.ID != "" && len(t.ID) <= MaxIDBytes {
			return fmt.Errorf("thing %q: %w", t.ID, err)
		}
		return fmt.Errorf("thing: %w", err)
	}
	return nil
}

func (t *Thing) validate() error {
	if t.ID == "" {
		return errors.New("missing id")
	}
	if len(t.ID) > MaxIDBytes {
		return fmt.Errorf("id longer than %d bytes", MaxIDBytes)
	}
	if t.Title == "" {
		return errors.New("missing title")
	}
	for _, f := range []struct {
		name, val string
		max       int
	}{
		{"id", t.ID, MaxIDBytes},
		{"title", t.Title, MaxTitleBytes},
		{"body", t.Body, MaxBodyBytes},
		{"url", t.URL, MaxURLBytes},
		{"kind", t.Kind, MaxKindBytes},
	} {
		if len(f.val) > f.max {
			return fmt.Errorf("%s longer than %d bytes", f.name, f.max)
		}
		if !utf8.ValidString(f.val) {
			return fmt.Errorf("%s is not valid UTF-8", f.name)
		}
	}
	if t.URL != "" {
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("url %q is not an absolute http or https URL", t.URL)
		}
	}
	if len(t.Tags) > MaxTags {
		return fmt.Errorf("more than %d tags", MaxTags)
	}
	for _, tag := range t.Tags {
		if tag == "" || len(tag) > MaxTagBytes || !utf8.ValidString(tag) {
			return fmt.Errorf("invalid tag %q", tag)
		}
	}
	if t.EndsAt != nil && t.StartsAt == nil {
		return errors.New("ends_at without starts_at")
	}
	if t.EndsAt != nil && t.EndsAt.Before(*t.StartsAt) {
		return errors.New("ends_at before starts_at")
	}
	if t.Date != "" {
		if _, err := time.Parse(time.DateOnly, t.Date); err != nil {
			return fmt.Errorf("date %q is not YYYY-MM-DD", t.Date)
		}
		if t.TimeZone == "" {
			return errors.New("date without time_zone")
		}
		// A date and an instant would be two representations of the
		// same temporal fact.
		if t.DueAt != nil || t.StartsAt != nil {
			return errors.New("date is exclusive with due_at and starts_at/ends_at")
		}
	}
	if t.TimeZone != "" {
		if t.Date == "" {
			return errors.New("time_zone without date")
		}
		if _, err := time.LoadLocation(t.TimeZone); err != nil || t.TimeZone == "Local" {
			return fmt.Errorf("unknown time_zone %q", t.TimeZone)
		}
	}
	return nil
}

// Message types.
const (
	// From sources:
	TypeSnapshot = "snapshot" // a source's complete set of things
	TypeDelta    = "delta"    // changes to the set
	TypeRefresh  = "refresh"  // renews validity without changing things

	// From the daemon:
	TypeAck   = "ack"   // a data message was accepted
	TypeError = "error" // a message was rejected; the daemon then closes
)

// Snapshot replaces the source's entire set of things. It must be the
// first message on each connection.
type Snapshot struct {
	Type string `json:"type"` // "snapshot"
	Seq  int64  `json:"seq"`
	// ValidForSeconds is how long the data stays fresh after the source
	// disconnects, counted from when the daemon accepts the message.
	// Required; zero means stale as soon as the source disconnects.
	ValidForSeconds *int64  `json:"valid_for_seconds"`
	Things          []Thing `json:"things"`
}

// Delta atomically upserts and removes things. An upsert replaces the
// whole thing; it's not a field patch.
type Delta struct {
	Type   string   `json:"type"` // "delta"
	Seq    int64    `json:"seq"`
	Upsert []Thing  `json:"upsert,omitempty"`
	Remove []string `json:"remove,omitempty"`
	// ValidForSeconds, if set, renews the validity of the whole
	// resulting set. If absent, the expiry is unchanged.
	ValidForSeconds *int64 `json:"valid_for_seconds,omitempty"`
}

// Refresh renews the validity of the current set without changing it.
type Refresh struct {
	Type            string `json:"type"` // "refresh"
	Seq             int64  `json:"seq"`
	ValidForSeconds *int64 `json:"valid_for_seconds"`
}

// Ack acknowledges that the daemon accepted and applied the data message
// with the same Seq. It means accepted, not durably written to disk.
type Ack struct {
	Type       string    `json:"type"` // "ack"
	Seq        int64     `json:"seq"`
	ReceivedAt time.Time `json:"received_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Error reports why the daemon rejected a message. The daemon closes the
// connection after sending it; the source should reconnect and send a
// fresh snapshot.
type Error struct {
	Type    string `json:"type"` // "error"
	Seq     int64  `json:"seq,omitempty"`
	Message string `json:"message"`
}

// ParseSourceMessage decodes and validates a message from a source,
// returning a *Snapshot, *Delta, or *Refresh. It checks each message on
// its own; sequencing and limits on the resulting set are the daemon's
// job.
func ParseSourceMessage(b []byte) (any, error) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	switch env.Type {
	case TypeSnapshot:
		var m Snapshot
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("invalid snapshot: %w", err)
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeDelta:
		var m Delta
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("invalid delta: %w", err)
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case TypeRefresh:
		var m Refresh
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("invalid refresh: %w", err)
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case "":
		return nil, errors.New("message has no type")
	default:
		return nil, fmt.Errorf("unknown message type %q", env.Type)
	}
}

func validateSeq(seq int64) error {
	if seq < 1 {
		return fmt.Errorf("invalid seq %d", seq)
	}
	return nil
}

func validateValidFor(v *int64, required bool) error {
	if v == nil {
		if required {
			return errors.New("missing valid_for_seconds")
		}
		return nil
	}
	if *v < 0 || *v > MaxValidSeconds {
		return fmt.Errorf("valid_for_seconds %d out of range [0, %d]", *v, MaxValidSeconds)
	}
	return nil
}

// Validate checks the snapshot on its own.
func (m *Snapshot) Validate() error {
	if err := validateSeq(m.Seq); err != nil {
		return err
	}
	if err := validateValidFor(m.ValidForSeconds, true); err != nil {
		return err
	}
	if len(m.Things) > MaxThings {
		return fmt.Errorf("snapshot has %d things; max is %d", len(m.Things), MaxThings)
	}
	seen := make(map[string]bool, len(m.Things))
	for i := range m.Things {
		t := &m.Things[i]
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate thing id %q", t.ID)
		}
		seen[t.ID] = true
	}
	return nil
}

// Validate checks the delta on its own.
func (m *Delta) Validate() error {
	if err := validateSeq(m.Seq); err != nil {
		return err
	}
	if err := validateValidFor(m.ValidForSeconds, false); err != nil {
		return err
	}
	if len(m.Upsert)+len(m.Remove) > MaxThings {
		return fmt.Errorf("delta has %d changes; max is %d", len(m.Upsert)+len(m.Remove), MaxThings)
	}
	seen := make(map[string]bool, len(m.Upsert)+len(m.Remove))
	for i := range m.Upsert {
		t := &m.Upsert[i]
		if err := t.Validate(); err != nil {
			return err
		}
		if seen[t.ID] {
			return fmt.Errorf("duplicate thing id %q", t.ID)
		}
		seen[t.ID] = true
	}
	for _, id := range m.Remove {
		if id == "" || len(id) > MaxIDBytes {
			return fmt.Errorf("invalid remove id %q", id)
		}
		if seen[id] {
			return fmt.Errorf("thing id %q appears more than once", id)
		}
		seen[id] = true
	}
	return nil
}

// Validate checks the refresh on its own.
func (m *Refresh) Validate() error {
	if err := validateSeq(m.Seq); err != nil {
		return err
	}
	return validateValidFor(m.ValidForSeconds, true)
}

// Seconds returns d as a ValidForSeconds value, rounding up.
func Seconds(d time.Duration) *int64 {
	s := int64((d + time.Second - 1) / time.Second)
	if s < 0 {
		s = 0
	}
	return &s
}
