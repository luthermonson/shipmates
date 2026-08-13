# Shipmates

**A fleet of AI coding crews that remember, coordinated from one place.**

Shipmates lets you run persona-driven Claude Code sessions across one or many machines, with persistent per-project memory, a shared task graph, a browser-native voice interface, and a real permission model. Two weeks in, the architect reviewing your PR doesn't just see the diff — it sees the diff against a remembered project: *"This re-introduces the pattern we rejected in #45 because X — has anything changed?"*

That memory layer alone was Shipmates' original pitch. Fleet Command adds a second dimension: coordinate multiple ships (machines) from a single browser, dispatch work across them, and talk to your fleet by voice through an AI officer called the Commodore.

## How it works

Shipmates uses a naval hierarchy that maps 1:1 to real roles:

```
                          Admiral (you)
                               │
                    voice ─────┼───── clicks / CLI
                               ▼
          ┌───────────────────────────────────────┐
          │            Fleet Command              │
          │                                       │
          │    Commodore ── dispatches ──► Web UI │
          │    AI officer                    │    │
          │                                  ▼    │
          │           Shared task graph           │
          │        (beads / Dolt on git refs)     │
          └───────────────────────────────────────┘
                  ▲                       ▲
                  │                       │
           outbound WebSocket        bd sync via
           tunnels (NAT-safe)         git refs
                  │                       │
                  └───────────┬───────────┘
                              │
                              ▼

    Ships — one per project. Any number of ships can
    run on the same machine (this workstation runs 4-5).

          ┌───────────────────────────────────────┐
          │  project-one                          │
          │    Captain ── ► mates                 │
          │      architect, security, frontend,   │
          │      backend, tester                  │
          └───────────────────────────────────────┘

          ┌───────────────────────────────────────┐
          │  project-two                          │
          │    Captain ── ► mates                 │
          │      (a smaller crew)                 │
          └───────────────────────────────────────┘

          ┌───────────────────────────────────────┐
          │  ephpm                                │
          │    Captain ── ► mates                 │
          └───────────────────────────────────────┘

          ┌───────────────────────────────────────┐
          │  ... one ship per project you run     │
          │      on any number of machines        │
          └───────────────────────────────────────┘
```

- **Admiral** — you, the human operator, giving strategic direction
- **Commodore** — the AI voice assistant at Fleet Command that executes your orders across the fleet
- **Fleet Command** — the coordinator node (one per fleet) that hosts the web UI, voice loop, and shared task graph
- **Ship** — a physical machine (or VM) running a ship server; each ship has its own project checkout
- **Captain** — the per-ship lead persona that coordinates that ship's crew and holds the ship's identity
- **Mate** — an individual AI coding agent under a captain (architect, security, frontend, etc.)

Ships open **outbound WebSocket tunnels** to Fleet Command, so nothing has to be exposed to the internet on the ship side — NAT-safe by design. Coordination between ships happens through a **shared task graph** ([beads](https://github.com/gastownhall/beads), a Dolt-backed SQL database that rides on hidden git refs in the project's own repo), so the fleet's work is versioned, merge-safe, and stored where you already trust it.

## Two ways to use it

**Solo dev on one machine** — skip Fleet Command entirely. Install shipmates, scaffold personas, use `shipmates ask`, `tell`, `open`, `feed`. Everything works standalone. This is the original pitch and it hasn't gone away.

**Distributed fleet** — install shipmates on multiple machines, run Fleet Command on one of them, and coordinate crews across the fleet from a single browser. Optionally add voice via the Commodore.

## Install

Download a prebuilt binary from the [latest release](https://github.com/luthermonson/shipmates/releases/latest) — pick the asset for your platform (macOS, Linux, Windows × amd64/arm64). The whole persona catalog is embedded in the binary, so a single executable is all you need.

```bash
# macOS / Linux
tar -xzf shipmates_*_*.tar.gz shipmates
sudo mv shipmates /usr/local/bin/
shipmates --version
```

On Windows, unzip `shipmates_<version>_windows_amd64.zip` and put `shipmates.exe` on your PATH.

With a Go toolchain (1.26+): `go install github.com/luthermonson/shipmates@latest`, or build from source with `go build -o shipmates .`.

Shipmates drives the [`claude` CLI](https://claude.com/claude-code), so make sure Claude Code is installed and authenticated.

## Quickstart — single ship

```bash
cd my-project
shipmates init --crew captain,architect,security,frontend,backend,tester
shipmates list
```

This vendors persona files into `.claude/agents/<name>.md` (Claude Code reads them natively — no new runtime), seeds each persona's memory at `.shipmates/memory/<name>/`, drops per-persona `policy.yaml` files into `.shipmates/policies/`, and writes `shipmates.yaml`. Personas write to their memory dir as they learn your project.

Memory is auto-loaded into every session via a Claude Code `SessionStart` hook wired into `.claude/settings.json` on `init` — the hook shells out to `shipmates hook load-memory`, which reads the persona's memory dir and injects the contents as session context. Deterministic: no dependence on the model choosing to read memory first. Reaches `ask`, `drain`, `drain-many`, `fanout`, and `open` on every startup and resume. (The live-server / PTY stream-json path loads memory separately via `--append-system-prompt`, unchanged.)

Use a persona three ways:

```bash
# 1. one-shot delegation (resumes the persona's session, so it remembers)
shipmates ask security "review the diff for auth regressions"

# 2. a live crew member you can talk to WHILE it works
shipmates tell security "double-check PR 10"
shipmates feed                 # watch its activity + replies

# 3. an interactive session as the persona
shipmates open captain
```

Or, inside any Claude Code session, invoke a persona via the Agent tool (e.g. "have security review the diff") or launch directly with `claude --agent security`.

## Quickstart — fleet coordination

On the coordinator machine:

```bash
# Start Fleet Command with a random token, optional voice pipeline
shipmates fleet serve \
  --addr 127.0.0.1:8443 \
  --token-file ~/.shipmates/fleet.token \
  --llm-backend claude-cli \
  --llm-model haiku \
  --stt-url http://127.0.0.1:8321/inference \
  --tts-url http://127.0.0.1:8322/v1/audio/speech \
  --tts-voice af_heart
```

Optionally expose Fleet Command via [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) so ships can find it from anywhere.

On each ship machine (workstation, laptop, VM, homelab box):

```bash
# One-time setup per project
cd my-project
shipmates init --crew captain,architect,security
# Edit shipmates.yaml: point the fleet block at Fleet Command
# fleet:
#   url: https://fleet.example.com

# Install as a persistent supervisor
shipmates ship install

# Or run manually
export SHIPMATES_FLEET_TOKEN=<same-token-fleet-command-uses>
shipmates server serve
```

Ships come online, open outbound WebSocket tunnels to Fleet Command, sync the beads graph from the project's git origin, and register their captain. In the browser at Fleet Command you'll see each captain appear in the sidebar, with a live PTY panel per mate, an event stream per captain, and a shared task-graph view.

## What Fleet Command does

**Single pane of glass across the fleet**

- Live PTY terminals for every mate on every ship — interact with a captain in Chicago from your phone in Barcelona
- Real-time event feed (assistant text, tool calls, results, permission requests)
- Approval queue for privileged tool calls, gate-able from CLI (`shipmates fleet resolve <id> allow|deny`) or the web UI
- Cross-ship dispatch — assign a bead to a captain on any ship, watch it appear there and start work

**Voice control via the Commodore**

Fleet Command's browser UI includes a mic button. Speak an order; the pipeline is:

1. Browser captures audio, streams to a **speech-to-text endpoint** (whisper.cpp or any OpenAI-compatible STT server)
2. Transcript goes to an **LLM backend** (a local Claude Code session, or any OpenAI-compatible endpoint like Ollama, LM Studio, vLLM)
3. The **Commodore** — an AI officer persona embedded in the LLM prompt — decides what fleet tools to call (list captains, tell captain, tell all captains, list beads, assign work, approve pending)
4. Response goes to a **text-to-speech endpoint** (Kokoro, Microsoft Edge neural voices, or OpenAI-compatible TTS)
5. Browser plays the reply

Everything is pluggable via OpenAI-compatible endpoints — you can run the whole voice stack locally on your GPU, or hybrid, or fully hosted. No lock-in.

**Attach binaries — photos, files, screenshots**

Drag a file into the Fleet Command UI (or use the 📷 Camera button on mobile). The bytes flow over the WebSocket tunnel to the target ship, land at `.shipmates/inbox/<uuid>.<ext>`, and the ship auto-tells the mate to look at it. The mate reads the file via Claude Code's own multi-modal Read tool — Shipmates never touches Anthropic's API.

Same thing works from CLI:

```bash
shipmates fleet show project-one:captain bug.png --caption "login redirect misaligned on iPhone"
```

## Permission model

Shipmates does not pass every tool call to the operator. It inherits Claude Code's existing permission model, layers fleet-scale policies on top, and gives the Admiral time-boxed overrides. Layers, in order of authority:

1. **Fleet-wide deny list** (`~/.shipmates/fleet-policy.yaml` on Fleet Command) — unconditional denies pushed to every ship. Un-overridable.
2. **Per-persona catalog policy** (`catalog/<persona>/policy.yaml`) — vendored per project, layered per persona
3. **Project `.claude/settings.local.json`** — individual dev overrides
4. **Project `.claude/settings.json`** — team-committed baseline
5. **User `~/.claude/settings.json`** — global defaults
6. **Read-only builtins** — `ls`, `cat`, `git status`, `git log`, `grep`, `find`, etc. always allowed

Precedence follows Claude Code's own rule: **deny in any layer beats allow in any layer.** Rule syntax matches Claude Code exactly — Bash globs with space-boundary semantics, gitignore paths for Read/Edit, domain patterns for WebFetch.

**Time-boxed approvals** — when you approve a Bash command, add `--for 30m` to auto-approve the same command for 30 minutes:

```bash
shipmates allow a1b2c3d4 --for 30m
```

Restart of the ship wipes time-boxes (fresh trust boundary).

**Session auto-repair** — if Claude Code loses track of a mate's session (jsonl rotated, upgrade cleaned the store), Shipmates detects the failure, deletes the stale session marker, and respawns fresh automatically. The operator sees a `session:auto-repair` event; the tell that triggered the recovery is delivered normally.

## Commands

### Persona / crew

| Command | What it does |
|---|---|
| `init [--crew a,b,c]` | scaffold `.shipmates/` + `shipmates.yaml`; optionally install personas |
| `add <persona>` / `remove <persona>` | install / uninstall a persona (memory preserved unless `--purge`) |
| `list` | catalog personas + which are installed |
| `update [persona]` | refresh installed personas from the embedded catalog (`--accept ours\|theirs` for non-interactive conflict resolution) |
| `render <p> --target` | export a persona to a thin target (`agents-md` / `cursor` / `windsurf`) |
| `open <p>` | launch an interactive session as a persona |

### Talk to crew

| Command | What it does |
|---|---|
| `ask <p> <prompt>` | one-shot delegation; resumes the persona's session (`--fresh` starts new) |
| `tell <p> <msg>` | message a live crew process while it works |
| `feed` | print the coordination server's activity feed |
| `bridge` | tabbed terminal UI over every live mate on this machine — NAVIGATE/TYPE modes, live PTY per tab, approvals with one key |
| `fanout <a,b,c> <prompt>` | run the same prompt across personas in parallel |
| `drain <p>` | dispatch a persona to drain its work queue, then exit (`--cap N`) |
| `drain-many <p...>` | drain several personas in parallel (`--all` / `--max-concurrent N`) |
| `autonomous --print-charter` | print a captain scheduler charter to feed into cron / CronCreate / Actions |
| `show <p> <file> [--caption <text>]` | attach a file (photo, screenshot, PDF, text) to a live crew process — mate reads it via Claude Code's multi-modal Read tool |

### Voyage — plan → commission → sail

A voyage is a multi-task plan executed by the Go orchestrator: dependencies, retries,
tier escalation, and state live in code, and the model is spent only on the tasks
themselves. See [`docs/sailing.md`](docs/sailing.md) for the full lifecycle and a worked
example.

| Command | What it does |
|---|---|
| `plan` | validate the voyage draft and show its status — per-task state, acceptance verdict, structured-recovery ledgers (`--fresh` clears the draft) |
| `commission` | commission the draft for execution — an admiral act, refused from inside agent turns |
| `sail` | execute the commissioned voyage until every task is done or blocked (`--dry-run`, `--retry-failed`, `--max-concurrent N`, `--task-timeout 30m`, `--runtime claude\|codex\|openai`) |
| `beads init` / `beads <bd args...>` | bootstrap and manage the optional Beads task graph; without it, plan/sail track state in markdown |

### Permission gate

| Command | What it does |
|---|---|
| `pending` | list crew tool calls awaiting a decision |
| `allow <id> [--for 30m]` | approve a pending request, optionally time-boxed (any `time.ParseDuration` string) |
| `deny <id>` | reject a pending request |

### The Brig — Ship's Articles

Fifteen security articles compiled into kernel rules and bound to every persona — deny/ask
gates a prompt cannot talk its way past (force-pushes, `.env` and credential writes,
`curl\|sh`, settings self-escalation). Configurable per project; disable entirely or waive
individual articles in `shipmates.yaml`. The fleet-wide deny list is not the Brig's to
waive — it holds even with the Brig off.

| Command | What it does |
|---|---|
| `brig status` | the operator's brig posture and the freeze state |
| `brig list` / `brig explain <handle>` | the fifteen Articles; full rule text with rationale and enforcement layer |
| `brig log` | print enforcement entries from `.shipmates/brig.log` |
| `freeze [--reason <text>] [--admiral <who>]` | engage the freeze: refuse all Write operations until released (binds bypass-mode personas too) |
| `release` | release the freeze; Writes resume |

### Routing (GitHub-issues-and-PRs conventions)

| Command | What it does |
|---|---|
| `routing apply <file>... \| --all` | compose the routing block (claim-by-label / worktree-per-issue / verdict-merge-gate) into custom persona files |
| `routing show` | print the active routing block (what `/sync-routing` loads) |

### Ship (per-machine daemon)

| Command | What it does |
|---|---|
| `ship install` | install the per-host supervisor (Windows Scheduled Task / macOS launchd) |
| `ship serve` | run the supervisor in foreground |
| `ship uninstall` | remove the supervisor |
| `ship add <dir>` | add a project to the ship's supervised list |
| `ship status` | show which projects the ship supervises |

### Fleet Command / cross-ship operations

| Command | What it does |
|---|---|
| `fleet serve` | run Fleet Command (web UI + voice + tunnels + shared graph) |
| `fleet ls` | list connected captains across the fleet |
| `fleet tell <captain-key> <persona> <msg>` | tell a mate on any ship |
| `fleet show <captain-key> <file>` | attach a file/photo to a captain |
| `fleet tail <captain-key>` | tail a captain's event feed |
| `fleet pending <captain-key>` | list pending requests on a specific ship |
| `fleet resolve <captain-key> <id> allow\|deny` | remotely approve/deny |
| `fleet status` | fleet-wide status dots |
| `fleet beads` | fleet-wide task-graph view |
| `fleet dispatch <captain-key> <bead-id> <ship> <persona>` | cross-ship bead reassignment |

### Server (transient coordination server on each project)

| Command | What it does |
|---|---|
| `server serve` | run the per-project coordination server (auto-spawned usually) |
| `server stop` | shut it down |

## Configuration

Per-project `shipmates.yaml`:

```yaml
# Prefix for session names — usually the repo name
sessionPrefix: my-project

# The lead persona on this ship
captainPersona: captain

# Optional: per-persona overrides
crew:
  captain:
    dangerouslySkipPermissions: true
  security:
    dangerouslySkipPermissions: false

# Optional: GitHub-issues-and-PRs as the coordination surface
routing: github
routingOnBoot: true
routingOptions:
  bylines: true    # "<persona> here, …" intros on GitHub comments
  labels:  true    # persona-name labels as the work queue

# Optional: commit per-persona memory for team sharing
sharedMemory: true

# Report in to Fleet Command (omit for single-ship use)
fleet:
  url: http://127.0.0.1:8443
```

Fleet Command policy (`~/.shipmates/fleet-policy.yaml`, edited by the Admiral):

```yaml
# Unconditional denies applied fleet-wide. Ships pull this on tunnel
# connect and every 5 minutes. Un-overridable.
deny:
  - Bash(rm -rf /)
  - Bash(rm -rf ~)
  - Bash(gh secret *)
  - Bash(sudo *)
  - Bash(curl * | sh)
  - Bash(curl * | bash)
```

## The starter crew

Six personas, opinionated, engineering-department flavored. Rename any of them to fit your team.

| Persona | Owns |
|---|---|
| **captain** | Strategy, direction, push-back. The human + AI partnership in the chair. Doesn't ship code. |
| **architect** | Cross-cutting design, docs, consistency over time |
| **security** | OWASP, dep hygiene, secret detection, auth patterns |
| **frontend** | UI, accessibility, perf, browser concerns |
| **backend** | APIs, database, request lifecycle |
| **tester** | QA, regression, coverage |

## Prior art and honest positioning

Shipmates sits in a small but growing category — coordination platforms for teams of AI coding agents. Reference points:

- [Andrew Brookins' Python multi-claude](https://github.com/abrookins/multi-claude)
- [Dan Lorenc's Go multiclaude](https://github.com/dlorenc/multiclaude)
- [Steve Yegge's Gas Town](https://github.com/gastownhall/gastown)
- [AmeanAsad's claude-fleet](https://github.com/AmeanAsad/claude-fleet) — closest architectural peer (outbound WebSocket, central server)
- [Kresimir Stevica's Captain Claw](https://github.com/kstevica/captain-claw) — closest feature-overlapping peer (multi-machine routing, browser-served PTY)

What Shipmates contributes to the shared territory:

- A **merge-safe versioned task graph** (beads/Dolt on git refs in the project's own repo) as the coordination substrate — versioned, mergeable across independent writers, ridden on infrastructure you already trust
- A **voice-first browser UI** with a conversational AI persona (the Commodore) driving fleet operations hands-free — pluggable STT/LLM/TTS end-to-end, no phone or third-party chat client required
- A **single Go binary** with a vanilla-JavaScript UI (no framework, no build step) and no proprietary dependencies

## What Shipmates is not

| Concern | Tool category | Shipmates' position |
|---|---|---|
| Big persona catalog (50-100+ headless experts) | claude-skills, VoltAgent, Agency Agents | Out of scope. Ships a small opinionated starter set; the value is the memory layer, not catalog breadth. |
| Build-your-own agent runtime | LangGraph, Burr, Temporal | Out of scope. Wrong abstraction level. |
| Model / inference layer | OpenAI, Anthropic, local llama.cpp | Out of scope. Rides on Claude Code + your own model endpoints. |

Shipmates fills exactly one gap that no other tool handles cleanly: **persona + persistent project memory + opinionated crew assembly + fleet coordination + human-in-the-loop permission**. Bring your own routing layer.

## Persona berths

A **berth** is a persona's persistent home — a git worktree at `.shipmates/berths/<persona>` that its Claude Code session runs *inside*, instead of sharing the repo root with every other session. Berths are opt-in per persona via frontmatter: `berth: auto` creates and uses the worktree (the captain default), `berth: off` runs at the repo root (today's behavior — the fleet default for crew), `berth: require` errors if the berth is absent. Sessions created before berthing was enabled keep their creation-cwd on resume; only new sessions land in the berth. Manifest-mutating commands (`add`/`remove`/`update`/`init`) refuse to run from a berth — they belong at the canonical root. See [`docs/persona-berths.md`](docs/persona-berths.md) for the full design.

## Deeper reading

- [`docs/architecture.md`](docs/architecture.md) — persona format, memory model, lifecycle, captain-and-crew shape
- [`docs/fleet-architecture.md`](docs/fleet-architecture.md) — Fleet Command + multi-ship architecture (tunnels, PTY panes, shared beads memory, Admiral/Commodore voice loop)
- [`docs/persona-berths.md`](docs/persona-berths.md) — per-persona worktrees, launch ergonomics, guardrails
- [`docs/PHILOSOPHY.md`](docs/PHILOSOPHY.md) — why persistent memory changes review quality, with a worked case study
- [`docs/diagrams.md`](docs/diagrams.md) — sequence diagrams for tell/dispatch/attach flows

## Status

Working, deployed, and running against real projects. Recent milestones:

- **v0.3.0** (July 2026) — naval-metaphor rename (Fleet Command / Captain / Commodore), permission model with Claude Code inheritance + fleet-native policies, binary attach (photo/file upload via UI or `fleet show` CLI), session auto-repair for stale claude session markers

The captain↔crew loop (dispatch → live steer → observe tool use → human-in-the-loop approval) is verified end-to-end against Claude Code. Cross-ship dispatch works via the beads graph. Voice control via the Commodore has been running against local qwen/haiku deployments and OpenAI-compatible endpoints.

Per-project config lives in `shipmates.yaml`. Dependencies are intentionally small — `urfave/cli`, `yaml.v3`, `rancher/remotedialer` for the tunnels, standard library for everything else.

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com) (v2) and GitHub Actions. Tag a commit and push:

```sh
git tag v1.2.3
git push origin v1.2.3
```

CI cross-builds binaries (linux/darwin/windows × amd64/arm64), archives them, generates checksums, and publishes a GitHub Release with the version embedded via `-ldflags`. Dry-run locally with `goreleaser release --snapshot --clean`.
