# Fleet Command architecture

Fleet Command is the optional central control plane for multiple Shipmates
captains. Captains make outbound websocket connections through remotedialer,
so a ship does not expose an inbound port. Fleet Command uses that tunnel to
reach the captain's loopback-only HTTP server.

## Runtime shape

```text
browser / CLI
     |
Fleet Command (auth, UI, aggregate APIs, voice boundary)
     |
remotedialer websocket
     |
captain server (events, permissions, PTY, beads, attachments)
```

The captain registry and event history are intentionally in memory. Fleet
Command no longer mirrors events to SQLite: the mirror had no read path and
made a transient control plane look durable when it was not. Captain event
logs are capped and consumed with monotonically increasing sequence cursors.

## Boundaries

- `internal/server` owns one ship's local API and bounded event log.
- `internal/fleet` owns tunnels, authentication, aggregation, streaming, and
  the embedded operator UI.
- `internal/api` contains DTOs shared by local, fleet, and CLI callers.
- `internal/backend` describes backend capabilities; callers fail closed for
  unsupported operations.
- `internal/persona` is the canonical frontmatter parser used by rendering,
  project configuration, and Codex adaptation.

Buffered fleet proxy calls and long-lived PTY streams share the same
`http.Transport` tunnel implementation. There is no second HTTP parser.

## Deliberate product choices

The voice surface (`conversation`, STT, and TTS) remains, but is isolated at
the Fleet boundary and can be left unconfigured. Cursor and Windsurf exports
also remain because they are explicit render targets, not runtime dependencies.
The removed captain skill was an embedded catalog artifact with no install or
lookup path; the captain persona remains supported.

See [api.md](api.md) for the maintained route inventory.
