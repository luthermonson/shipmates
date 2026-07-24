# Architecture

Shipmates is a persona runtime with persistent project memory. It sits
between the human captain and an underlying agent runtime — Claude Code or
OpenAI Codex — and adds durable memory, local policy, delegation,
voyages, a local dashboard, and narrow Fleet observation. The public
product consists of six deliberately separate planes.

**Runtime scope.** The `internal/runtime` package defines a shared
interface with `claude` and `codex` implementations, selected by
`.shipmates/config.yaml` / `--runtime`. `ask` honors that selection —
resolving `claude` dispatches the turn through the runtime interface,
resolving `codex` (the default) uses the codex-native dispatcher. The
rest of the production command surface (`open`, `live`, `feed`, `tell`,
`interrupt`, `sail`, `fleet`, `ship`, `server`) is codex-native pending
migration — the sections below describe that path.
See [Runtime interface plan](runtime-interface-plan.md) for the
migration onto the runtime interface, and
[Platform support](platform-support.md) for what runs where.

For installation and command usage, see [Getting started](getting-started.md)
and the [CLI reference](cli-reference.md); this document records ownership and
boundaries rather than repeating setup instructions.

The human operator is captain. Runtime leadership is split between the skipper,
which owns conversation and execution sequencing, and the quartermaster, which
preserves strategic memory and constraints.

## Terminology: two different "servers"

Shipmates docs mention two processes whose names both contain the word
"server". They are not the same:

- **Codex app-server** — a mode of the OpenAI Codex CLI (`codex
  app-server`) owned and shipped by OpenAI. It runs as a JSON-RPC
  process over stdio and is what Shipmates spawns as a managed child
  when driving the codex runtime. The adapter lives in
  `internal/codexapp/adapter.go`. It is cross-platform because it is
  driven over stdio.
- **Shipmates server** — Shipmates' own project-local coordination
  server. It binds an ephemeral loopback address, requires a bearer
  token in `.shipmates/sessions/`, and exposes the lifecycle,
  controller, feed, and Fleet-adapter routes. It is registered as
  `shipmates server` (`internal/commands/servercmd.go`) and implemented
  in `internal/server/`. The CLI-level `server` subcommand is registered
  only on unix (via `internal/commands/public_unix.go`), because the
  Fleet Commander subsystem it fronts is unix-only.

When this document (or any other Shipmates doc) says "app-server", it
means Codex. When it says "local server", "loopback server",
"project-local server", or `shipmates server`, it means the Shipmates
process. See [Platform support](platform-support.md) for the platform
matrix.

## Project state

| Path | Purpose |
|---|---|
| `.codex/agents/<persona>.toml` | Installed Codex persona definition (`shipmates init` writes this today) |
| `.claude/agents/<persona>.md` | Claude Code persona definition — written by `runtime.claude.InstallPersona`; the CLI's `init` will emit it once the persona installer is migrated onto the runtime interface |
| `.shipmates/memory/<persona>/` | User-owned durable project memory |
| `.shipmates/policies/<persona>.yaml` | Project-local execution policy |
| `.shipmates/sessions/` | Bounded Codex continuity and local discovery state |
| `.shipmates/manifest.json` | Managed-file baselines and catalog version |
| `.shipmates/voyage.json` | Human-readable captain-approved execution plan |
| `.shipmates/voyages/<hash>.json` | Private resumable state for an immutable plan |

Catalog installation is conservative: missing memory seeds may be added, but
normal updates do not replace learned memory. Managed file conflicts require an
explicit keep/take decision. Persona inventory is currently derived from
canonical Codex artifacts, not legacy directories; a runtime-neutral
canonical persona format lives at `catalog/<persona>/persona.md` and both
runtimes' installers translate from it.

## Voyage plane

The skipper turns a planning conversation into a strict versioned voyage plan.
Planning does not dispatch work. The human captain reviews the complete plan and
must explicitly approve it before `sail` accepts it.

`sail` validates the plan hash, task identifiers, installed personas, dependency
graph, bounds, and approval state before dispatch. It executes only
dependency-ready tasks with bounded concurrency and per-task deadlines. Every
transition is atomically persisted. Completed work survives restart; an
interrupted running task returns to pending; failure blocks dependent work until
the captain intentionally retries or revises the plan.

The terminal display projects persisted state. Persona colors are presentation,
never identity or authority. Redirected output remains complete without ANSI.

`shipmates plan` attaches to the managed Skipper session and renders the
strictly decoded voyage draft beside the conversation. `/consult` performs a
bounded managed architect turn and returns advisory context to the Skipper;
the architect cannot approve or dispatch. `/sail` reloads the approved file and
enters the same Sail engine used by the headless command. Activity identifies
persona, model, and effort. A bounded `SHIPMATES_NEEDS_INPUT:` crew result is
persisted as `needs_input` and returns control to Captain-Skipper chat.

Voyage execution can optionally mirror tasks to the external Beads CLI. Beads
owns its graph storage and schema; projects without a Beads workspace use only
Shipmates' validated plan and voyage state.

## Dispatch plane

Local raster-image input is a turn-start primitive, not an attachment store.
`ask --image` and managed live turns map validated descriptors to text-first
app-server `localImage` inputs. Validation
bounds count and size, verifies magic bytes, pins filesystem identity, refuses
links and out-of-project paths, and revalidates immediately before handoff.
Only `image_count` may enter normalized events.

There is no upload endpoint, inbox, attachment ID, URL fetch, byte transport,
thumbnail, retention state, remote image capability, or mid-turn image
steering. Dashboard selection exists only in the attached local controller and
applies atomically to its next exact idle turn.

Synchronous `ask`, `fanout`, and drain operations load the installed persona,
validate its immutable policy snapshot, and use the managed Codex app-server
turn boundary. Normalized events provide the thread identifier and final
response. Shipmates stores a bounded continuity marker so a later turn can
resume the same Codex thread. `--fresh` creates a new one.

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

### M2 local Commander delegation seam

The M2 seam sits beside, not inside, the Fleet transport plane. A local
processor consumes one Commander-signed M1 envelope supplied by a future
composition layer, validates it against the exact approved local recovery
snapshot, reserves one assessment, calls a fresh read-only/tool-less Sol
adviser, lets Sail validate the response, and appends redacted provenance to
`.shipmates/delegations/<voyage-plan-sha256>.jsonl`. Its only state-changing
entry point is internal; there is no M2 listener, route, CLI registration,
outbound connection, Fleet credential use, work executor, Beads writer, or
voyage mutator. M3 owns the separately authorized transport composition.

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

Generic file/PDF input, upload/inbox handling, remote start or approval, durable
grants, hooks/plugins/webhooks, unapproved or remote graph dispatch, Codex terminal
passthrough, rescue/restart, broadcast, hosted relay, conversation, and voice
input/output are not shipped architecture.
