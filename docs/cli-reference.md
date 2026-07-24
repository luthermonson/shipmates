# CLI reference

This reference describes the public Shipmates command surface. Run
`shipmates <command> --help` for flags accepted by the installed version.

## Global behavior

Commands operate on the Git repository containing the current directory.
Persona names begin with a lowercase letter and contain lowercase letters,
digits, underscores, or hyphens.

Delegation writes normalized progress to stderr and the final persona
response to stdout. Shipmates invokes the underlying runtime CLI directly
with argument arrays; persona input is never evaluated by a shell.

### Global flags

The following flags apply to every subcommand:

- `--verbose` — enable debug logging.
- `--runtime <name>` — select the agent runtime. Accepted values: `claude`,
  `codex`. Overrides `.shipmates/config.yaml` and `~/.shipmates/config.yaml`.
  Also readable from the `SHIPMATES_RUNTIME` environment variable.

The default when nothing is set is `claude`. Precedence is:
`--runtime` (or `SHIPMATES_RUNTIME`) > project `.shipmates/config.yaml` >
user `~/.shipmates/config.yaml` > built-in default.

The runtime interface lives in `internal/runtime` and is wired through
`internal/runtime/factory`. The individual commands below are being
migrated onto that interface incrementally; in this release the codex-
native command path remains the production dispatch path, and the
sections that describe Codex behavior below apply to that path. See
[Runtime interface plan](runtime-interface-plan.md).

## Runtime installation

### `sudo shipmates install [--dry-run] [--json] [--uninstall] [--profile ubuntu-rojo-localhost]`

Installs the offline, embedded, manifest-verified Shipmates runtime assets at
fixed system paths. The command requires root and accepts no positional
arguments or destination/source/service/credential overrides.

`--dry-run` reports platform composition without changing files. `--json`
prints one bounded `shipmates.install.report.v1` report. `--uninstall` requires
a proven inactive service state, refuses unknown/active state without stopping
anything, removes only matching Shipmates-owned release/current assets, and
retains both recovery journals, credentials, authority, and state. Drifted
objects are reported as incomplete. `--profile ubuntu-rojo-localhost` is a
typed optional hardened-layout plan; it never provisions secrets, starts a
unit, contacts Fleet, or runs qualification.

Installation is idempotent for the verified release and refuses drift,
symlinks, unsafe parents, active-unit conflicts, and partial asset changes.
See [Installer and platform contract](installer-platforms.md).

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

Runs one synchronous persona turn against the configured runtime (currently
the codex-native path in this release). `--fresh` starts a new thread,
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

Prints a bounded orchestration charter for an external scheduler. It does not
install or run a scheduler itself.

### `shipmates beads [bd arguments...]`

Optional integration with the installed external `bd` CLI in the nearest
initialized project. Initialize with `shipmates beads init`; Shipmates forces
noninteractive setup and suppresses editor-agent and hook installation because
it supplies Codex context directly. All other documented `bd` commands and
flags pass through without shell evaluation:

```bash
shipmates beads ready --json
shipmates beads show project-abc --json
shipmates beads comments add project-abc "Verified the focused tests."
```

Running `shipmates beads` without arguments prints Beads project status.
Beads owns graph storage and schema. Ordinary Shipmates projects do not need
the `bd` CLI or a `.beads/` directory.

### `shipmates sail`

Executes the captain-approved `.shipmates/voyage.json` dependency graph until
every task completes or a failure blocks progress. `--dry-run` validates and
displays the order, `--max-concurrent` bounds parallel crew turns,
`--task-timeout` bounds each task, `--retry-failed` resumes failed work, and
`--verbose` shows task briefs, agent reports, and exact tool details exposed by
Codex. Exact command arguments may contain sensitive values, so verbose mode is
explicit. `--no-color` disables persona colors. State persists beneath
`.shipmates/voyages/`. See [Sailing projects](sailing.md).
The Skipper-first recovery contract, optional `recovery.autoCaptain` stage,
lineage flags, reason codes, and restart semantics are documented in
[Sailing projects](sailing.md#skipper-first-recovery-and-optional-auto-captain).

### `shipmates plan [--fresh] [--plain]`

Opens the interactive Captain-Skipper planning room with a validated voyage
sidebar on wide terminals. The Skipper automatically consults the Architect
when a consequential design decision needs specialist input; `/consult
<question>` remains available as a Captain-initiated override. `/sail` starts an
approved voyage, `/sail --verbose` opens the transparent operations-room view,
and incomplete execution returns to the planning conversation with persisted
blocker context. `--fresh` starts a new Skipper thread and clears only the active
`.shipmates/voyage.json` draft; completed voyage state, reports, and memory are
preserved.

## Live sessions

### `shipmates open <persona> [--fresh] [--plain]`

Starts or attaches the terminal dashboard and acquires a renewable controller
lease. `--plain` supports constrained terminals and logs.

### `shipmates live <persona> <prompt>`

Starts a managed Codex app-server turn and reports session, thread, and turn
identifiers. It accepts `--fresh` and repeatable `--image` flags. This
command is codex-native today.

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

- `shipmates fleet init --authority-store <dir> --fleet-id <opaque-id>` creates
  the owner-only durable authority store. Keep it outside repositories.
- `shipmates fleet enrollment create ... --output <new-0600-file>` creates a
  short-lived one-use artifact. Consume it with `fleet enrollment consume` from
  that protected file or from non-echoing stdin; successful file consumption
  removes the artifact and writes ship identity outside the repository.
- `shipmates fleet credential issue --kind observer|steer|interrupt ...
  --output <new-0600-file>` issues one role-separated secret. The command
  prints metadata only. `credential inspect` prints metadata only; rotate,
  commit, and revoke require the exact kind and generation where applicable.
- `shipmates fleet serve-observer` runs the observer UI/API and authority.
- `shipmates fleet ships` lists visible ships.
- `shipmates fleet status` returns one bounded ship snapshot.
- `shipmates fleet events` reads bounded normalized events.
- `shipmates fleet follow` follows later events.
- `shipmates fleet steer-targets` discovers fresh opaque steer targets using
  only a `fleet.steer.turn.v1` credential.
- `shipmates fleet steer` sends text to one exact active target.
- `shipmates fleet interrupt-targets` discovers fresh opaque interrupt targets
  using only a `fleet.interrupt.turn.v1` credential.
- `shipmates fleet interrupt` interrupts one exact active target.

Observer credentials are read-only. Steer and interrupt require separate,
short-lived operation capabilities. Secret output paths must be absolute,
create-new regular files with mode `0600`, and must not be repository paths,
symlinks, existing files, argv values, URLs, stdout, logs, or error text. The
authority store and ship identity store are also external to the project;
Fleet service TLS certificates/keys are supplied separately to
`fleet serve-observer`. Read [Fleet architecture](fleet-architecture.md), the
[Fleet beta.2 runbook](fleet-beta2-runbook.md), and [Operations](operations.md)
before exposing an observer service.
