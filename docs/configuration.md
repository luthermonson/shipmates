# Configuration and state

Shipmates has one project configuration file and a deliberately small set of
managed state directories. Paths are resolved from the canonical project root.

## `shipmates.yaml`

A typical configuration:

```yaml
sessionPrefix: my-project
captainPersona: captain
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

fleet:
  url: wss://fleet.example.test/connect
  tokenEnv: SHIPMATES_FLEET_TOKEN
  name: my-project-dev
```

The parser uses a strict schema. Unknown keys are errors. Persona launch
configuration supports Codex `model` and `effort` only; there is no alternate
backend or arbitrary process command.

### Project fields

`sessionPrefix`
: Namespace used for project session naming. Initialization derives it from the
  repository directory. Keep it stable unless intentionally separating clones.

`captainPersona`
: Persona used as the project coordinator by supervisor and Fleet workflows.
  Defaults to `captain`.

`sharedMemory`
: Reserved project memory-sharing switch. The default is `false`; personas own
  separate memory directories and continuity.

`crew`
: Map of installed persona names to optional Codex launch overrides. Supported
  child fields are `model` and `effort`.

`routing`
: Name of the active embedded routing convention. `github` composes the shipped
  GitHub work-queue instructions into installed persona definitions.

`routingOptions.bylines`
: Controls persona bylines in generated routing instructions.

`routingOptions.labels`
: Controls persona-label queue conventions.

`fleet`
: Outbound observer/tunnel configuration. Credentials are never stored here;
  `tokenEnv` names the environment variable that supplies them.

## Canonical persona files

Installed personas live at:

```text
.codex/agents/<persona>.toml
```

These TOML files are canonical runtime instructions. Catalog source lives at
`catalog/<persona>/agent.md` and is rendered directly into the Codex artifact.
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
