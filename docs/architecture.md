# Architecture

Shipmates is a Codex-native persona runtime with persistent project memory. The
public product consists of five deliberately separate planes.

## Project state

| Path | Purpose |
|---|---|
| `.codex/agents/<persona>.toml` | Installed Codex persona definition |
| `.shipmates/memory/<persona>/` | User-owned durable project memory |
| `.shipmates/policies/<persona>.yaml` | Project-local execution policy |
| `.shipmates/sessions/` | Bounded Codex continuity and local discovery state |
| `.shipmates/manifest.json` | Managed-file baselines and catalog version |

Catalog installation is conservative: missing memory seeds may be added, but
normal updates do not replace learned memory. Managed file conflicts require an
explicit keep/take decision. Persona inventory is derived from canonical Codex
artifacts, not legacy directories.

## Dispatch plane

Local raster-image input is a turn-start primitive, not an attachment store.
`ask --image` maps validated descriptors to Codex `exec --image` arguments;
managed turns map them to text-first app-server `localImage` inputs. Validation
bounds count and size, verifies magic bytes, pins filesystem identity, refuses
links and out-of-project paths, and revalidates immediately before handoff.
Only `image_count` may enter normalized events.

There is no upload endpoint, inbox, attachment ID, URL fetch, byte transport,
thumbnail, retention state, remote image capability, or mid-turn image
steering. Dashboard selection exists only in the attached local controller and
applies atomically to its next exact idle turn.

Synchronous `ask`, `fanout`, and drain operations load the installed persona,
validate its immutable policy snapshot, and invoke `codex exec --json` directly.
Normalized JSON events provide the thread identifier and final response.
Shipmates stores a bounded continuity marker so a later turn can resume the
same Codex thread. `--fresh` creates a new one.

## Live control plane

`live` and `open` use a managed `codex app-server --stdio` child. The adapter
accepts bounded protocol frames and projects them into closed event categories;
raw prompts, tool payloads, credentials, and unrestricted diagnostics are not
published.

The local server binds an ephemeral loopback address. Its supported routes are:

- lifecycle: `GET /health`, authenticated `POST /shutdown`;
- controller: `POST /api/live/{persona}` with `attach`, `release`, `heartbeat`,
  `sync`, `action`, `approval`, `tell`, and `interrupt` operations, plus the
  authenticated feed; and
- exact local Fleet adapters for already-active steer and interrupt targets.

Controller attachment is a lease, not a bearer permission to arbitrary state.
Only the exact active controller may send a message, interrupt, or decide its
captured approval card. EOF, cancellation, and detach release the lease without
interrupting the worker.

There is no generic process terminal. Terminal handling in `open` is local UI
presentation: raw mode is restored on every exit path, submitted lines are
bounded, and backend output is rendered from normalized events only.

## Policy and approval plane

Policy loading produces an immutable semantic snapshot from project-local
Shipmates rules. `allow` and `deny` decisions are automatic. `ask` may pause an
exact Codex app-server request while the active local dashboard displays a
sanitized approval card. The controller may allow that request once or deny it;
timeout, lease loss, stale authority, or delivery uncertainty fails closed.

Approvals do not create durable grants. They are not exposed through top-level
commands, Fleet, hooks, or generic resolve endpoints.

## Fleet plane

Fleet uses separate identities and capabilities:

1. `ship observe` publishes a bounded snapshot/event projection over an
   outbound authenticated tunnel.
2. The observer service exposes authenticated read-only roster, snapshot,
   events, and follow APIs plus a static read-only UI.
3. Remote steer and interrupt use distinct scoped credentials, exact ship and
   turn binding, closed tunnel messages, deduplication, deadlines, and sanitized
   immutable audit records.

Remote operations apply only to a turn that already exists. Fleet has no
generic command proxy and no authority to start work, approve requests, mutate
a graph, transfer files, or control a terminal.

## Lifecycle and shutdown

The CLI discovers the exact project server from authenticated state. `server
stop` sends only the authenticated shutdown request; stale or mismatched state
fails closed. The supervisor may spawn only the current Shipmates executable's
`server serve` command. Child processes are monitored and reaped during bounded
shutdown.

Ordinary startup reads no legacy runtime state and creates no legacy inbox,
pending/grant store, hook configuration, or alternate backend session marker.

## Security, accessibility, and performance boundaries

- Secrets and private turn tuples are excluded from observer and dashboard
  projections; credentials are role-separated and stored as verifiers.
- Browser assets use semantic headings, a labelled credential input, keyboard
  native controls, table headers, alert/status regions, and a responsive
  small-screen table layout.
- The observer has no framework or third-party client bundle. Its small embedded
  HTML/CSS/JavaScript assets render with text nodes, cap retained events, poll
  bounded pages, and avoid storing credentials.
- All listeners, routes, subprocesses, and background workers are closed
  allowlists. Unknown and deleted paths are absent rather than redirected.

## Explicitly deferred

Image/file/PDF input, upload/inbox handling, remote start or approval, durable
grants, hooks/plugins/webhooks, graph dispatch and scheduling, Codex terminal
passthrough, rescue/restart, broadcast, hosted relay, conversation, and voice
input/output are not shipped architecture.
