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

The default when nothing is set is `codex`. Precedence is:
`--runtime` (or `SHIPMATES_RUNTIME`) > project `.shipmates/config.yaml` >
user `~/.shipmates/config.yaml` > built-in default.

The runtime interface lives in `internal/runtime` and is wired through
`internal/runtime/factory`. `ask`, `show`, and the live-session surface
(`live`, `tell`, `feed`, `interrupt`) honor `--runtime` / config:
resolving `claude` dispatches through the runtime interface, and resolving
`codex` uses the codex-native path unchanged. `open`, `sail`, `plan`, and
the queue workflows are codex-native pending migration, and the sections
that describe Codex behavior below apply to that path. See
[Runtime interface plan](runtime-interface-plan.md).

`init`, `add` and `update` additionally wire the configured runtime's
session-start memory hook — on claude, a `SessionStart` entry in
`.claude/settings.json` running `shipmates hook load-memory`. Nothing is
written for codex. Re-run `shipmates update` after changing `runtime:` to
wire the new one. See
[Configuration → Session-start memory hook](configuration.md#session-start-memory-hook).

`add` and `update` also install the configured runtime's persona artifact.
On claude that is `.claude/agents/<persona>.md`, which `claude --agent
<persona>` loads the persona's role and instructions from; a codex project
never grows one. The canonical `.codex/agents/<persona>.toml` is written on
every runtime, because it is also shipmates' persona inventory. See
[Configuration → Canonical persona files](configuration.md#canonical-persona-files).

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
existing memory and session continuity are preserved. Writes
`.codex/agents/<persona>.toml` always, plus `.claude/agents/<persona>.md`
when the configured runtime is claude.

### `shipmates list`

Lists catalog personas and installation state.

### `shipmates update [persona] [--accept ours|theirs]`

Updates one persona, or all installed personas when omitted. Modified managed
files produce conflicts. `ours` preserves local content; `theirs` explicitly
accepts current catalog content. The rules apply equally to
`.claude/agents/<persona>.md`: an edited one is preserved, a deleted one is
re-added, and a second run reports nothing. It is also how a project that
switched `runtime:` to claude gains claude artifacts for personas it already
had.

### `shipmates remove <persona> [--purge]`

Removes managed agent and policy artifacts — including
`.claude/agents/<persona>.md` when shipmates installed it. Memory survives by
default. `--purge` also deletes persona memory and is intentionally
destructive. As with every managed target, a hand-edited artifact refuses the
removal rather than being destroyed, and a file shipmates never installed is
left alone.

### `shipmates render <persona> --target <target> [--write]`

Renders `agents-md`, `codex`, `cursor`, or `windsurf` output. Without `--write`,
the result is printed. Export targets do not become runtime authority.

## Delegation

### `shipmates ask <persona> <prompt>`

Runs one synchronous persona turn against the configured runtime. `ask`
honors `--runtime` / `SHIPMATES_RUNTIME` / the `runtime:` config key:
resolving `claude` dispatches through the runtime interface (session ids
persist to `.shipmates/sessions/<persona>.claude.session` so later asks
resume), resolving `codex` — the default — uses the codex-native path
unchanged. `--fresh` starts a new session/thread and `--timeout
<duration>` bounds it. `--image` was removed; use
`shipmates show` (below), which attaches any file on either runtime.

```bash
shipmates ask security --timeout 15m 'Review the current diff.'
```

Only the final response is written to stdout. Cancellation and timeout reap the
child and preserve the last successful continuity marker.

`ask` is one-shot: there is no feed to watch and no controller lease, so
when the runtime asks permission to use a tool the project's policy
snapshot decides alone — allow when a rule matches, deny otherwise — and
the decision is printed to stderr as `approval: allowed by policy: …` or
`approval: denied (no allow rule; ask cannot prompt): …`. A request is
always answered; leaving one unanswered would stall the turn. Use
`shipmates live` / `shipmates open` when a human should decide. The
policy snapshot is captured securely on Linux, macOS, and Windows; on a
platform with neither implementation `ask` runs with no authority and
denies every request, saying so on stderr.

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

The lease is what lets you answer a tool-permission request that project
policy leaves as `ask`. With no attached controller such a request is
refused as `mediation_unavailable` and published to the feed — never
silently allowed, and never left unanswered. This works on both runtimes:
codex approvals arrive as app-server RPCs, claude approvals as
`can_use_tool` control requests, and both are presented as the same
sanitized approval card.

### `shipmates live <persona> <prompt>`

Starts a managed live turn and reports session, thread, and turn
identifiers. It accepts `--fresh`. `--image` was removed; use
`shipmates show` (below).

The live session runs on whichever runtime the project resolves to. Because
the coordination server is a separate process, the client-side `--runtime`
flag does not reach it: set `SHIPMATES_RUNTIME` in the server's environment
or `runtime:` in `.shipmates/config.yaml`. On codex the transport is the
app-server thread; on claude it is a persistent `claude -p
--input-format stream-json --permission-prompt-tool stdio` session, with
the runtime session id occupying the thread slot of the
session/thread/turn tuple. Each runtime keeps its own live continuity
marker, so switching runtimes never resumes the other one's thread.

Tool-permission requests are mediated the same way on both. Policy decides
first; anything policy leaves as `ask` goes to the attached controller
(`shipmates open`) for 30 seconds; with no controller the request is
refused. Every outcome is published to the feed as `request.allowed`,
`request.denied`, `request.refused`, `approval.pending`, or
`approval.resolved`.

### `shipmates feed <persona> [--follow] [--after <sequence>]`

Reads normalized events. `--follow` waits for later events; `--after` resumes
after a known sequence.

### `shipmates tell <persona> <session> <thread> <turn> <message>`

Steers one exact active turn with text. A stale tuple fails closed and is never
redirected to newer work. Works on both runtimes; a runtime that does not
report the steer capability refuses with a runtime-scoped error rather than
silently dropping the message.

### `shipmates show <persona> <file-path>... [--caption <text>]`

Attaches one or more in-project files to a persona. Any file works —
screenshot, log, diff, PDF, source file — not just images. Repeat the path
argument to send several at once (up to 8 per batch).

Delivery depends on what the persona is doing:

- **A turn is already running** — the attachment is injected into that
  turn, so the crew member sees it without waiting for the turn to end.
- **The live session is idle** — it starts a new turn on that session.
- **No live session (or no server)** — it dispatches a one-shot turn, the
  same shape `ask` produces. `--fresh` and `--timeout` apply to that case.

How each file travels depends on what it is, sniffed from the bytes rather
than the extension:

| Detected kind | How it is delivered |
| --- | --- |
| Image (PNG, JPEG, GIF, WebP) | Attached to the turn natively: a `localImage` input on a codex turn, a base64 image content block on claude. The one exception is a **codex mid-turn** injection — shipmates only sends codex steer input as text, so images are referenced by project-relative path there instead, and the message says so. |
| Text (valid UTF-8, no NUL bytes) | Inlined into the turn text, bounded at 64 KiB per file and 128 KiB per batch, with an explicit truncation notice naming the path. |
| Binary (everything else, including PDFs and archives) | **Never** base64-encoded into the prompt. Referenced by project-relative path with its size and detected kind, so the agent reads it with its own file tool. |

Validation is the same on every path: confined to the project root,
symlinks and Windows reparse points refused, regular files only, size-capped
(20 MiB per image, 10 MiB per other file, 64 MiB per batch), and revalidated
immediately before the bytes are read, so a file swapped after validation is
refused rather than sent. When a server is running it revalidates every path
against its own project root — the client's check is for error messages, not
authority.

```bash
shipmates show frontend ./screenshot.png --caption 'The layout breaks here.'
shipmates show security ./build.log ./patch.diff
```

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

Note: `shipmates server` is Shipmates' own project-local loopback
coordination server, distinct from `codex app-server` (the OpenAI Codex
CLI mode Shipmates spawns as a managed child for turn control). The
`server` subcommand below manages the Shipmates process only. See the
[Architecture terminology note](architecture.md#terminology-two-different-servers)
for the full distinction.

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
- `shipmates ship install` installs the user service — one per machine, not
  per project; it supervises every dir in `~/.shipmates/ship.yaml`. Add
  `--unattended` on Windows to register a boot trigger so the supervisor
  returns after a power cut with nobody logged in, and `--store-password`
  to run it under a stored password instead of S4U. See
  `docs/platform-support.md` for what each principal buys.
- `shipmates ship uninstall` removes the user service.

`ship observe` is unix-only; the rest of the `ship` tree runs everywhere.

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
