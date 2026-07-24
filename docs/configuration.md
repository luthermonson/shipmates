# Configuration and state

Shipmates has one project configuration file (`shipmates.yaml`), an
optional runtime-selection file (`.shipmates/config.yaml`), and a
deliberately small set of managed state directories. Paths are resolved
from the canonical project root.

## Runtime selection: `.shipmates/config.yaml`

The runtime interface (`internal/runtime`) selects between the `claude`
and `codex` runtime adapters at command entry. Selection sources, from
highest to lowest precedence:

1. `--runtime <name>` CLI flag (or `SHIPMATES_RUNTIME` env var).
2. Project `.shipmates/config.yaml` (`runtime:` key).
3. User `~/.shipmates/config.yaml` (`runtime:` key).
4. Built-in default: `codex` (the command surface is codex-native today;
   claude is an explicit opt-in).

A minimal project override:

```yaml
runtime: claude   # or codex
```

Per-runtime settings live under `runtimes:`; unknown keys are preserved
so a runtime can consume its own settings without this loader knowing
them. Example:

```yaml
runtime: claude

runtimes:
  claude:
    binary: claude          # override PATH lookup
    default_args:
      - --dangerously-skip-permissions   # example only; know what this does
  codex:
    binary: codex

containment:
  mode: watchdog            # watchdog | cgroup | none
  memory_limit_mb: 8192     # 0 = uncapped
  cpu_limit_seconds: 0      # 0 = uncapped; enforced by the poll loop
  max_processes: 0          # 0 = uncapped; kernel-enforced on Windows
  poll_interval_ms: 500
  graceful_timeout_ms: 2000
```

Notes:

- `runtime` values are lower-cased and trimmed before comparison.
- Runtime `settings` are taken from the *first* file that specifies them
  (project, then user) and are **not** merged.
- `containment.mode` accepts `watchdog` (default; kernel-primitive-backed:
  Windows Job Objects enforce `memory_limit_mb` and `max_processes` in the
  kernel, the poll loop covers CPU everywhere), `cgroup` (Linux
  enterprise), and `none` (escape hatch). The cgroup watcher adapter is
  not implemented yet: selecting `cgroup` degrades to `watchdog` and logs
  `containment mode cgroup requested; cgroup adapter not yet implemented,
  degrading to watchdog` at warn level so the weaker posture is visible.
- The `codex` runtime, when requested through `env.Selector`, returns a
  pointing `runtime.ErrNotConfigured` because the codex adapter needs
  `codexapp.StartOptions`. Codex-native commands call
  `factory.NewCodexWith(ctx, opts)` directly; `ask` treats that answer as
  "use the codex-native dispatcher". See
  [Runtime interface plan](runtime-interface-plan.md).

The command surface is being migrated onto the runtime interface
incrementally: `ask` honors `--runtime` / `SHIPMATES_RUNTIME` / the
`runtime:` config key (resolving `claude` dispatches the turn through the
runtime interface; resolving `codex` uses the codex-native dispatcher
unchanged). All other commands are codex-native pending migration.

## Runtime assets

The optional system runtime is installed offline with `sudo shipmates install`.
Its fixed manifest, platform fallback, immutable layout, dry-run/report/
uninstall behavior, and retained state are documented in
[Installer and platform contract](installer-platforms.md). This is separate
from project configuration: it does not read `shipmates.yaml` for credentials,
does not contact Fleet, and does not start M3 qualification.

## `shipmates.yaml`

A typical configuration:

```yaml
sessionPrefix: my-project
skipperPersona: skipper
modelLadder:
  - gpt-5.6-luna
  - gpt-5.6-terra
  - gpt-5.6-sol
sharedMemory: false

crew:
  backend:
    model: gpt-5.4
    effort: high
  tester:
    effort: medium

routing: github
routingOptions:
  bylines: true
  labels: true
```

`modelLadder` is ordered from least to most capable. `sail` requires it,
starts tasks without an override at the first model with `low` effort, and
rejects unknown or descending task ladders.

The generated ladder uses Luna for routine, cost-sensitive work, Terra for a
stronger balance of capability and cost, and Sol as the flagship fallback for
the hardest tasks. Projects may reorder or replace the ladder to match the
models their Codex workspace exposes.

The parser uses a strict schema. Unknown keys are errors. Persona launch
configuration in `shipmates.yaml` supports the Codex-style `model` and
`effort` fields; these are consumed by the codex-native command path.
Runtime binary selection and per-invocation defaults for the claude/codex
runtime adapters live in `.shipmates/config.yaml` under
`runtimes:` (see [Runtime selection](#runtime-selection-shipmatesconfigyaml)
above). There is no alternate backend or arbitrary process command.

### Project fields

`sessionPrefix`
: Namespace used for project session naming. Initialization derives it from the
  repository directory. Keep it stable unless intentionally separating clones.

`skipperPersona`
: Human-facing execution lead used for voyage planning. The human operator is
  captain; the default agent role is `skipper`.

`sharedMemory`
: Reserved project memory-sharing switch. The default is `false`; personas own
  separate memory directories and continuity.

`crew`
: Map of installed persona names to optional Codex launch overrides. Supported
  child fields are `model` and `effort`.

`recovery`
: Optional bounded Sail recovery configuration. Set `autoCaptain: true` only
  when an advisory Sol assessment is wanted. Sail uses the configured
  installed Skipper persona with a fixed model override; no `sol` persona is
  required. `continuationEnvelope` can bound attempts and Sol assessments,
  require a future expiry, and allowlist the stable blocker reason codes. Sol
  remains advisory; it never approves plans, changes criteria, or mutates
  voyage/Beads state. See [Skipper-first recovery](sailing.md#skipper-first-recovery-and-optional-auto-captain).

`derivativeEnvelope`
: Optional Captain-approved ceiling for machine-approved recovery derivatives.
  Sail accepts only finite task mechanics and frozen successor templates within
  this envelope; publication is atomic and material or human-only changes still
  return to the Captain.

`recovery.commanderDelegation`
: Optional, disabled-by-default M1 ship-local policy. When enabled, it names
  one exact Fleet, protocol version `1`, a maximum offer lifetime of at most ten
  minutes, and permitted Commander Ed25519 issuer IDs/public keys. It permits
  only one local read-only recovery assessment; it does not enable Fleet
  transport, remote work, plan changes, Beads operations, or existing recovery
  journal writes. M1 state is isolated under
  `.shipmates/delegations/<voyage-plan-hash>.jsonl` and is never stored in this
  committed configuration file beyond the local policy settings.

The M2 local policy is opt-in independently of Fleet enrollment. A complete
example uses placeholders, not a private key:

```yaml
recovery:
  commanderDelegation:
    enabled: true
    fleetId: "<fleet-id>"
    protocolVersion: 1
    maxOfferSeconds: 600
    permittedIssuers:
      - keyId: "<commander-key-id>"
        publicKey: "<32-byte-ed25519-public-key-base64url-without-padding>"
        revoked: false
```

Keep this file owner-readable and protect the project directory. `enabled:
false` (or an absent block) is the rollback switch. Public keys are trust
anchors, not credentials; rotate by adding the replacement issuer and marking
the old issuer `revoked: true`, then remove the old key after retained journal
records are no longer needed. Configuration validation rejects unknown fields,
invalid key lengths, duplicate key IDs, a non-`1` protocol, lifetimes above ten
minutes, and more than 32 issuers. There is no M2 CLI or network listener:
the local Sail integration supplies the signed envelope and exact approved
voyage/task/recovery case to the internal processor.

`routing`
: Name of the active embedded routing convention. `github` composes the shipped
  GitHub work-queue instructions into installed persona definitions.

`routingOptions.bylines`
: Controls persona bylines in generated routing instructions.

`routingOptions.labels`
: Controls persona-label queue conventions.

`fleet`
: Optional project metadata. Ordinary projects do not need it. Fleet
  enrollment, identity stores, tunnel destinations, and operator capabilities
  are managed by the `ship` and `fleet` commands; see [Operations](operations.md)
  and [Fleet architecture](fleet-architecture.md). Never put credentials here.

## Optional Beads integration

Beads is a first-class but optional integration for projects that want an
external task graph. The external `bd` CLI owns `.beads/` storage, schema,
synchronization, and graph behavior. Shipmates passes bounded arguments to
`bd` and stores only opaque Bead IDs in voyage state; it does not vendor or
reimplement Beads. Ordinary Shipmates projects do not require `bd` or `.beads/`.

Enable it explicitly with:

```bash
shipmates beads init
```

## Canonical persona files

The canonical, runtime-neutral persona source lives at
`catalog/<persona>/persona.md` (with legacy `agent.md` still supported by
the catalog reader). Each runtime installer translates the canonical
form into its native on-disk shape:

```text
.codex/agents/<persona>.toml    # codex — written by shipmates init today
.claude/agents/<persona>.md     # claude — written by runtime.claude.InstallPersona
```

Today `shipmates init` and `shipmates add` write the codex TOML artifact
directly. The claude Markdown writer is implemented in
`internal/runtime/claude`; installing a claude persona also wires a
`SessionStart` memory hook by modifying the project's
`.claude/settings.json` to run the hidden `shipmates hook load-memory`
subcommand, which prints the persona's `.shipmates/memory/<persona>/`
files into the session context at start (bounded, and it never fails a
session — missing memory prints nothing). The CLI's `init`/`add` will
emit the claude artifacts once they are migrated onto the runtime
interface (see [Runtime interface plan](runtime-interface-plan.md)).

Persona names must match `^[a-z][a-z0-9_-]*$`.

## Policy files

```text
.shipmates/policy.yaml
.shipmates/policies/<persona>.yaml
```

The base policy applies to every persona. A persona overlay adds scoped rules.
Policy files are securely opened, bounded, parsed with a strict schema, and
combined into an immutable snapshot before a turn starts.

Rule effects are `allow`, `ask`, and `deny`. Exact-command explanation is
available through `shipmates policy explain`; runtime mediation still binds an
approval to the exact persona, session, thread, turn, controller lease, request,
and policy snapshot.

## Durable memory

```text
.shipmates/memory/<persona>/
```

Catalog memory seeds are copied only when missing. `add` and `update` never
overwrite accumulated memory. Ordinary `remove` preserves it. Only
`remove --purge` deletes a persona memory directory.

Memory files are project content available to the persona through its
instructions and working directory. They are not an isolation boundary between
host processes; the Codex workspace sandbox remains the filesystem boundary.

## Manifest ownership

```text
.shipmates/manifest.json
```

Manifest version 2 maps each Shipmates-managed artifact to its baseline SHA-256.
The lifecycle uses this to distinguish:

- clean managed files that may be advanced;
- locally edited files that must be preserved or explicitly resolved;
- untracked target paths that Shipmates must not claim or delete;
- missing managed files that may be safely restored.

Manifest publication is atomic. Lifecycle operations stage filesystem changes,
publish the complete new manifest, and roll back when the commit boundary has
not been crossed.

## Session and server state

Private transient state lives beneath:

```text
.shipmates/sessions/
```

This includes:

- one-shot Codex continuity markers;
- live app-server continuity markers;
- per-persona dispatch locks;
- loopback server discovery and control state;
- controller and Fleet capability stores;
- bounded interrupt/steer audit state.

Sensitive files are created with restrictive permissions and rejected when
their ownership, type, link count, path, or permissions are unsafe. Session
markers are written only after successful backend acceptance.

## Routing output

`shipmates routing apply` composes a managed routing block into every selected
Codex persona. Routing updates use a project transaction: all target identities
and manifest baselines are checked before any exchange, and partial updates are
rolled back.

## Generated targets

`shipmates render` can produce:

- `agents-md`: a thin `AGENTS.md` target;
- `codex`: a Codex agent TOML file;
- `cursor`: a Cursor-compatible rule target;
- `windsurf`: a Windsurf-compatible rule target.

Generated third-party targets are export artifacts. They do not become
Shipmates runtime authority.
