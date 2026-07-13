# CLI reference

This reference describes the public Shipmates command surface. Run
`shipmates <command> --help` for flags accepted by the installed version.

## Global behavior

Commands operate on the Git repository containing the current directory.
Persona names begin with a lowercase letter and contain lowercase letters,
digits, underscores, or hyphens.

Delegation writes normalized progress to stderr and the final persona response
to stdout. Shipmates invokes Codex directly with argument arrays; persona input
is never evaluated by a shell.

## Project lifecycle

### `shipmates init [--crew <names>]`

Initializes the repository. `--crew` accepts comma-separated catalog personas.

```bash
shipmates init --crew backend,security,tester
```

### `shipmates add <persona>`

Installs one catalog persona and policy. Missing memory seeds are copied;
existing memory and session continuity are preserved.

### `shipmates list`

Lists catalog personas and installation state.

### `shipmates update [persona] [--accept ours|theirs]`

Updates one persona, or all installed personas when omitted. Modified managed
files produce conflicts. `ours` preserves local content; `theirs` explicitly
accepts current catalog content.

### `shipmates remove <persona> [--purge]`

Removes managed agent and policy artifacts. Memory survives by default.
`--purge` also deletes persona memory and is intentionally destructive.

### `shipmates render <persona> --target <target> [--write]`

Renders `agents-md`, `codex`, `cursor`, or `windsurf` output. Without `--write`,
the result is printed. Export targets do not become runtime authority.

## Delegation

### `shipmates ask <persona> <prompt>`

Runs one synchronous Codex turn. `--fresh` starts a new thread,
`--timeout <duration>` bounds it, and repeatable `--image <path>` flags attach
validated project-local raster images.

```bash
shipmates ask security --timeout 15m 'Review the current diff.'
shipmates ask frontend --image ./screen.png 'Check this layout.'
```

Only the final response is written to stdout. Cancellation and timeout reap the
child and preserve the last successful continuity marker.

### `shipmates fanout <personas> <prompt>`

Sends one prompt to comma-separated personas. Each retains separate memory and
Codex continuity.

### `shipmates drain <persona> [--cap <n>] [--prompt <text>] [--fresh]`

Processes bounded routing work for one persona. `--cap` limits one invocation.

### `shipmates drain-many [personas] [--all] [--cap <n>] [--max-concurrent <n>]`

Drains several personas with bounded concurrency. Supply comma-separated names
or `--all`. Per-persona serialization protects continuity.

### `shipmates autonomous --persona <name> --cadence <duration> --cap <n>`

Runs a bounded recurring drain loop as a local foreground process.

## Live sessions

### `shipmates open <persona> [--fresh] [--plain]`

Starts or attaches the terminal dashboard and acquires a renewable controller
lease. `--plain` supports constrained terminals and logs.

### `shipmates live <persona> <prompt>`

Starts a managed Codex app-server turn and reports session, thread, and turn
identifiers. It accepts `--fresh` and repeatable `--image` flags.

### `shipmates feed <persona> [--follow] [--after <sequence>]`

Reads normalized events. `--follow` waits for later events; `--after` resumes
after a known sequence.

### `shipmates tell <persona> <session> <thread> <turn> <message>`

Steers one exact active turn with text. A stale tuple fails closed and is never
redirected to newer work.

### `shipmates interrupt <persona> <session> <thread> <turn>`

Interrupts one exact active turn with the same stale-target guarantees.

## Policy and routing

### `shipmates policy validate <persona>`

Validates combined project and persona policy without starting a turn.

### `shipmates policy explain <persona> --command-exact <command>`

Explains the effective decision for one exact command using bounded output.

### `shipmates routing show`

Prints the active routing convention and composed instructions.

### `shipmates routing apply [persona]`

Atomically composes routing into one or all installed Codex personas. It does
not start tasks or construct an execution graph.

## Local server

### `shipmates server serve`

Runs the project-local loopback server in the foreground. Authenticated
discovery state lives beneath `.shipmates/sessions/`.

### `shipmates server stop`

Authenticates to the exact discovered server and requests bounded shutdown.
Stale, unsafe, or mismatched discovery state is rejected.

## Fleet observer and control

Ship-side commands:

- `shipmates ship observe` publishes bounded state through an outbound tunnel.
- `shipmates ship serve` runs the supervised ship-side service.
- `shipmates ship add` enrolls or configures a ship identity.
- `shipmates ship status` reports local service and identity state.
- `shipmates ship install` installs the user service.
- `shipmates ship uninstall` removes the user service.

Fleet-side commands:

- `shipmates fleet serve-observer` runs the observer UI/API and authority.
- `shipmates fleet ships` lists visible ships.
- `shipmates fleet status` returns one bounded ship snapshot.
- `shipmates fleet events` reads bounded normalized events.
- `shipmates fleet follow` follows later events.
- `shipmates fleet steer` sends text to one exact active target.
- `shipmates fleet interrupt` interrupts one exact active target.

Observer credentials are read-only. Steer and interrupt require separate,
short-lived operation capabilities. Read [Fleet architecture](fleet-architecture.md)
and [Operations](operations.md) before exposing an observer service.
