# sotot source protocol, version 1

This is the contract between a sotot **source** and the sotot daemon. A
source is a separate program that watches something (a GitHub account, a
bills spreadsheet, a chat service) and publishes the **things** that need
its user's attention. Sources can live anywhere, in any language, with
their own credentials; they depend only on this document.

Go sources should use the [`source`](../source) package, which implements
everything here. The Go types are in [`protocol`](../protocol).

## Connecting

Each source has its own **tailcat address**, printed by
`sotot address SOURCE-ID` on the daemon's host. The address is a secret
capability: whoever holds it can publish as that source, and nothing in
the messages can name a different source. Keep it out of command lines,
logs, and URLs; pass it in a file or environment variable.

The source dials the address with a tailcat client, opens TCP port 80
inside the tunnel, and makes a WebSocket request to `/sotot/v1/source`.
The HTTP host name is irrelevant and is never resolved. The path carries
the protocol version; a daemon that doesn't speak v1 fails the upgrade.

Every message, in both directions, is one JSON object in a WebSocket text
frame. Unknown fields are ignored, so fields may be added compatibly.
Unknown message types are errors.

## Things

A thing's identity is (source, `id`). IDs must stay the same across
reconnects and source restarts.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string, required | Opaque, unique within the source, ≤ 256 bytes. |
| `title` | string, required | Short description, ≤ 1 KiB. |
| `body` | string | Plain-text context (never rendered as HTML), ≤ 16 KiB. |
| `url` | string | Absolute `http` or `https` link to the thing or its action page. |
| `kind` | string | Category, e.g. `pull_request`, `meeting`, `bill`, `message`, `email`, `renewal`. |
| `attention_at` | timestamp | When attention should begin. |
| `starts_at`, `ends_at` | timestamps | A timed event. `ends_at` requires `starts_at` and must not precede it. |
| `due_at` | timestamp | A deadline instant. |
| `date` | `YYYY-MM-DD` | A date-only obligation or all-day event. Requires `time_zone`. |
| `time_zone` | IANA zone | Zone for interpreting `date`; only with `date`. |
| `tags` | string array | Labels with no built-in meaning; ≤ 32, each ≤ 64 bytes. |
| `priority` | `"low"` or `"high"` | How pressing the thing is; absent means normal. High sorts first and is highlighted; low sorts last and is dimmed. A source may change it over time, e.g. as a deadline approaches. |

Timestamps are RFC 3339 with an explicit offset (`Z` or `-07:00`). A
`date` is not an instant, and must not be combined with `due_at` or
`starts_at`: provide one representation of each temporal fact. Missing
times mean unknown, not overdue. Reaching a date doesn't remove a thing;
the source removes things when they're resolved.

## Messages from the source

**`snapshot`** replaces the source's entire set of things. It must be the
first message on every connection (an empty `things` array is fine and
means "nothing needs attention"). Further snapshots are allowed.

```json
{"type":"snapshot","seq":1,"valid_for_seconds":2592000,"things":[
  {"id":"example.net","title":"Renew example.net","kind":"renewal","date":"2027-06-12","time_zone":"America/Los_Angeles"}
]}
```

**`delta`** atomically upserts and removes things. An upsert replaces the
whole thing; it's not a field patch. Removing an unknown ID is harmless.
An ID may appear only once per message.

```json
{"type":"delta","seq":2,"upsert":[{"id":"pr:123","title":"Review connection cleanup","kind":"pull_request"}],"remove":["pr:122"]}
```

A snapshot may include **`summary`**, a very short description of the
source's state (≤ 64 bytes, one line), such as `"15 domains"`. The
dashboard shows it in place of "Connected". A delta may change it with its
own `summary` field; a snapshot without one clears it.

**`refresh`** renews validity without changing things:

```json
{"type":"refresh","seq":3,"valid_for_seconds":86400}
```

`seq` starts at 1 on every connection and increases by one with each
message. There's no replay across connections: reconnecting always starts
with a snapshot, so a source must be able to produce its complete current
set at any time.

### Validity

`valid_for_seconds` (required on `snapshot` and `refresh`, optional on
`delta`) is how long the data stays fresh after the source disconnects,
counted from when the daemon accepts the message. It's a cache lifetime,
not a deadline. A delta without it leaves the expiry unchanged: learning
about one thing doesn't vouch for all the others. Zero means the data goes
stale as soon as the source disconnects.

While connected, a source is current regardless of expiry. Choose the
validity from the source's schedule: a job that runs daily might use a
little over a day.

## Messages from the daemon

**`ack`** confirms that the data message with the same `seq` was accepted
and applied. It means accepted, not yet durably on disk.

```json
{"type":"ack","seq":3,"received_at":"2026-09-12T19:00:00Z","expires_at":"2026-09-13T19:00:00Z"}
```

A one-shot source must wait for its snapshot's ack before exiting.
Closing the socket isn't proof of publication.

**`error`** reports why a message was rejected, after which the daemon
closes the connection. Nothing in a rejected message is applied. The
source should fix the problem or reconnect and send a fresh snapshot.

```json
{"type":"error","seq":4,"message":"got seq 5; want 4"}
```

## Liveness and sessions

- The first snapshot must arrive within 30 seconds of connecting.
- The daemon sends WebSocket pings every 30 seconds and drops a session
  that doesn't answer within 90 seconds. Keep reading the socket even with
  nothing to send; WebSocket libraries answer pings while reading. Pings
  prove liveness only; they don't renew validity.
- Only one session per source is authoritative. A new connection takes
  over when its first snapshot is accepted; the old one then receives an
  `error` ("superseded") and is closed. Two publishers for one source is a
  misconfiguration.
- Reconnect with exponential backoff and jitter, about 1 second growing to
  1 minute, reset after a session is established.

## Limits

| Limit | Value |
| --- | --- |
| Message size | 8 MiB |
| Things per source | 10,000 |
| `valid_for_seconds` | 0 to ten years |
