# Handoff — feat/bridge fleet work (as of 2026-07-05)

> Context dump for whoever picks this branch up. The design rationale lives in
> [`fleet-architecture.md`](fleet-architecture.md); this file is what's DONE,
> what's RUNNING, and what's NEXT. Everything below landed on `feat/bridge`
> between 2026-07-02 and 2026-07-05 (commits `bde07d1..885749e`).

## The one-paragraph story

Shipmates grew from "personas with memory + a CLI" into a working fleet
platform: a central **bridge** (web UI at bridge.tricorder.cc via cloudflared)
with live status dots, real PTY terminals in the browser, per-agent
conversation tabs, permission approvals, and a **beads** (bd/Dolt) work graph
that syncs across ships. Two smoke ships run on the captain's PC. The real
deployment target is **card-cannon** with a **Mac mini as the iOS/Mac lead**
running its own crew.

## What's built and working (all pushed)

### Bridge + lead server core
- Status dots per mate (blocked/working/idle/done/off) derived from hook
  events + pending queue — `/status.json` per lead, `/api/status` fleet-wide.
- PTY terminals: `go-pty` (ConPTY on Windows), 64KB ring backscroll, DEC
  private-mode latching replayed on attach (tmux trick — wheel/paste modes
  survive late attach), SSE streaming through the tunnel, xterm.js + fit
  addon in the UI. Multi-terminal with tabs (`persona@ship`), full-screen
  takeover on all devices.
- One long-term session per shipmate (`project.SessionLaunch`) shared by
  ask/tell/term. Single-attachment rule: term takeover retires the headless
  proc (with a grace-period spinner + "attach now"/"cancel" if mid-turn);
  tells while a term is open are typed INTO the terminal (bracketed paste).
- Permission model: headless mates keep the blocking PreToolUse gate (bridge
  pending pane, grouped by mate with allow/deny-all + expandable details);
  PTY mates run observe-only hooks and use claude's native in-terminal y/n.
- Bridged leads never idle-exit — after 1h idle they reap crew but keep the
  tunnel, so the ship is always wakeable from the bridge.
- Mobile UX pass: drawer nav, ship-picker landing screen, visualViewport
  keyboard handling, hold-to-scroll arrows, roster dropdown for tell targets,
  lead-first ordering everywhere, tell "all" broadcast (client-side fanout).

### Beads (phases 4 + 5 of the fleet doc)
- bd v1.1.0 (`~/bin/bd.exe`). Mates are beads-native: `bd prime` output is
  injected at every mate spawn via `--append-system-prompt` (headless -p
  sessions never fire SessionStart, so bd's own auto-prime can't run there).
- Lead server: `GET /beads.json` (bd list), `GET /bead/{id}` (bd show,
  allow-list validated id), 3-minute pull+push sync heartbeat when a
  `sync.remote` is configured.
- Bridge: per-ship proxy + `GET /api/beads` fleet aggregate (dedupe by bead
  id, ship attribution). UI: ⛃ beads button — per-ship view when a ship is
  selected, fleet-wide view from the landing screen; rows expand for the full
  record.
- Cross-ship sync PROVEN: two ships share one graph over a `file://` Dolt
  remote; bead created on either converges to both.

### Positioning decisions (argued out with the captain, don't relitigate casually)
- **gh issues = human-facing contract layer; beads = agent coordination
  layer.** Lead decomposes a gh issue into beads (`external_id: gh:issues/N`
  convention — NOT yet implemented); bead ids act as context capsules for
  subagent dispatch.
- Three memory layers, three jobs: Claude sessions (conversation), markdown
  `.shipmates/memory/` (persona prose knowledge), beads (structured work).
  `bd remember` overlaps markdown memory — unresolved, let usage decide.
- Terminal is a full-screen mode, not a split pane. Lead is the front door
  of every ship (selecting a ship opens the lead conversation).

## The running demo environment (captain's PC)

- Bridge: `127.0.0.1:18443`, token in `tmp/smoke/bridge.token`, exposed as
  https://bridge.tricorder.cc via cloudflared (`~/.cloudflared/config.yml`
  ingress, tunnel process must be running).
- Ships: `tmp/smoke/proj` (laptop: lead/security/tester, beads workspace) and
  `tmp/smoke/proj2` (homelab: lead/backend/frontend, beads bootstrapped from
  the shared remote). Binary: `tmp/smoke/shipmates.exe`.
- Shared beads remote: `file://C:/Users/luthe/shipmates/tmp/smoke/beads-remote-dolt`.
- Restart pattern (used ~40 times, copy from shell history or):
  build → kill shipmates procs → move new exe → start bridge with
  `--token-file` → start `server serve` in each proj dir with
  `SHIPMATES_BRIDGE_TOKEN` set → health-check.
- UI assets are cache-busted with `?v=N` in index.html — **bump N on every
  UI change** (Cloudflare edge-caches .js/.css by extension; origin also
  sends no-cache now, but the ?v bump is the belt to those suspenders).

## Hard-won gotchas (each cost a debugging session)

1. **Bridge tunnel proxy must decode chunked encoding** — Go chunks responses
   >2KB; the old hand-rolled reader glued chunk framing into JSON and the
   feed stream silently starved. Fixed with `http.ReadResponse`. If a proxied
   endpoint "works small, breaks big," think of this.
2. **Event-stream watermark is an INDEX, not a timestamp** — second-granularity
   timestamps dropped all-but-first of same-second batches (broadcast tells).
3. **Auth allow-list vs Cloudflare**: any asset URL answering with login HTML
   gets edge-cached and served to authenticated users as JS. `/app.js`,
   `/vendor/*` are public; keep it that way.
4. **bd init auto-configures sync.remote from the enclosing git origin** —
   correct for a real project (data rides `refs/dolt/data`, invisible to
   branches), WRONG when the dir sits inside an unrelated checkout (the smoke
   sandbox inherited the shipmates source repo's origin — neutralized).
5. **Second-machine beads order: configure remote → `bd bootstrap`. NEVER
   `bd init` first** — independent init = unrelated Dolt history ("no common
   ancestor") + a workspace-identity mismatch that `bd doctor --fix` did not
   fix in embedded mode (manual `metadata.json` project_id alignment worked).
6. **go-pty resolves bare command names relative to Dir** — always
   `exec.LookPath` first.
7. **iOS**: tabs freeze EventSource into zombies (reopen on visibilitychange),
   keyboard overlays without resizing (pin to visualViewport), inputs <16px
   auto-zoom (maximum-scale=1 + don't autofocus on coarse pointers).

## Not built yet (the queue, in intended order)

1. ~~gh ↔ beads seam~~ — DONE (2026-07-05). Landed on bd's NATIVE
   `external_ref` field (`gh-<n>` or full URL) instead of the planned
   `external_id: gh:issues/N` metadata convention — bd already had the flag
   and it round-trips through `list/show --json`. Routing template grows a
   beads section when `.beads/` exists; leads send `X-Shipmates-Repo-URL`
   (git origin, normalized) so the UI linkifies refs.
2. ~~Ship supervisor~~ — DONE (2026-07-05). `shipmates ship serve|install|
   uninstall`, `~/.shipmates/ship.yaml` with `${VAR}` env-indirection,
   restart-on-crash with backoff, stands by when a healthy server already
   runs in a dir. Windows Scheduled Task / macOS launchd user agent. The
   `backend: claude|command` driver seam landed too (PTY-only foreign mates).
   Design now actually written in fleet-architecture.md.
3. **Card-cannon rollout** — the real deployment: `bd init` in the card-cannon
   checkout (its origin IS the correct sync remote there), PC ship + Mac mini
   ship (iOS/Mac crew), both registered with the bridge. Supervisor now
   exists, so the mini is unblocked.
4. ~~Polish backlog~~ — DONE (2026-07-05): live beads refresh (5s poll,
   expanded rows survive), bead create/close from the bridge, single-writer
   terminal lock with explicit takeover, and a ＋ roster menu in the term
   pane (was a dead end — could close tabs but not open them). SQLite feed
   replay still deliberately skipped.

## Where things live

- `internal/server/` — lead server: status.go, ptyproc.go (+ mode latching),
  ring.go, beads.go, sessionlaunch consumers. Tests: status, ring, ptymodes.
- `internal/bridge/` — bridge: bridge.go (routes, proxy, stream), auth.go,
  status.go / beads.go (aggregates), ptyproxy.go, ui/ (vanilla JS, no build).
- `internal/project/sessionlaunch.go` — shared session identity resolution.
- `docs/fleet-architecture.md` — the design doc, phases 1-5 + open questions.
- Voice conversation mode: LANDED (2026-07-05, un-parked from the stashes).
  `/conversation.html` — mic captures 16kHz WAV via Web Audio (NOT the flaky
  Web Speech API that got it parked), `/api/stt` proxies to a whisper.cpp
  server (`~/.shipmates/whisper/`, run: `Release/whisper-server.exe -m
  ggml-small.en-q5_1.bin --port 8321`), `/api/conversation` is an Ollama
  tool-loop driving the fleet, `/api/tts` speaks replies via Edge neural
  voices (pure Go websocket). Bridge flags: --ollama-url --ollama-model
  --ollama-cpu --tts-voice --stt-url. --ollama-cpu is REQUIRED on the
  captain's PC (GT 1030's CUDA toolchain is unsupported; GPU path 500s).
