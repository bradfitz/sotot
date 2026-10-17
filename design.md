# sotot — Stay On Top Of Things

Status: initial design, 2026-09-12. This document records agreed requirements
and proposes a first protocol. Proposed defaults remain adjustable before v1.

## Purpose

sotot is a Go daemon and read-only web dashboard for things requiring personal
attention, from the next few minutes to months away. Examples include PRs to
review or merge, messages and emails, meetings, vacations, estimated taxes,
vehicle registration renewals, and domain expirations.

Every integration is a separate program. Sources can live in this repository
or external private repositories, use their own credentials and configuration,
and publish through a documented, versioned schema. sotot does not contain
provider-specific polling, webhook, or credential logic.

Sources push initial snapshots and subsequent updates over WebSockets carried
by tailcat. sotot pushes changes to dashboard browsers over another WebSocket.
For example, merging a PR elsewhere triggers a provider webhook to its source,
which removes the PR from sotot immediately.

## Agreed scope

- Go for the daemon, sources, shared protocol types, and client library.
- Read-only dashboard. No dismiss, snooze, mark-done, or provider commands in v1.
- Dashboard grouping, urgency ranking, and time horizons are deliberately deferred.
- Cache last known data as JSON files per source; no SQLite initially.
- Dashboard can use an ordinary TCP listener (`--listen=:8080`, including
  `--listen=127.0.0.1:8080`) or a `tsnet.Server` listener.
- Connected sources count as current. Heartbeats detect dead connections;
  independently detecting a live but hung integration is out of scope.
- Disconnected data remains fresh until the source-specified expiration.
- Named source identities survive changes to their tailcat listening addresses.

## Tailcat integration, grounded in the implementation

Reviewed [tailcat README](https://github.com/tailscale/tailcat/blob/91dc4979bd4ae88af6ae2c8bb549616de4bcaa5a/README.md)
and [Go library](https://github.com/tailscale/tailcat/blob/91dc4979bd4ae88af6ae2c8bb549616de4bcaa5a/tailcat.go)
at commit `91dc4979bd4ae88af6ae2c8bb549616de4bcaa5a`.

Tailcat is a userspace WireGuard transport without the Tailscale control plane.
It bootstraps through DERP, attempts direct UDP connectivity, and falls back to
DERP. Sources need neither Tailscale accounts nor public inbound TCP ports.

The concrete APIs are:

- `tailcat.Server{OnTCP: ...}` followed by `Start()` accepts TCP streams through
  callbacks. There is no public `Server.Listen` returning a `net.Listener` in
  the reviewed code. An adapter must queue accepted `net.Conn`s for `http.Server`.
- `Server.TailcatAddr()` returns the bearer capability after startup, including
  full DERP region details. Treat it as opaque and secret.
- `tailcat.NewClient(addr).DialTCPPort(ctx, port)` opens a stream to that server.
  Use this through a custom HTTP transport for the source's WebSocket dial.
- `Server.Key`, `Server.PresharedKey`, and a fixed `Region` or `RegionID` provide
  persistent listener identity. Restoring only the node key is insufficient.
- `tailcat.NewPrivateKey()` creates node and preshared keys but leaves relay
  selection to the caller. Generate persistent material before starting the
  listener; do not assume zero-value generated keys are copied into public fields.
- Server configuration such as `OnTCP` is set before `Start()`. Reconfiguration
  replaces servers rather than mutating these fields on running instances.

### Ownership and connection direction

Proposed interpretation of the source-push requirement: sotot owns one tailcat
server/listener per named source. Each source receives its corresponding
capability address and dials into sotot. A listening address is not enough to
recreate its listener: sotot also retains the corresponding private key material.

Use a fixed tunnel TCP port, proposed port 80, exposing only the ingestion HTTP
handler at `/sotot/v1/source`. This port is inside tailcat, not a host TCP binding.
The source HTTP transport uses a synthetic hostname and dials the configured
tailcat address directly; it does not resolve that hostname through DNS or place
the case-sensitive capability in a browser URL. Plain HTTP/WebSocket inside the
WireGuard tunnel is sufficient for this transport.

The listener determines source identity. A source cannot select another source
by supplying a name in its JSON. Separate ports under one shared capability
would not provide equivalent isolation. Each listener exposes ingestion only,
never the dashboard or other sources' data.

The adapter must bound its queue, close connections it cannot accept, unblock
`Accept` on shutdown, and explicitly track and close upgraded WebSockets.

## Source configuration and address changes

Each source has an immutable ID, editable display name, and one or more listener
identity references. One listener is normal; two permit an address transition.
The advertised tailcat address is derived from a listener identity and relay.
Capabilities and private material are excluded from browser payloads and logs.

Proposed JSON configuration shape (paths relative to the configuration file):

```json
{
  "version": 1,
  "sources": [
    {
      "id": "github-work",
      "name": "Work GitHub",
      "listeners": [{"identity_file": "keys/github-work.json"}]
    },
    {
      "id": "domains",
      "name": "Domain renewals",
      "listeners": [{"identity_file": "keys/domains.json"}]
    }
  ]
}
```

The identity file uses tailcat private-key fields and pinned relay information;
exact provisioning CLI is an implementation detail to settle next. Exporting
an address is an explicit local administrative operation.

Proposed reload mechanism: SIGHUP on supported systems; restart also reloads.
Validate the full configuration and start replacement listeners before applying
the change. Failure retains the previous configuration and closes any staged
listeners. Unchanged listeners remain running. Cache is keyed by source ID,
never by its current capability address.

For rotation, add a new listener, distribute its address out of band, migrate
the source, then remove the old listener. Removing a listener closes its active
sessions as well as preventing new connections. Immediate revocation skips the
overlap. Rotation does not erase last known source data. A removed source no
longer appears in the dashboard; its cache can remain for explicit cleanup.

## Stable item schema

Publish Go types and protocol documentation together. Source authors depend on
the wire contract, not daemon internals. An item's identity is `(source ID, item
ID)`; item IDs must remain stable across reconnects and source restarts.

Proposed v1 fields:

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string, required | Opaque ID unique within the source. |
| `title` | string, required | Short human-readable description. |
| `body` | string, optional | Plain text context; no arbitrary HTML. |
| `url` | string, optional | HTTP(S) link to the original item or its action page. |
| `kind` | string, optional | Descriptive category such as `pull_request`, `meeting`, `renewal`. |
| `attention_at` | timestamp, optional | When the source thinks attention should begin. |
| `starts_at` | timestamp, optional | Event start. |
| `ends_at` | timestamp, optional | Event end. |
| `due_at` | timestamp, optional | Deadline, distinct from event start and cache expiration. |
| `date` | string, optional | `YYYY-MM-DD` for a date-only event or obligation. |
| `time_zone` | string, optional | IANA zone for interpreting `date`; required with `date`. |
| `tags` | string array, optional | Descriptive labels; no built-in ranking semantics. |

Timestamps use RFC 3339 with an explicit offset; persist server timestamps in
UTC. Date-only obligations remain dates rather than invented midnight instants.
`date` and `due_at` are alternative date-only and instant representations of an
obligation. For events, `date` may represent an all-day event, while
`starts_at`/`ends_at` represent timed events. Do not provide both representations
for the same temporal fact. Multi-day all-day ranges are deferred.

Missing times are unknown, not overdue. No required urgency enum or scoring
formula yet. The UI can display supplied dates without committing to a ranking
scheme. An item reaching its date or deadline does not automatically remove it.
The source removes resolved items in a delta or its next snapshot.

## Source WebSocket protocol v1

Each text frame contains one JSON object. The version is negotiated by the v1
endpoint; unsupported versions fail the upgrade. Unknown optional fields are
ignored for additive compatibility; unknown message types are protocol errors.
All examples below are proposed normative v1 shapes.

The first source message must be a full snapshot, including an empty array when
there is nothing to report. Set a proposed 30-second initial snapshot deadline.
The daemon validates and replaces the source's item set atomically; a partial or
invalid snapshot never erases the previous data.

```json
{
  "type": "snapshot",
  "seq": 1,
  "valid_for_seconds": 2592000,
  "items": [
    {
      "id": "example.net",
      "title": "Renew example.net",
      "kind": "renewal",
      "date": "2027-06-12",
      "time_zone": "America/Los_Angeles"
    }
  ]
}
```

`valid_for_seconds` is a required nonnegative integer. On acceptance, sotot sets
`expires_at = received_at + valid_for_seconds`. It is a cache validity period,
not an item's deadline. A 30-day validity is exactly 2,592,000 seconds.
Zero means data becomes stale immediately when the source disconnects.

The source can then remain connected and send atomic deltas:

```json
{
  "type": "delta",
  "seq": 2,
  "upsert": [{"id": "pr:123", "title": "Review connection cleanup", "kind": "pull_request"}],
  "remove": ["pr:122"]
}
```

An upsert replaces the entire item; it is not a field patch. Removing an unknown
ID is harmless. Duplicate IDs within a snapshot or message, or the same ID in
both delta lists, are errors. A delta may include `valid_for_seconds` to refresh
the validity of the whole resulting dataset. If absent, expiry is unchanged:
learning about one item does not implicitly renew confidence in all other items.

A source may renew validity without changing items:

```json
{"type":"refresh","seq":3,"valid_for_seconds":86400}
```

Additional full snapshots are allowed. Sequence numbers start at 1 for every
new connection and increment by one for snapshot, delta, and refresh messages.
There is no replay across connections: reconnect always starts with a snapshot.
Reject invalid sequences and invalid messages without partial application,
send a bounded error response, then close so the source can resynchronize.

After committing a data message to memory, sotot replies with an acknowledgement:

```json
{"type":"ack","seq":3,"received_at":"2026-09-12T19:00:00Z","expires_at":"2026-09-13T19:00:00Z"}
```

An ack means accepted, not guaranteed durable on disk. A one-shot source waits
for its snapshot ack before closing. It must not interpret socket closure alone
as successful publication. The Go helper handles WebSocket closure and bounded
tailcat transport cleanup without exiting before the ack arrives.

### Liveness and competing connections

Proposed default: sotot sends WebSocket pings every 30 seconds and requires a
matching pong within 90 seconds. Source clients continuously read control frames
even when they have no item updates. Pongs assert process/connection liveness;
they do not renew persisted dataset expiration.

After its first accepted snapshot, a live session makes the source current
regardless of `expires_at`. Once that connection closes or times out, use the
stored expiration immediately. There is no extra TTL beginning at disconnect.

Only one session is authoritative per source. A new connection becomes
authoritative after its valid initial snapshot; until then, the old session
continues. The new snapshot atomically supersedes the old dataset and session.
Close the old session and fence its queued messages using a daemon-assigned
session generation. An old disconnect must not mark the new session offline.
Running competing publishers for one source is a configuration error.

Reconnect with exponential backoff and jitter, proposed 1 second to 1 minute,
reset after a successfully established publishing session. Source integrations
must capture a snapshot and subsequent changes without a gap in their own data
stream; the Go helper can serialize publication but cannot repair provider gaps.

## Source status and freshness

Track `last_connected_at` when a WebSocket session is established,
`last_disconnected_at`, `last_message_at` for accepted data messages, and
`last_pong_at` for runtime liveness. Track `has_snapshot`, `expires_at`, and the
current authoritative session separately.

| Condition | Display |
| --- | --- |
| Live authoritative session with an accepted snapshot | Bright green: Connected |
| No authoritative session; cached snapshot exists and now is before expiry | Muted/dark green: Fresh, disconnected |
| No authoritative session; no snapshot or now is at/after expiry | Red: Never reported / Stale |

A newly opened session awaiting its first snapshot has a Connecting annotation
and retains its previous data status. An accepted empty snapshot is fresh data.
Keep stale items visible with a source-level warning; do not silently imply
that nothing needs attention. Use labels/icons as well as color.

Timers publish status changes at expiry even if no source sends anything.
At daemon startup, every source is disconnected until a new session establishes
current data. Load cached expiry as written; restarting never extends validity.

## JSON disk cache

Use one versioned JSON file per stable source ID, containing items, whether a
snapshot exists, expiry, data revision, and historical connection/data timestamps.
Store listener private material separately from cache data. Use validated source
IDs (proposed `[a-z0-9][a-z0-9_-]{0,63}`) for safe filenames, private directories,
and owner-only files. Allow only one daemon to own a state directory at a time.

Serialize mutations per source. After acceptance, enqueue its latest full cache
record for a serialized writer. Coalesce rapid updates with a proposed maximum
one-second flush delay, avoiding per-heartbeat writes. Write to a temporary file
in the same directory, sync it, rename over the previous file, and sync the
directory where supported. Flush pending state on graceful shutdown.

Memory is the running authority. Disk failure leaves real-time operation working
but exposes a cache-write error in the dashboard/logs and retries persistence.
An abrupt crash can lose recently acknowledged updates; the next source snapshot
repairs them. The cache is not an event log or a durable message queue.

Validate cache version and contents at startup. A missing or corrupt source
cache affects only that source and produces a visible diagnostic. Preserve a
corrupt file for diagnosis rather than treating it as an empty valid snapshot.
Never persist or restore an active-session flag as evidence of a live source.

## Dashboard serving and realtime updates

Normal mode uses `net.Listen("tcp", listenAddr)` and `http.Server.Serve`.
Proposed tsnet mode adds `--tsnet` and `--tsnet-hostname=sotot`, using a persisted
`tsnet.Server` and its `Listen("tcp", listenAddr)`. Reject loopback-only binding
strings in tsnet mode if unsupported. The exact extra flag names are provisional.
Tailcat source listeners and the dashboard's tsnet node have separate identities
and lifecycles. Tailcat ingestion still requires no tailnet membership.

Initial scope is a single user's dashboard. Normal mode relies on the chosen
bind address and deployment's access controls; tsnet mode relies on tailnet
access policy. Do not expose ingestion on the browser listener. Browser
WebSockets require same-origin validation and inherit dashboard access control.

Serve Go-rendered HTML and embedded styles. To honor Go for application logic,
propose a small Go/Wasm browser client for WebSockets and DOM updates, with only
the standard Wasm runtime/bootstrap JavaScript. No separate JavaScript application
or Node build is required. This choice should be checked against initial page
weight; a minimal JavaScript client would require revisiting the all-Go preference.

On browser connection, send a complete dashboard snapshot followed by ordered
updates for item and source-status changes. Use a daemon instance ID plus a
monotonic revision. Register subscriptions and capture the initial snapshot under
the same synchronization boundary so updates cannot fall between them.
Reconnect always requests a fresh full snapshot; no replay log in v1.

Bound per-browser queues. A slow browser is disconnected and resynchronizes,
without blocking ingestion. Show a dashboard-connection warning while browser
updates are unavailable; do not imply displayed source statuses are live then.

The initial view can list source status and items with their supplied dates and
links. Specific grouping, filtering, urgency sorting, and horizon presentation
remain deferred until real sources provide experience. Render source text safely
and do not fetch arbitrary source-provided URLs on the daemon.

## Repository and source client

### Chromecast display companion

We also want a separate Go side component, provisionally `sotot-cast`, that
shows the dashboard on a Chromecast device on the LAN. It is a dashboard
consumer, not a data source, and runs independently of the main daemon.

The companion will select a configured Chromecast, establish and maintain the
display session, and recover after device or network interruptions. The display
should reflect realtime dashboard updates and show when its connection to sotot
is unavailable, preserving the dashboard's source-freshness indicators.

The exact Cast receiver and rendering approach is deferred. The design must
account for how the display reaches dashboard content when sotot is listening
only on localhost or through tsnet; the companion may need to bridge access
from its host to the LAN device. Source-ingestion capability tokens must not be
passed to the display. Device discovery, selection, and access configuration
will be specified when implementing the companion.

### Layout

Proposed layout:

```text
cmd/sotot/                 daemon entry point
cmd/sotot-cast/            optional LAN Chromecast display companion
cmd/sotot-demo-live/       updates, removals, and reconnects
cmd/sotot-demo-periodic/   snapshot, ack, disconnect, expiry
protocol/                 public JSON types and validation
source/                   public Go publishing client
internal/ingest/           tailcat and WebSocket sessions
internal/state/            source state, serialized mutations, cache
internal/dashboard/        HTTP handlers and subscriptions
web/                      Go/Wasm client, templates, styles
docs/source-protocol.md    standalone external-source contract
design.md                 this design
```

The client wraps tailcat dialing, WebSocket handshakes, sequencing, acks,
heartbeat handling, reconnection, and serialized writes. It requests a fresh
snapshot from its integration on every reconnect. Integrations retain their own
provider state and decide when a publication legitimately extends cache validity.
Pin a reviewed tailcat version/commit: its README explicitly disclaims API and
wire stability. Do not inherit tailcat CLI build tags blindly, since sotot also
needs tsnet functionality for dashboard mode.

Potential external sources include Telegram, Signal, Gmail, Google Calendar,
GitHub PRs, recurring obligations from labeled private GitHub issues or Sheets,
and domain expiry from Gandi, Porkbun, or registration lookup data. Provider API
feasibility and date accuracy belong to each integration. Webhook reception is
also the source's responsibility; tailcat ingestion does not expose a public
webhook endpoint for GitHub or other providers.

## Implementation stages and meaningful checks

1. Freeze the first protocol types and examples, with validation and documented
   limits. Proposed initial configurable limits: 8 MiB per message, 10,000 items
   per source, and bounded title/body lengths.
2. Implement source state, cache, liveness, and ingestion with both demo sources.
3. Implement the read-only dashboard and both listener modes.
4. Exercise actual source data before choosing attention ranking or adding
   provider integrations to this repository.

Check atomic snapshot replacement and deltas; rejected messages retaining prior
state; empty versus never-reported data; live sessions past cache expiry;
disconnect and timed expiry; restart without TTL extension; session takeover
fencing; address rotation; corrupt cache and write failure; subscription ordering;
and slow-browser isolation. Use an injected clock for expiration checks and a
focused tailcat/WebSocket integration check for the transport adapter.

Deferred: snoozes and other local mutations, commands back to sources,
multi-user permissions, cross-source deduplication, a durable history, replay,
advanced temporal modeling, and final dashboard organization.
