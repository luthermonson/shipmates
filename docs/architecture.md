# Shipmates architecture

Shipmates installs persistent agent personas into a repository and provides a
small runtime for delegation, permissions, live sessions, and optional fleet
control. Repository files remain the source of truth; the local server is a
transient coordination process, not a database.

## On-disk model

```text
.shipmates/
  shipmates.yaml       project and fleet configuration
  manifest.json        hashes of files managed by install/update
  install-id           stable identity for this checkout
  memory/<persona>/    persona-owned durable context
  policies/            installed permission overlays
  sessions/            transient ports, logs, and backend session markers
.claude/agents/         rendered Claude personas
.codex/agents/          rendered Codex personas
```

Runtime state under `.shipmates/sessions/` and uploaded attachment inboxes are
ignored by Git. The manifest never owns accumulated persona memory.

## Canonical definitions

Persona catalog files contain YAML frontmatter plus a Markdown body.
`internal/persona` parses that format once. Rendering, project configuration,
and Codex adaptation consume the same typed definition so supported fields do
not drift between callers.

The catalog is embedded into the Go binary. `shipmates add` renders a persona
for installed harnesses and seeds its memory. `shipmates update` compares the
manifest hash before overwriting a managed file, preserving user edits.

## Backend capabilities

`internal/backend` resolves each persona backend and exposes capabilities
instead of relying on scattered string checks.

| Backend | Headless | Interactive open | Live tell | Fleet PTY |
|---|---:|---:|---:|---:|
| Claude | yes | yes | yes | yes |
| Codex | yes | no | no | no |
| Command | no | no | no | yes |

Unsupported and unknown backends fail closed with a surface-specific error.
Claude and Codex headless dispatch share project session metadata conventions;
arbitrary commands are hosted only under the PTY boundary.

## Local execution

- `shipmates ask` runs one headless turn and resumes the persona's backend
  session on later calls.
- `shipmates fanout` invokes the same dispatch path concurrently and buffers
  each result independently.
- `shipmates open` starts a supported interactive session.
- `shipmates tell` sends a message to a supported live process through the
  local captain server.

`internal/project` is split into configuration, layout, session, install-ID,
and manifest files. Session launch policy is centralized there, so command
surfaces do not independently invent resume IDs or names.

## Captain server

The captain server binds loopback and owns live-process state, PTYs, pending
permissions, status, and a recent activity log. Harness hooks write through
`POST /hook/{persona}/{event}`. There is no second event-ingestion interface.

Events receive a strictly increasing sequence number and RFC3339 timestamp.
Only the newest 2,048 events are retained. Readers request
`GET /events?after=<seq>`; Fleet Command turns those cursor reads into SSE.
This bounds memory while preserving ordered reconnect behavior.

For `PreToolUse`, the permissions evaluator applies these layers:

1. fleet-wide denies;
2. installed persona policy;
3. project and user Claude settings;
4. temporary exact-command approvals.

Deny wins, then ask, then allow. Human decisions appear in the single JSON
`GET /pending` endpoint and resolve through `POST /resolve/{id}`. Missing or
unknown policy fails toward human review.

The process tracks live and PTY children directly. An idle watchdog bounds
standalone servers; a fleeted captain keeps its lightweight tunnel alive while
reaping idle mate processes. `POST /shutdown` provides graceful explicit stop.

## Fleet Command

Fleet Command accepts outbound remotedialer websocket connections from
captains. Its operator API aggregates captain DTOs from `internal/api` and
proxies bounded calls and PTY streams over one shared HTTP transport. It does
not persist or mirror captain events.

The embedded browser UI is build-free JavaScript. `app.js` coordinates views,
while `api.js` owns authenticated request behavior and `utils.js` owns shared
formatting/conversion helpers. Conversation and optional voice behavior remain
separate in `conversation.js` and the Fleet voice handlers.

See [api.md](api.md) for the supported routes and
[fleet-architecture.md](fleet-architecture.md) for the tunnel boundary.

## Verification contract

Pull requests run the unit suite, `go vet`, and the race detector. A cleanup
audit should additionally run JavaScript syntax checks and the Go dead-code
analyzer. Generated session logs, attachment staging files, and local inboxes
must not appear as source changes.
