# Fleet architecture — bridge + ships + shared memory

> Working doc for the `feat/bridge` direction. Where the bridge is today, where it's going,
> and the gap between. Companion to `architecture.md` (persona/memory fundamentals).
>
> Design inputs: [herdr](https://github.com/ogulcancelik/herdr) (terminal multiplexing model),
> [beads](https://github.com/gastownhall/beads) (distributed graph memory), and the existing
> shipmates lead↔crew loop.

## The one-paragraph vision

Multiple **ships** (machines — a laptop, a homelab box, a cloud VM) each run shipmates
crews working on code. A single **bridge** gives the captain one pane of glass across
all of them: live terminals for every mate, red/yellow/green/blue status per mate,
inline approval of risky tools, and a shared task/memory graph so a lead on Ship A
knows what the security mate on Ship B learned yesterday. Think *herdr's UX* +
*shipmates' personas and memory* + *beads' federated graph* in one product.

## Vocabulary

| Term | Definition |
|---|---|
| **Bridge** | The central rendezvous server (`shipmates bridge serve`). Owns the web UI, auth, the lead registry, and (target state) the shared beads remote. One per fleet. |
| **Ship** | One machine running a shipmates lead + crew. Ships dial *out* to the bridge — no inbound ports, NAT-safe. |
| **Lead** | The coordinating persona/session on a ship (existing concept). One lead per ship per repo. |
| **Mate** | A crew persona process on a ship — interactive (PTY-hosted) or headless (stream-json). |
| **Beads** | The shared, versioned task/decision graph (Dolt-backed) that all ships push/pull. The fleet's structured memory. |

## Where feat/bridge is TODAY

The transport and control plane already work end-to-end:

```
   ship (lead process)                        bridge
   ───────────────────                        ──────
   local server :port  ◄──── proxied ────  /api/lead/{key}/feed
        │                    API calls     /api/lead/{key}/events
        │                                  /api/lead/{key}/pending
        └── outbound websocket ─────────►  /connect (remotedialer)
            X-Shipmates-Identity                │
            X-Shipmates-Repo                    ├── lead registry (in-mem + SQLite)
            Bearer token                        ├── event mirror (replay)
                                                ├── web UI (embedded, auth-gated)
                                                └── /api/conversation + /api/tts
                                                    (Ollama voice-operator loop)
```

Implemented (`internal/bridge/`, `internal/server/bridge.go`):

- **Outbound tunnels** — leads dial the bridge over websocket (`rancher/remotedialer`);
  the bridge dials *back through the tunnel* to reach each lead's local `127.0.0.1` API.
  NAT/firewall-safe by construction. Reconnect loop with 5s backoff.
- **Auth** — shared bearer token on tunnel connect; session login for the web UI.
- **Lead registry** — identity headers (`repo`, `install-id`, `persona`, `port`),
  first-seen/last-seen, persisted to optional SQLite store.
- **Proxied control plane** — `feed`, `events`, `pending`, `tell`, `resolve` all reach
  any connected lead from the bridge. The CLI (`shipmates bridge ls/tail/tell/pending/resolve`)
  and the web UI consume the same JSON API.
- **Event mirroring** — SQLite store replays disconnected leads' history.

This is the "multi-lead control" half of the fleet. What it does **not** yet have:

1. **No live terminals.** The bridge sees hook *events* (tool use, pending, replies) but
   not the mate's actual screen — no thinking text, no streamed narration, no scrollback.
2. **No per-mate status.** The UI has feeds, not herdr-style 🔴🟡🟢🔵 glanceable state.
3. **No shared memory.** Each ship's `.shipmates/memory/` is local. Two leads on two
   ships cannot see each other's decisions, pins, or rejected patterns. Coordination
   state (pins, verdicts, queues) lives in per-repo files and GitHub, not in the fleet.

## Target architecture

```
                        ┌─────────────────────────────────────────┐
                        │                BRIDGE                   │
                        │                                         │
                        │  web UI ─ xterm.js panes (per mate)     │
                        │         ─ status dots (hook-derived)    │
                        │         ─ pending approvals (inline)    │
                        │         ─ beads graph view (fleet-wide) │
                        │                                         │
                        │  remotedialer hub    beads remote        │
                        │  (existing)          (dolt sql-server)   │
                        └─────────┬──────────────────┬────────────┘
                     tunnels      │                  │  bd push/pull
              ┌───────────────────┼──────────────────┼───────────────┐
              ▼                   ▼                  ▼               ▼
      ┌──────────────┐    ┌──────────────┐    ┌──────────────┐
      │   SHIP A     │    │   SHIP B     │    │   SHIP C     │
      │ lead server  │    │ lead server  │    │ lead server  │
      │  ├ mate: PTY │    │  ├ mate: PTY │    │  ├ headless  │
      │  │ (go-pty)  │    │  │           │    │  │ stream-   │
      │  ├ mate: PTY │    │  └ mate:     │    │  │ json      │
      │  └ hooks ────┤    │    headless  │    │  └ hooks     │
      │  bd (embed)  │    │  bd (embed)  │    │  bd (embed)  │
      └──────────────┘    └──────────────┘    └──────────────┘
```

Three additions to what exists, in dependency order:

### 1. PTY layer (herdr's kernel, in Go)

Each ship's lead server gains a **PTY host** for interactive mates:

- **Library:** `github.com/aymanbagabas/go-pty` — cross-platform, ConPTY-native on
  Windows, pure Go (no CGO). Same architectural shape as herdr's `portable-pty`.
- **Per-mate:** spawn `claude --agent <persona>` under a PTY; pump output into a
  ring buffer (~64KB backscroll) and fan out to subscribers.
- **Transport:** PTY frames ride the *existing* remotedialer tunnel — the bridge
  already dials back to the lead's local server; add a `GET /pty/{mate}/stream`
  (websocket) endpoint on the lead server and proxy it like `feed`/`events`.
- **Browser:** xterm.js in the bridge UI renders the raw ANSI stream. Keystrokes
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
events into the lead server.

| Dot | Derivation (all from existing hook events) |
|---|---|
| 🔴 blocked | mate has an entry in the `pending` approval queue |
| 🟡 working | hook event seen < N seconds ago and nothing pending |
| 🟢 idle | no hook events for > N seconds, process alive |
| 🔵 done | mate process exited / drain completed |

No parsing, no heuristics — a fold over the event stream the bridge already mirrors.
Ship it as a computed field on `/api/leads` (per-mate sub-records) so the CLI and web
UI get it for free. The 🔴 dot in the UI opens the pending-approval panel inline —
the *what is it asking* + allow/deny, which herdr structurally cannot do.

### 3. Beads as fleet memory

Adopt [beads](https://github.com/gastownhall/beads) (`bd`, Dolt-backed) as the
structured shared-state substrate. The bridge hosts (or fronts) the Dolt remote;
every ship's `bd` embedded DB pushes/pulls against it.

**What moves into beads** (structured, mergeable, queryable):

| Today | Becomes |
|---|---|
| `pinned-for-human-<persona>.md` files | `type: pin` beads — `status: blocked`, `assignee: captain`, body = pin markdown |
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
     SessionStart hooks, so bd's own auto-prime can't trigger. The lead server
     runs `bd prime` at spawn and passes it via `--append-system-prompt`.
  3. **Surface** — `GET /beads.json` on the lead shells `bd list --json`; the
     bridge proxies it per ship for a read-only graph view.
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

**Sync model:** embedded `bd` per ship (single-writer, file-locked) + `bd dolt push/pull`
against the bridge's remote on a heartbeat (post-write hook + N-second poll). Hash-based
bead IDs make concurrent multi-ship writes merge-safe (Dolt cell-level merge). Real-time
nudge: bridge broadcasts "pull now" over the existing tunnel when it sees new pushes.

**Schema extensions** beyond stock beads (promote from metadata JSON to columns for
dashboard query speed): `ship_id`, `verdict_state`, `surface` (file-glob
scope for decisions/patterns), `session_ref` (Claude session that wrote it).

**gh ↔ beads seam (landed):** no custom column needed — bd v1.1.0 already has a
first-class `external_ref` field (`--external-ref` on create/update, round-trips
through `list/show --json`). Convention: `gh-<n>` for this repo's issue/PR n, or
a full URL for anything else. The github routing template gains a beads section
(rendered only when `.beads/` exists) teaching the crew to stamp decomposed
beads with the originating issue and to treat bead ids as context capsules for
dispatch. The lead sends its git-origin browse URL (`X-Shipmates-Repo-URL`) on
tunnel connect; the bridge UI resolves `gh-<n>` refs against it for clickable
links (`/issues/<n>` — GitHub redirects to `/pull/<n>` where the ref is a PR).

## Execution modes — the two mate flavors

| Flavor | Backend | What the bridge shows | Herdr equivalent |
|---|---|---|---|
| **Interactive** | PTY (`go-pty`) | xterm.js pane — thinking, narration, full screen | Yes (its only mode) |
| **Headless** | `claude -p --output-format stream-json` over a pipe | Structured timeline: thinking blocks, tool calls, results, cost/model/timing as UI elements | **No — cannot exist in herdr** |

Headless mates are shipmates' unfair advantage: `stream-json` emits `thinking`,
`tool_use`, `tool_result`, and `result` (cost/duration) as parseable JSON — no ANSI, no
PTY, deterministic. `drain`, `fanout`, and CI dispatch should all use this mode and get
first-class structured rendering in the bridge UI. Guard the decoder behind a
version-tagged interface (`internal/bridge/streamjson/`) — the format is Anthropic's
contract and can drift.

## What we deliberately are NOT building

| Concern | Position |
|---|---|
| tmux/screen compatibility | No. The PTY host is in-process; herdr proved the model. |
| Non-Claude agents (Devin, Aider, Copilot CLI) | Not v1. Hooks + stream-json are Claude-specific; PTY-heuristic status for foreign agents is a later, separate effort. |
| Our own replication protocol | No. Dolt push/pull *is* the replication layer. If beads/Dolt doesn't fit, reassess — don't hand-roll. |
| Bridge-hosted code execution | No. Ships run the mates; the bridge routes, renders, and remembers. |

## Phasing

Each phase lands independently and is useful without the ones after it.

**Phase 1 — status dots.** Pure fold over existing hook events. New computed per-mate
status in `/api/leads`; dots in the web UI; 🔴 opens pending panel. No new deps.
*Small; do first.*

**Phase 2 — PTY panes.** `go-pty` host in the lead server, ring buffer, websocket
stream endpoint, tunnel proxy, xterm.js in the UI. Read-only attach first;
keystrokes + single-writer lock second. *The herdr-parity milestone.*

**Phase 3 — headless timeline.** stream-json decoder + structured timeline UI for
`drain`/`fanout` mates. Cost/duration surfaced per run. *The beyond-herdr milestone.*

**Phase 4 — beads, single ship.** `bd` embedded on one ship; pins/verdicts/patterns
shadow-written to beads; `bd prime` wired into persona session start; bridge reads the
graph read-only. Prove the schema before federating.

**Phase 5 — beads, fleet.** Dolt remote on/behind the bridge; push/pull heartbeat +
tunnel-nudged pulls; cross-ship graph view; bead reassignment as cross-ship dispatch.

## Open questions

- **Bridge HA / single point of failure.** Ships keep working when the bridge is down
  (tunnels reconnect; beads queue local writes) — but is the bridge's SQLite mirror +
  Dolt remote worth backing up as one unit?
- **Mate lifetime vs ship-daemon lifetime.** A PTY dies with the lead server process.
  Split a minimal PTY-supervisor out of the lead server (tmux-style tiny daemon) or
  accept mate loss on lead restart? Lean: accept for v1; supervisor later if it hurts.
- **Beads schema ownership.** Extending beads' schema means owning migrations alongside
  upstream's. Vendor the schema (fork-friendly) or keep extensions purely in metadata
  JSON until the queries prove they need columns?
