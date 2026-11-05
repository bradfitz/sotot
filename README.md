# sotot: Stay On Top Of Things

sotot is a small daemon and read-only dashboard for the things that need
your attention, from the next few minutes to months away: PRs to review,
email to answer, bills to pay, domains and registrations to renew.

Each integration is a separate program, a **source**, that publishes
**things** to the daemon over a documented WebSocket protocol, carried by
[tailcat](https://github.com/tailscale/tailcat) so sources need no
inbound ports. Sources can live anywhere, with their own credentials.

| | |
| --- | --- |
| [`cmd/sotot`](cmd/sotot) | the daemon: source listeners, state, dashboard (TCP or tsnet) |
| [`cmd/sotot-cast`](cmd/sotot-cast) | shows the dashboard on a Chromecast |
| [`cmd/sotot-gandi`](cmd/sotot-gandi), [`cmd/sotot-porkbun`](cmd/sotot-porkbun) | sources: domain expiries at those registrars |
| [`cmd/sotot-gmail`](cmd/sotot-gmail) | source: Gmail threads with a label |
| [`cmd/sotot-demo-live`](cmd/sotot-demo-live), [`cmd/sotot-demo-periodic`](cmd/sotot-demo-periodic) | fake sources for testing |
| [`source`](source) | Go client for writing sources |
| [`protocol`](protocol) | the wire types |

- [`docs/source-protocol.md`](docs/source-protocol.md): the source protocol, for writing sources in any language.
- [`design.md`](design.md): the design and its current status.

## Quick start

    go generate ./web     # builds the dashboard's Go/Wasm browser client
    go run ./cmd/sotot new-identity keys/demo.json
    cat > sotot.json <<'EOF'
    {"version": 1, "sources": [{"id": "demo", "name": "Demo", "listeners": [{"identity_file": "keys/demo.json"}]}]}
    EOF
    go run ./cmd/sotot run --config=sotot.json --state-dir=state &
    SOTOT_ADDR=$(go run ./cmd/sotot address --config=sotot.json demo) go run ./cmd/sotot-demo-live

Then open http://127.0.0.1:8080/.

Requires Go 1.27.1 or later.
