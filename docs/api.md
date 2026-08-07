# HTTP route inventory

This inventory documents the supported HTTP surface after the cleanup.

## Captain server

| Method and path | Purpose |
|---|---|
| `GET /health` | Readiness probe. |
| `GET /events?after=<seq>` | Bounded event history after a sequence cursor. |
| `POST /hook/{persona}/{event}` | Ingest a hook; `PreToolUse` may block for permission. |
| `GET /feed` | Human-readable recent activity for the CLI. |
| `GET /pending` | Pending decisions as JSON. |
| `POST /resolve/{id}` | Resolve a pending decision. |
| `POST /tell/{persona}` | Send input to a supported live backend. |
| `POST /attach` | Stage an attachment for a mate. |
| `GET /status.json` | Mate runtime status. |
| `/pty/{persona}/*` | Start, stream, snapshot, input, resize, and lock a PTY. |
| `/bead*` and `/beads*` | Local task graph reads and mutations. |
| `POST /shutdown` | Graceful local shutdown. |

The obsolete `POST /events`, `/register`, `/deregister`, and duplicate
`/pending.json` interfaces were removed. Hook ingestion is the sole event write
path, and `/pending` is the sole structured pending endpoint.

## Fleet Command

| Method and path | Purpose |
|---|---|
| `/connect` | Authenticated captain websocket tunnel. |
| `GET /health` | Readiness probe. |
| `GET/POST /login`, `POST /logout` | Browser session authentication. |
| `GET /api/captains` | Captain registry snapshot. |
| `GET /api/fleet-policy` | Fleet-wide deny policy for connected ships. |
| `GET /api/status` | Fleet-wide mate status. |
| `GET /api/pending` | Fleet-wide pending decisions. |
| `GET /api/beads`, `POST /api/beads/nudge` | Aggregate task graph and sync nudge. |
| `GET /api/captain/{key}/stream` | Cursor-backed captain event SSE. |
| `GET /api/captain/{key}/feed` | Proxied human-readable feed. |
| `GET /api/captain/{key}/pending` | One captain's pending JSON. |
| `/api/captain/{key}/pty/{persona}/*` | Proxied PTY operations. |
| `/api/captain/{key}/bead*` | Proxied task graph operations. |
| `POST /api/captain/{key}/tell/{persona}` | Proxied live message. |
| `POST /api/captain/{key}/attach` | Proxied attachment upload. |
| `POST /api/captain/{key}/resolve/{id}` | Proxied permission resolution. |
| `/api/conversation`, `/api/stt`, `/api/tts`, `/api/voice/config` | Optional voice boundary. |
