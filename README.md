# Shipmates

Shipmates runs persistent, project-scoped AI personas on top of an agent
runtime — Claude Code or OpenAI Codex — with durable per-project memory,
local policy enforcement, one-shot delegation, live sessions, a local
dashboard, and narrow authenticated Fleet observation and exact-turn control.

The runtime is selectable per-project via `.shipmates/config.yaml`
(`runtime: claude` or `runtime: codex`) or per-invocation with `--runtime`
(env `SHIPMATES_RUNTIME`); the built-in default is `codex`. The `runtime`
interface, config loader, and both runtime adapters ship in this release,
and `ask` honors the selection: resolving `claude` dispatches the turn
through the runtime interface, resolving `codex` uses the codex-native
dispatcher. The rest of the command surface (`open`, `sail`, `plan`, …)
is codex-native pending migration. See
[`docs/runtime-interface-plan.md`](docs/runtime-interface-plan.md)
and [`docs/platform-support.md`](docs/platform-support.md) for scope.

The Shipmates binary now compiles on Linux, macOS, and Windows. Codex-native
subsystems (Sail voyages, Fleet Commander M1-M3) currently require Linux or
WSL; see [`docs/platform-support.md`](docs/platform-support.md).

## Requirements

- Go 1.26.5 or newer when building from source.
- One of:
  - **Claude Code CLI** on `PATH`, authenticated (`claude auth`), for the
    `claude` runtime. Cross-platform (Linux, macOS, Windows).
  - **Codex CLI** on `PATH`, authenticated (`codex login`), for the
    default codex-native command path. Full Sail/Fleet features on Linux.
- A Git repository for the project.

## Quick start

For a released binary, download the appropriate OS/arch asset from the
[latest release](https://github.com/luthermonson/shipmates/releases/latest),
verify its published checksum, and place `shipmates` on `PATH`. On Linux,
install the optional Shipmates-owned system runtime assets offline:

```bash
sudo shipmates install --dry-run --json
sudo shipmates install
```

The installer does not start a service, install credentials, contact Fleet, or
run qualification. It selects ordinary-operation fallback when hardened Linux
capabilities are unavailable. Then initialize the project:

```bash
cd your-project
shipmates init --crew quartermaster,skipper,security,tester
shipmates ask security "Review the current diff and report the highest-risk issue."
```

From this checkout, build and install the current source revision:

```bash
./scripts/install-codex-adaptation.sh
cd your-project
shipmates init --crew quartermaster,skipper,security,tester
shipmates ask security "Review the current diff and report the highest-risk issue."
```

The source installer script remains a development bootstrap for the project
binary. It is not the M3 runtime installer; released operators use
`sudo shipmates install`.

Authenticate the runtime CLI before the first delegation. For the codex
path:

```bash
codex login
codex login status
```

For the claude path:

```bash
claude auth
```

Initialization creates `.codex/agents/`, `.shipmates/policies/`,
`.shipmates/memory/`, `.shipmates/manifest.json`, and private session state.
The claude runtime's persona installer writes `.claude/agents/<persona>.md`
and wires a `SessionStart` memory hook into `.claude/settings.json` that
runs the hidden `shipmates hook load-memory` subcommand to inject the
persona's `.shipmates/memory/` files into each session; migration of
`shipmates init` onto that interface is tracked in
[`docs/runtime-interface-plan.md`](docs/runtime-interface-plan.md).
See [Getting started](docs/getting-started.md) for the canonical setup guide.

## Core workflows

```bash
shipmates ask backend "Implement the bounded parser change."
shipmates open backend
shipmates live backend "Investigate the failure."
shipmates feed backend --follow
shipmates tell backend SESSION THREAD TURN "Check the edge case."
shipmates interrupt backend SESSION THREAD TURN

shipmates plan
shipmates sail --dry-run
shipmates sail
```

Use `fanout`, `drain`, `drain-many`, and `autonomous` for bounded
multi-persona orchestration. Use `policy validate|explain`, `routing show|apply`,
and `update` for project lifecycle maintenance.

## A deterministic control plane

Shipmates keeps project judgment in the agent runtime while moving orchestration
mechanics into the Go binary. The Skipper helps the Captain define scope, resolve
ambiguity, consult specialists, and produce an approved voyage. Go then owns
dependency scheduling, concurrency, persona isolation, model escalation,
timeouts, retries, cancellation, policy enforcement, and durable resume. Crew
personas spend their turns on the bounded work that actually requires judgment.

This split avoids repeatedly asking a model to rediscover deterministic facts
such as which task is ready, whether a dependency passed, or which retry tier
comes next. It also makes execution easier to test, safer to interrupt, and more
reliable after a crash. The architecture is designed to reduce orchestration
token use, although exact savings depend on the voyage and require measured
token telemetry rather than assumption.

Beads is an optional first-class integration. If the external `bd` CLI is
installed and the project is initialized, `sail` can mirror voyage tasks and
dependencies to Beads. Beads owns its graph storage and schema; ordinary
Shipmates projects do not need `.beads/` or `bd`.

```bash
shipmates beads init
shipmates beads ready --json
```

## Runtime boundaries

On the codex path, `ask`, queue workflows, `live`, and `open` use the managed
Codex app-server boundary. Local images are existing, validated PNG/JPEG/GIF/WebP files inside
the project and are passed only to the starting turn. Fleet observes bounded
state and can steer or interrupt one exact active turn with separate
capabilities. Fleet cannot start work, approve requests, upload files, open
terminals, or run generic commands.

## Documentation map

- [Getting started](docs/getting-started.md) — installation and first delegation
- [Installer and platform contract](docs/installer-platforms.md) — offline runtime installation, fallback, packaging, and M3 boundaries
- [Platform support](docs/platform-support.md) — command availability by OS and runtime
- [Runtime interface plan](docs/runtime-interface-plan.md) — claude/codex runtimes, config, and migration phases
- [CLI reference](docs/cli-reference.md) — commands and flags
- [Sailing projects](docs/sailing.md) — plan, approval, execution, and Beads
- [Configuration and state](docs/configuration.md) — project files and generated state
- [Operations](docs/operations.md) — lifecycle, recovery, upgrades, Fleet
- [Fleet beta.2 runbook](docs/fleet-beta2-runbook.md) — public bootstrap, TLS observation, exact-turn control, and release evidence
- [Security](docs/security.md) — trust boundaries and excluded authority
- [Architecture](docs/architecture.md) — runtime planes and ownership
- [Fleet architecture](docs/fleet-architecture.md) — observer and exact-turn control
- [Diagrams](docs/diagrams.md) — bounded data-flow views

Use `shipmates <command> --help` for the installed binary’s exact flags.
