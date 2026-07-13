# Shipmates

Shipmates runs persistent, project-scoped AI personas through Codex. It installs
Codex persona files, keeps each persona's durable project memory, applies a
project-local policy, and supports synchronous delegation, live turns, a local
operator dashboard, and capability-scoped Fleet observation and exact-turn
control.

The shipped runtime, catalog, configuration, and lifecycle are Codex-native.

## Install and initialize

Requirements: Go 1.26 or newer and the `codex` CLI on `PATH`.

```bash
go install github.com/luthermonson/shipmates@latest
cd your-project
shipmates init --crew security,frontend,tester
```

Initialization writes managed persona definitions to `.codex/agents/`, policy
to `.shipmates/policies/`, durable memory to `.shipmates/memory/`, and a
manifest to `.shipmates/manifest.json`. Memory seeds are copied only when
missing; updates do not overwrite accumulated memory.

## Delegate work

```bash
shipmates ask security "review the current diff"
shipmates fanout security,tester "check the release candidate"
shipmates drain backend --cap 3
shipmates drain-many --all --max-concurrent 2
```

Attach existing project-local raster images with repeatable flags:

```bash
shipmates ask security --image ./screenshot.png "review this UI"
shipmates live frontend --image ./before.png --image ./after.webp "compare these"
```

PNG, JPEG, GIF, and WebP files are validated and passed directly to Codex for
that turn. Shipmates does not upload, copy, retain, fetch, or accept URLs or
generic files.

`ask` and the queue commands run `codex exec --json` without a shell. A
persona's Codex thread is resumed on later turns unless `--fresh` is supplied.
Shipmates records only the Codex thread identity and configuration fingerprint
needed for continuity.

There is no alternate backend, executable fallback, or shell expansion.

## Live sessions and local approval

```bash
shipmates live frontend "inspect the responsive navigation"
shipmates feed --follow frontend
shipmates tell frontend SESSION THREAD TURN "also check keyboard focus"
shipmates interrupt frontend SESSION THREAD TURN
shipmates open frontend
```

`live` uses the Codex app-server protocol. `open` attaches a local terminal
dashboard to the exact project session and holds a controller lease. The
dashboard renders a bounded event projection and supports messages,
`/interrupt`, `/detach`, and `/quit`. When Codex requests an approval, only that
controller may answer with `/allow-once` or `/deny`; approvals expire and never
create durable or time-boxed grants.

While idle, `/image add <path>`, `/image remove <index>`, and `/image clear`
manage images for the next submitted text turn. Selection remains local to the
dashboard, is never queued or persisted, and clears on submission, detach,
stale refusal, or terminal state. Active-turn steering remains text-only.

The local server listens on an ephemeral loopback address and publishes
authenticated discovery state beneath `.shipmates/sessions/`. It has no file
upload, hook, generic terminal, graph mutation, or legacy approval endpoints.

## Policy

Policy is project-local and Codex-native:

```bash
shipmates policy validate frontend
shipmates policy explain frontend --command-exact "go test ./..."
```

Rules live in `.shipmates/policies/`. Validation and explanation are bounded,
read-only operations. A policy decision of `ask` is handled only by the active
local dashboard controller; Fleet cannot approve requests remotely.

## Persona and routing lifecycle

```bash
shipmates list
shipmates add architect
shipmates render architect --target codex
shipmates routing show
shipmates routing apply architect
shipmates update architect
shipmates remove architect
```

`add`, `update`, and `remove` operate on managed Codex artifacts. User-edited
files are not silently overwritten, and memory is retained unless
`remove --purge` is explicitly requested. Routing composes the active routing
block into installed Codex personas; it does not dispatch a task graph.

## Fleet

Fleet is a narrow, authenticated M7–M9 surface:

- `ship observe` publishes a bounded read-only project projection through an
  outbound authenticated tunnel.
- `fleet serve-observer` serves the read-only observer UI/API.
- `fleet ships`, `status`, `events`, and `follow` read fleet state.
- `fleet steer` and `fleet interrupt` target one already-active turn using
  separate short-lived capabilities and immutable audit records.

Fleet cannot start sessions, upload files, answer approvals, open terminals,
dispatch graphs, rescue processes, broadcast messages, or provide voice or
conversation services.

Run `shipmates fleet --help` and `shipmates ship --help` for the required
identity, TLS, capability, and supervisor options.

## Documentation

- [Getting started](docs/getting-started.md)
- [CLI reference](docs/cli-reference.md)
- [Configuration and state](docs/configuration.md)
- [Operations guide](docs/operations.md)
- [Security model](docs/security.md)
- [Architecture](docs/architecture.md)
- [Fleet architecture](docs/fleet-architecture.md)
- [Diagrams](docs/diagrams.md)
- [Philosophy](docs/PHILOSOPHY.md)

## License

MIT
