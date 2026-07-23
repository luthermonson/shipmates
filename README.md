# Shipmates

Shipmates runs persistent, project-scoped AI personas through Codex. It installs
Codex persona definitions, keeps durable project memory, applies local policy,
and provides one-shot delegation, live sessions, a local dashboard, and narrow
authenticated Fleet observation and exact-turn control.

The supported runtime is Linux. WSL is acceptable because it supplies Linux;
WSL setup and native Windows/macOS support are outside this repository.

## Requirements

- Linux (including WSL).
- Go 1.26.5 or newer when building from source.
- An installed and authenticated Codex CLI, available as `codex` on `PATH`.
- A Git repository for the project.

## Quick start

For a released Linux binary, download the appropriate asset from the
[latest release](https://github.com/luthermonson/shipmates/releases/latest),
verify its published checksum, place `shipmates` on `PATH`, and install the
optional Shipmates-owned runtime assets offline:

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

Authenticate Codex before the first delegation:

```bash
codex login
codex login status
```

Initialization creates `.codex/agents/`, `.shipmates/policies/`,
`.shipmates/memory/`, `.shipmates/manifest.json`, and private session state.
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

Shipmates keeps project judgment in Codex while moving orchestration mechanics
into the Go binary. The Skipper helps the Captain define scope, resolve
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

`ask`, queue workflows, `live`, and `open` use the managed Codex app-server
boundary. Local images are existing, validated PNG/JPEG/GIF/WebP files inside
the project and are passed only to the starting turn. Fleet observes bounded
state and can steer or interrupt one exact active turn with separate
capabilities. Fleet cannot start work, approve requests, upload files, open
terminals, or run generic commands.

## Documentation map

- [Getting started](docs/getting-started.md) — installation and first delegation
- [Installer and platform contract](docs/installer-platforms.md) — offline runtime installation, fallback, packaging, and M3 boundaries
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
