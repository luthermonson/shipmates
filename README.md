# Shipmates

**Subagents that remember.**

A toolkit for assembling a small crew of role-specialized AI personas — architect, security, frontend, backend, tester, captain — that accumulate per-project memory across sessions. Two weeks in, the architect reviewing your PR doesn't just see the diff. It sees the diff against a remembered project: *"This re-introduces the pattern we rejected in #45 because X — has anything changed?"*

That's not a 10% better review. That's review on a different cognitive axis (architectural consistency over time) that headless persona catalogs can't reach.

## Why this exists

The existing catalogs (claude-skills, VoltAgent, Agency Agents) ship **headless experts**: system prompts that run in isolated context windows, no memory between invocations. Every call costs ~800 input tokens reloading the situation for ~300 tokens of generic best-practice. High input, low output, low signal.

Shipmates personas have **persistent per-project memory**. They write down what worked, what got rejected, the team's pushback patterns, the rationale behind earlier decisions. On the next session, they read it back. Over weeks, that compounds.

## Install

Download a prebuilt binary from the [latest release](https://github.com/luthermonson/shipmates/releases/latest) — pick the asset for your platform (macOS, Linux, Windows × amd64/arm64). The whole persona catalog is embedded in the binary, so the single executable is all you need.

Asset names look like `shipmates_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), e.g. `shipmates_0.1.0_darwin_arm64.tar.gz`. Extract it and put `shipmates` on your PATH:

```bash
# macOS / Linux — after downloading the asset for your OS/arch:
tar -xzf shipmates_*_*.tar.gz shipmates
sudo mv shipmates /usr/local/bin/
shipmates --version
```

On Windows, unzip the `..._windows_amd64.zip` (or `arm64`) and put `shipmates.exe` on your PATH.

With a Go toolchain (1.26+) you can instead `go install github.com/luthermonson/shipmates@latest`, or build from source with `go build -o shipmates .`.

Shipmates drives the `claude` CLI, so make sure [Claude Code](https://claude.com/claude-code) is installed and authenticated.

## Quickstart

```bash
# scaffold the project and install the full crew
shipmates init --crew captain,architect,security,frontend,backend,tester
shipmates list
```

This vendors persona files into `.claude/agents/<name>.md` (Claude Code reads them natively — no new runtime), seeds each persona's memory at `.shipmates/memory/<name>/`, and writes `shipmates.yaml`. Personas write to their memory dir as they learn your project.

Then use a persona three ways:

```bash
# 1. one-shot delegation (resumes the persona's session each time, so it remembers)
shipmates ask security "review the diff for auth regressions"

# 2. a live crew member you can talk to WHILE it works
shipmates tell security "double-check PR 10"
shipmates feed                 # watch its activity + replies

# 3. an interactive session as the persona (honors its config)
shipmates open captain
```

Or, inside any Claude Code session, invoke a persona via the Agent tool (e.g. "have security review the diff") or launch directly with `claude --agent security`.

## The live captain-and-crew channel

The standout feature, working end-to-end: a **captain** session spawns a small local coordination server, and you dispatch **crew** (individual **mates**) that run as live Claude Code processes you can steer and supervise.

```bash
shipmates tell security "audit the auth middleware"   # auto-spawns server + crew
shipmates feed                                         # live tool-use + responses

# when a crew member wants to run a risky tool (Bash/PowerShell), it blocks:
shipmates pending                                      # → "a1b2c3d4  security wants Bash"
shipmates allow a1b2c3d4                               # approve  (or: shipmates deny <id>)
```

Crew tool activity streams to `feed` via Claude Code HTTP hooks, and the human-in-the-loop approval is a real blocking gate — proven both ways (allow → tool runs, deny → tool blocked).

## Commands

| Command | What it does |
|---|---|
| `init [--crew a,b,c]` | scaffold `.shipmates/` + `shipmates.yaml`; optionally install personas |
| `add` / `remove` | install / uninstall a persona (memory preserved unless `--purge`) |
| `list` | catalog personas + which are installed |
| `update [persona]` | refresh installed personas from the embedded catalog (diff-on-conflict; `--accept ours\|theirs`) |
| `render <p> --target` | export a persona to a thin target (`agents-md` / `cursor` / `windsurf`) |
| `ask <p> <prompt>` | one-shot delegation; resumes the persona's session (`--fresh` to start new) |
| `tell <p> <msg>` | message a live crew process while it works |
| `feed` | print the coordination server's activity feed |
| `pending` / `allow` / `deny` | list and resolve crew permission requests |
| `open <p>` | launch an interactive session as a persona (honors `permissions.mode`, `model`, `effort`, `remoteControl`) |
| `fanout <a,b> <prompt>` | run the same prompt across personas in parallel |
| `drain <p>` | dispatch a persona to drain its work queue, then exit (`--cap N`) |
| `drain-many <p...> \| --all` | drain several personas in parallel (`--max-concurrent N`) |
| `autonomous --print-charter` | print a captain scheduler charter to feed into cron / CronCreate / Actions |
| `routing apply <file>... \| --all` | compose the routing block into custom (non-catalog) persona files |
| `routing show` | print the active routing block (what `/sync-routing` loads) |
| `server stop` | shut down the transient coordination server |

Opt-in **GitHub routing** (`routing: github` in `shipmates.yaml`) composes claim-by-label / worktree-per-issue / verdict-merge-gate conventions into crew personas; `routingOptions: { bylines, labels }` toggle the private-fleet bits off for open-source. The `/standup` slash command ships in the catalog and installs to `.claude/commands/`.

## The starter crew

Six personas, opinionated, engineering-department flavored. Rename any of them to fit your team — "captain" can become "skipper," "PM," "lead," whatever.

| Persona | Owns |
|---|---|
| **captain** | Strategy, direction, push-back. The human + AI partnership in the chair. Doesn't ship code. |
| **architect** | Cross-cutting design, docs, consistency over time |
| **security** | OWASP, dep hygiene, secret detection, auth patterns |
| **frontend** | UI, accessibility, perf, browser concerns |
| **backend** | APIs, database, request lifecycle |
| **tester** | QA, regression, coverage |

## What Shipmates is not

Honest positioning — adjacent tools and where they live:

| Concern | Tool category | Shipmates' position |
|---|---|---|
| Routing & lifecycle (claim-by-label, branch/worktree management) | Code Conductor, Anthropic Agent Teams, Gas Town | **Out of scope.** Plug shipmates personas into one of these. |
| Big persona catalog (50–100+ headless experts) | claude-skills, VoltAgent subagents, Agency Agents | **Out of scope.** We ship a small opinionated starter set; the value is the memory layer, not catalog breadth. |
| Build-your-own agent runtime | LangGraph, Burr, Temporal | **Out of scope.** Wrong abstraction level. |

Shipmates fills exactly one gap: **persona + persistent project memory + opinionated assembly pattern.** Bring your own routing layer.

## Deeper reading

- [`docs/architecture.md`](docs/architecture.md) — the working architecture doc (persona format, memory model, lifecycle, captain-and-crew shape, open questions)
- [`docs/fleet-architecture.md`](docs/fleet-architecture.md) — Fleet Command + multi-ship architecture (tunnels, PTY panes, shared beads memory, the Admiral/Commodore voice loop)
- [`docs/PHILOSOPHY.md`](docs/PHILOSOPHY.md) — why persistent memory changes review quality, with a worked case study

## Status

Early but working. The Go CLI is implemented — `init`, `add`, `list`, `update`, `remove`, `render`, `ask`, `tell`, `feed`, `pending`/`allow`/`deny`, `open`, `fanout`, and the coordination `server`. The captain↔crew loop (dispatch → live steer → observe tool use → human-in-the-loop approval) is verified end-to-end against Claude Code. Six starter personas ship in the embedded catalog.

Per-project config lives in `shipmates.yaml`: a `crew:` map overrides each persona's permission mode, `dangerouslySkipPermissions`, and `remoteControl` (overrides win over the persona file's frontmatter). `render --write` exports thin targets to their canonical files (`.cursor/rules/<p>.mdc`, marked sections in `AGENTS.md` / `.windsurf/rules.md`). Dependencies are intentionally tiny — `urfave/cli` and `yaml.v3`, everything else standard library.

Still pending: published binaries (release tooling is wired — see below), fuller harness exports, and server-lifecycle edge cases (the coordination server self-terminates after 5 minutes idle).

## Releasing

Releases are automated with [GoReleaser](https://goreleaser.com) (v2) and GitHub Actions. Tag a commit and push the tag:

```sh
git tag v1.2.3
git push origin v1.2.3
```

Pushing a `vX.Y.Z` tag triggers CI, which cross-builds binaries (linux/darwin/windows × amd64/arm64), archives them, generates checksums, and publishes a GitHub Release with the version embedded via `-ldflags`. Dry-run locally with `goreleaser release --snapshot --clean` (requires GoReleaser v2).
