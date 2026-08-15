# Fleet architecture — Fleet Command + ships + shared memory

> Working doc for the `feat/fleet-command-rename` direction. Where Fleet Command is
> today, where it's going, and the gap between. Companion to `architecture.md`
> (persona/memory fundamentals).
>
> Design inputs: [herdr](https://github.com/ogulcancelik/herdr) (terminal multiplexing model),
> [beads](https://github.com/gastownhall/beads) (distributed graph memory), and the existing
> shipmates captain↔crew loop.

## The one-paragraph vision

Multiple **ships** (machines — a laptop, a homelab box, a cloud VM) each run shipmates
crews working on code. A single **Fleet Command** node gives the **Admiral** (the
human operator) one pane of glass across all of them: live terminals for every mate,
red/yellow/green/blue status per mate, inline approval of risky tools, and a shared
task/memory graph so a captain on Ship A knows what the security mate on Ship B
learned yesterday. Fleet Command is also where the Admiral talks to the **Commodore**
— the AI voice persona embedded in the web UI — for hands-free orchestration. The
Commodore coordinates squadrons on the Admiral's behalf and reports back up. Think
*herdr's UX* + *shipmates' personas and memory* + *beads' federated graph* + *a
voice-driven flag officer* in one product.

## Vocabulary

| Term | Definition |
|---|---|
| **Admiral** | The human operator commanding the whole fleet. Sets strategic intent; the Commodore reports up. This is you. |
| **Fleet Command** | The central coordinator node (`shipmates fleet serve`). Owns the web UI, auth, the captain registry, the voice interface (the Commodore), and (target state) the shared beads remote. One per fleet. |
| **Commodore** | The AI voice persona at Fleet Command that the Admiral speaks with. Whisper.cpp STT → LLM → Kokoro TTS. When the Admiral says "wake up the captains," the Commodore processes and dispatches. Identity, not a separate component. |
| **Ship** | One machine running a shipmates captain + crew. Ships dial *out* to Fleet Command — no inbound ports, NAT-safe. Each ship has its own git checkout and its own local crew. |
| **Captain** | The coordinating persona/session on a ship. One captain per ship per repo. (Formerly "lead.") |
| **Mate** | A crew persona process on a ship — interactive (PTY-hosted) or headless (stream-json). |
| **Beads** | The shared, versioned task/decision graph (Dolt-backed) that all ships push/pull. The fleet's structured memory. |

## Where feat/fleet-command-rename is TODAY

The transport and control plane already work end-to-end:

```
   ship (captain process)                     Fleet Command
   ──────────────────────                     ─────────────
   local server :port  ◄──── proxied ────  /api/captain/{key}/feed
        │                    API calls     /api/captain/{key}/events
        │                                  /api/captain/{key}/pending
        └── outbound websocket ─────────►  /connect (remotedialer)
            X-Shipmates-Identity                │
            X-Shipmates-Repo                    ├── captain registry (in-mem + SQLite)
            Bearer token                        ├── event mirror (replay)
                                                ├── web UI (embedded, auth-gated)
                                                └── /api/conversation + /api/tts
                                                    (Commodore voice loop)
```

Implemented (`internal/fleet/`, `internal/server/fleet.go`):

- **Outbound tunnels** — captains dial Fleet Command over websocket
  (`rancher/remotedialer`); Fleet Command dials *back through the tunnel* to reach
  each captain's local `127.0.0.1` API. NAT/firewall-safe by construction.
  Reconnect loop with 5s backoff.
- **Auth** — shared bearer token on tunnel connect; session login for the web UI.
- **Captain registry** — identity headers (`repo`, `install-id`, `persona`, `port`),
  first-seen/last-seen, persisted to optional SQLite store.
- **Proxied control plane** — `feed`, `events`, `pending`, `tell`, `resolve` all reach
  any connected captain from Fleet Command. The CLI (`shipmates fleet ls/tail/tell/pending/resolve`)
  and the web UI consume the same JSON API.
- **Event mirroring** — SQLite store replays disconnected captains' history.
- **Commodore voice loop** — `/api/conversation` (LLM turn) + `/api/tts` (Kokoro) power
  the hands-free channel the Admiral uses to steer the fleet by voice.

This is the "multi-captain control" half of the fleet. What it does **not** yet have:

1. **No live terminals.** Fleet Command sees hook *events* (tool use, pending, replies)
   but not the mate's actual screen — no thinking text, no streamed narration, no scrollback.
2. **No per-mate status.** The UI has feeds, not herdr-style 🔴🟡🟢🔵 glanceable state.
3. **No shared memory.** Each ship's `.shipmates/memory/` is local. Two captains on
   two ships cannot see each other's decisions, pins, or rejected patterns. Coordination
   state (pins, verdicts, queues) lives in per-repo files and GitHub, not in the fleet.

## Target architecture

```
                        ┌─────────────────────────────────────────┐
                        │             FLEET COMMAND               │
                        │                                         │
                        │  web UI ─ xterm.js panes (per mate)     │
                        │         ─ status dots (hook-derived)    │
                        │         ─ pending approvals (inline)    │
                        │         ─ beads graph view (fleet-wide) │
                        │         ─ Commodore voice channel        │
                        │                                         │
                        │  remotedialer hub    beads remote        │
                        │  (existing)          (dolt sql-server)   │
                        └─────────┬──────────────────┬────────────┘
                     tunnels      │                  │  bd push/pull
              ┌───────────────────┼──────────────────┼───────────────┐
              ▼                   ▼                  ▼               ▼
      ┌──────────────┐    ┌──────────────┐    ┌──────────────┐
      │   SHIP A     │    │   SHIP B     │    │   SHIP C     │
      │ captain srv  │    │ captain srv  │    │ captain srv  │
      │  ├ mate: PTY │    │  ├ mate: PTY │    │  ├ headless  │
      │  │ (go-pty)  │    │  │           │    │  │ stream-   │
      │  ├ mate: PTY │    │  └ mate:     │    │  │ json      │
      │  └ hooks ────┤    │    headless  │    │  └ hooks     │
      │  bd (embed)  │    │  bd (embed)  │    │  bd (embed)  │
      └──────────────┘    └──────────────┘    └──────────────┘
```

Three additions to what exists, in dependency order:

### 1. PTY layer (herdr's kernel, in Go)

Each ship's captain server gains a **PTY host** for interactive mates:

- **Library:** `github.com/aymanbagabas/go-pty` — cross-platform, ConPTY-native on
  Windows, pure Go (no CGO). Same architectural shape as herdr's `portable-pty`.
- **Per-mate:** spawn `claude --agent <persona>` under a PTY; pump output into a
  ring buffer (~64KB backscroll) and fan out to subscribers.
- **Transport:** PTY frames ride the *existing* remotedialer tunnel — Fleet Command
  already dials back to the captain's local server; add a `GET /pty/{mate}/stream`
  (websocket) endpoint on the captain server and proxy it like `feed`/`events`.
- **Browser:** xterm.js in the Fleet Command UI renders the raw ANSI stream. Keystrokes
  flow back for steer-from-browser. Attach modes: read-only vs read-write.

Design decisions to make deliberately:

| Decision | Options | Lean |
|---|---|---|
| Backpressure | block PTY reader / drop-oldest per client | **drop-oldest** — never stall the agent for a slow browser tab |
| Resize policy | smallest-wins / owner-wins / sticky | **owner-wins** (the attach that spawned it sets size; viewers reflow) |
| Multi-viewer input | free-for-all / single-writer lock | **single-writer** with explicit takeover |

### 2. Status dots (better than herdr's)

Herdr derives 🔴🟡🟢🔵 from PTY-output *heuristics* (process-name + screen-scraping).
Shipmates already has the superior signal: **Claude Code hooks** stream structured
events into the captain server.

| Dot | Derivation (all from existing hook events) |
|---|---|
| 🔴 blocked | mate has an entry in the `pending` approval queue |
| 🟡 working | hook event seen < N seconds ago and nothing pending |
| 🟢 idle | no hook events for > N seconds, process alive |
| 🔵 done | mate process exited / drain completed |

No parsing, no heuristics — a fold over the event stream Fleet Command already mirrors.
Ship it as a computed field on `/api/captains` (per-mate sub-records) so the CLI and
web UI get it for free. The 🔴 dot in the UI opens the pending-approval panel inline —
the *what is it asking* + allow/deny, which herdr structurally cannot do.

### 3. Beads as fleet memory

Adopt [beads](https://github.com/gastownhall/beads) (`bd`, Dolt-backed) as the
structured shared-state substrate. Fleet Command hosts (or fronts) the Dolt remote;
every ship's `bd` embedded DB pushes/pulls against it.

**What moves into beads** (structured, mergeable, queryable):

| Today | Becomes |
|---|---|
| `pinned-for-human-<persona>.md` files | `type: pin` beads — `status: blocked`, `assignee: operator`, body = pin markdown |
| Verdict comments parsed from GitHub | `type: verdict` sub-beads replying to a PR bead |
| Per-repo issue/PR mental model | bead graph linked via bd's native `external_ref` (`gh-<n>` or full URL), epic → task → implementation hierarchy |
| Rejected patterns buried in memory *.md | `type: pattern, status: rejected` beads, surfaced by `bd prime` on session start |
| "which ship owns which work" (nowhere) | `ship_id` column on beads |

**What stays where it is:**

- **Prose memory** (`.shipmates/memory/<persona>/*.md`) — narrative, worked examples,
  rationale essays. Link to beads via a `bead_ref:` frontmatter field.
- **Standup/activity logs** — time-series; plain SQL tables beside beads, not bead nodes.
- **PTY streams** — fire-and-forget bytes; wrong shape for a graph store.

**Phase-4 design (validated against bd v1.1.0 hands-on):**

- **Mates are beads-native, not shipmates-mediated.** bd ships its own Claude
  Code hook integration — `bd prime` auto-injects workflow context when a
  beads workspace resolves, and its session-close protocol tells the agent to
  `bd close` finished work. Shipmates does NOT shadow-write every feed event
  into beads (a "you alive?" tell is not a task); mates create/close their own
  work beads. Shipmates' jobs are the plumbing around that:
  1. **Detect** — a `.beads/` dir in the project marks the ship beads-enabled.
  2. **Prime injection** — live/PTY mates run `claude -p`, which never fires
     SessionStart hooks, so bd's own auto-prime can't trigger. The captain server
     runs `bd prime` at spawn and passes it via `--append-system-prompt`.
  3. **Surface** — `GET /beads.json` on the captain shells `bd list --json`;
     Fleet Command proxies it per ship for a read-only graph view.
  4. **Sync** (phase 5) — `bd dolt push/pull` heartbeat against a dedicated
     fleet remote.
- **bd init gotcha:** it auto-configures `sync.remote` from the enclosing git
  repo's origin — inside a source checkout that aims bead pushes at the source
  repo. Shipmates' beads-enable step must set (or clear) `sync.remote`
  explicitly; never trust the auto-detected one.
- **Useful bd affordances observed:** `--json` on create/show/list; `bd dep add
  <dependent> <dependency>`; comments (`comment_count`) for threading;
  `events-export` (audit trail JSONL); no-db JSONL-only mode; embedded
  dolt sql-server auto-starts per project.

**Sync model (landed):** embedded `bd` per ship (single-writer, file-locked) +
`bd dolt push/pull` against the shared remote. Two tiers:

- **Real-time nudge** — the captain watches the workspace signature (content of
  the dolt noms manifest, NOT file mtimes — journal writes churn those — and
  NOT `.beads/last-touched` — bd updates it on `show`, so reads would false-
  trigger) on a 3s poll; Fleet Command-mediated writes mark dirty directly. On a
  local write: pull, push, then POST `/api/beads/nudge` to Fleet Command, which
  fans `POST /beads/pull` out to every other connected ship through the
  tunnels. Cross-ship propagation measured at ~4s. The sync loop re-baselines
  the signature after its own pull/push so sync-induced changes never
  self-trigger (no nudge storms).
- **3-minute heartbeat** — unchanged, now the safety net under the nudge
  (failed pushes, missed watches, ships that were offline).

Hash-based bead IDs make concurrent multi-ship writes merge-safe (Dolt
cell-level merge).

**Bead reassignment as cross-ship dispatch (landed):** assigning a bead to
`persona@ship` from Fleet Command MOVES the work there. `POST
/api/captain/{key}/bead/{id}/assign {ship, persona}` runs three tunnel calls in
sequence: update the assignee on the carrying ship → synchronous
`/beads/pull?wait=1` on the target ship (the bead must exist locally before
anyone references it) → `/tell/{persona}` with a dispatch message (`bd show
<id>`, claim, work, close). The tell path spawns the mate if it's asleep, so
dispatch also wakes the crew; the mate's first `bd show` lands in the normal
permission gate. UI: an `assign to… / dispatch` picker in the bead detail,
built from the fleet-wide roster.

**Schema extensions** beyond stock beads (promote from metadata JSON to columns for
dashboard query speed): `ship_id`, `verdict_state`, `surface` (file-glob
scope for decisions/patterns), `session_ref` (Claude session that wrote it).

**gh ↔ beads seam (landed):** no custom column needed — bd v1.1.0 already has a
first-class `external_ref` field (`--external-ref` on create/update, round-trips
through `list/show --json`). Convention: `gh-<n>` for this repo's issue/PR n, or
a full URL for anything else. The github routing template gains a beads section
(rendered only when `.beads/` exists) teaching the crew to stamp decomposed
beads with the originating issue and to treat bead ids as context capsules for
dispatch. The captain sends its git-origin browse URL (`X-Shipmates-Repo-URL`)
on tunnel connect; the Fleet Command UI resolves `gh-<n>` refs against it for
clickable links (`/issues/<n>` — GitHub redirects to `/pull/<n>` where the ref
is a PR).

## Ship supervisor (landed)

One daemon per host keeps that host's captains alive: `shipmates ship serve`
reads `~/.shipmates/ship.yaml` and runs a captain server per project dir,
restarting on crash (exponential backoff 1s→60s, reset after 5 min healthy).
If a healthy server already runs in a dir (started manually), the supervisor
stands by and re-probes instead of racing it. Captain output appends to each
project's `.shipmates/sessions/server.log`; on supervisor shutdown captains get
a graceful `/shutdown` (crew reaped) before the hard kill.

```yaml
# ~/.shipmates/ship.yaml
env:                                   # host-level, applied to every captain
  SHIPMATES_FLEET_TOKEN: ${HOMELAB_FLEET_TOKEN}   # env-indirection: name the
projects:                              # source, never store the secret
  - dir: C:/Users/you/project-one
    env:                               # per-project overrides
      SHIPMATES_FLEET_TOKEN: ${PROJECT_ONE_TOKEN}
```

`shipmates ship install` wires it to run at logon — **Windows: Scheduled Task
(ONLOGON), macOS: launchd user agent** (`~/Library/LaunchAgents/cc.shipmates.ship.plist`,
KeepAlive). Deliberately NOT a session-0 Windows service or launch daemon:
claude needs the user's environment, credentials, and profile. `ship uninstall`
reverses it. Linux: run `ship serve` from a systemd *user* unit.

### The backend driver seam: `backend: claude|command`

The operator's `~/.shipmates/personas.yaml` may declare a foreign agent:

```yaml
personas:
  opencode:
    backend: command
    command: [opencode]
```

Not persona frontmatter and not a `crew:` override: both arrive with `git
clone`, and a checkout that could name the argv shipmates spawns would be
arbitrary code execution. A persona file that tries it is refused, loudly —
see [docs/security.md](security.md#persona-execution-config-is-operator-owned).

Command-backed mates are **PTY-only**: `ensurePTY` spawns the argv under a
PTY instead of claude — no session resume, no hooks, no beads prime. Their
status dots derive from screen activity (pumpPTY already feeds lastSeen).
Headless surfaces (`ask`, `drain`, tell-to-headless) reject them with a
pointed error; a tell while their terminal is open still works (it's typed
into the PTY like any other).

## Execution modes — the two mate flavors

| Flavor | Backend | What Fleet Command shows | Herdr equivalent |
|---|---|---|---|
| **Interactive** | PTY (`go-pty`) | xterm.js pane — thinking, narration, full screen | Yes (its only mode) |
| **Headless** | `claude -p --output-format stream-json` over a pipe | Structured timeline: thinking blocks, tool calls, results, cost/model/timing as UI elements | **No — cannot exist in herdr** |

Headless mates are shipmates' unfair advantage: `stream-json` emits `thinking`,
`tool_use`, `tool_result`, and `result` (cost/duration) as parseable JSON — no ANSI, no
PTY, deterministic. `drain`, `fanout`, and CI dispatch should all use this mode and get
first-class structured rendering in the Fleet Command UI. Guard the decoder behind a
version-tagged interface (`internal/fleet/streamjson/`) — the format is Anthropic's
contract and can drift.

## What we deliberately are NOT building

| Concern | Position |
|---|---|
| tmux/screen compatibility | No. The PTY host is in-process; herdr proved the model. |
| Non-Claude agents (Devin, Aider, Copilot CLI) | Not v1. Hooks + stream-json are Claude-specific; PTY-heuristic status for foreign agents is a later, separate effort. |
| Our own replication protocol | No. Dolt push/pull *is* the replication layer. If beads/Dolt doesn't fit, reassess — don't hand-roll. |
| Fleet Command-hosted code execution | No. Ships run the mates; Fleet Command routes, renders, and remembers. |

## Phasing

Each phase lands independently and is useful without the ones after it.

**Phase 1 — status dots.** Pure fold over existing hook events. New computed per-mate
status in `/api/captains`; dots in the web UI; 🔴 opens pending panel. No new deps.
*Small; do first.*

**Phase 2 — PTY panes.** `go-pty` host in the captain server, ring buffer, websocket
stream endpoint, tunnel proxy, xterm.js in the UI. Read-only attach first;
keystrokes + single-writer lock second. *The herdr-parity milestone.*

**Phase 3 — headless timeline (landed 2026-07-05).** stream-json decoder
(`internal/streamjson`, version-guarded: all format knowledge lives there,
unknown types decode to nothing) + type-aware feed rendering: thinking and
tool results collapsed-with-tap-to-expand, tool hooks as compact chips, and
per-turn cost/duration/model on result events. tool_use items from the stream
are deliberately dropped — the PreToolUse hook already records every call.
*The beyond-herdr milestone.*

**Phase 4 — beads, single ship.** `bd` embedded on one ship; pins/verdicts/patterns
shadow-written to beads; `bd prime` wired into persona session start; Fleet Command
reads the graph read-only. Prove the schema before federating.

**Phase 5 — beads, fleet.** Dolt remote on/behind Fleet Command; push/pull heartbeat +
tunnel-nudged pulls; cross-ship graph view; bead reassignment as cross-ship dispatch.

## Open questions

- **Fleet Command HA / single point of failure.** Ships keep working when Fleet
  Command is down (tunnels reconnect; beads queue local writes) — but is Fleet
  Command's SQLite mirror + Dolt remote worth backing up as one unit?
- **Mate lifetime vs ship-daemon lifetime.** A PTY dies with the captain server
  process. Split a minimal PTY-supervisor out of the captain server (tmux-style
  tiny daemon) or accept mate loss on captain restart? Lean: accept for v1;
  supervisor later if it hurts.
- **Beads schema ownership.** Extending beads' schema means owning migrations alongside
  upstream's. Vendor the schema (fork-friendly) or keep extensions purely in metadata
  JSON until the queries prove they need columns?
