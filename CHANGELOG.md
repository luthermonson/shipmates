# Changelog

All notable changes to Shipmates are documented here. This project follows
[semantic versioning](https://semver.org/).

## Unreleased

### Added

- **The Brig — Ship's Articles.** shipmates' security-and-hardening
  subsystem enforces fifteen rules on every persona at three layers:
  prompt (developer_instructions reminder), kernel (per-persona policy
  overlay), and freeze (`.shipmates/freeze` emergency stop). Articles 1-5
  are standards-grounded (OWASP Top 10, OWASP LLM Top 10, CWE Top 25,
  NIST SSDF, 12-Factor); Articles 6-15 are incident-driven (No Prod DB,
  No Destructive Git, No Secrets in Commits, Verify Every Package, No
  Piped Execution, No Lies About Failure, Respect the Freeze, Confirm
  Before Destroying, No Self-Escalation, Stay Aboard).
  - New commands: `shipmates brig list|explain|log|install` and
    `shipmates freeze|release`. `brig install` and `brig install --fleet`
    are idempotent — safe to re-run on every upgrade.
  - New package: `internal/brig` carries the canonical rule inventory,
    idempotent `MergeInto` for persona overlays, freeze marker
    check/set/clear, and the JSONL denial log helpers.
  - New catalog assets: `catalog/ARTICLES.md` (canonical rules doc),
    `catalog/brig.policy.yaml` (kernel-policy template merged into
    persona overlays), `catalog/fleet-brig.default.yaml` (fleet-wide
    baseline for `~/.shipmates/brig.yaml`).
  - `shipmates add` and `shipmates update` now stamp the Articles
    reminder block into every persona's `developer_instructions` and
    into the persona's policy overlay (as commented documentation).
    Both are marker-delimited and safe to hand-edit around.
  - Operator guide: [`docs/brig.md`](docs/brig.md).
- **Runtime interface.** New `internal/runtime` package introduces a
  `Runtime` interface with `claude` and `codex` as peer implementations,
  selected by config. Commands are being migrated onto the interface so
  future runtimes can be added without touching command code.
- **Claude Code runtime.** First-class support for driving Claude Code
  alongside Codex. The Claude runtime spawns `claude -p --session-id
  --output-format stream-json`, decodes the JSONL event stream, and
  installs personas as `.claude/agents/<name>.md` with a `SessionStart`
  hook for durable memory.
- **Containment watcher.** Cross-platform process containment with three
  modes: `watchdog` (default, kernel-primitive-backed), `cgroup` (Linux
  enterprise, currently degrades to watchdog), and `none` (escape hatch).
  Windows uses Job Objects (`JOB_OBJECT_LIMIT_JOB_MEMORY`,
  `JOB_OBJECT_LIMIT_ACTIVE_PROCESS`, `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`),
  Linux reads `/proc/<pid>/statm` and uses `Setpgid` + `kill -PGID` for
  tree-kill, macOS uses `Setpgid` + a `ps` subprocess for RSS.
- **`--runtime` CLI flag** and `runtime:` config block. Precedence:
  CLI flag > project `.shipmates/config.yaml` > user `~/.shipmates/config.yaml` > default (`claude`).
- **Canonical persona catalog.** `internal/runtime/persona` reads and
  writes `persona.md` in a canonical form so the same persona definition
  can be installed for both Claude and Codex without drift.
- **`internal/beads` package.** External `bd` CLI is now wrapped as a
  first-class Go client. `sail` mirrors every voyage task to a Bead with
  lifecycle sync, dependency mirroring, opt-in via `beads.Workspace(".")`,
  and durable prompt injection so personas can read + write task records.

### Changed

- **Cross-platform build.** Shipmates now compiles on Linux, macOS, and
  Windows. Prior to this release, the binary was Linux-only.
- **Fleet Commander (M1-M3) is unix-only.** `Fleet`, `Ship`, and `Server`
  commands are gated `//go:build unix` because the underlying durable
  mailbox + delegation validator depend on filesystem primitives
  (`openat`, `O_NOFOLLOW`, `flock`) not yet ported to Windows. On
  non-unix platforms these commands are absent from the CLI surface.
- **Sail returns a clean unix-only error on Windows.** The voyage
  executor depends on PID-file dispatch locks and unix signal semantics.
  Rather than hang, `shipmates sail` on Windows reports the platform
  limitation and points to the issue tracker.

### Fixed

- Windows build no longer fails on `containment.fd undefined` in
  `codexapp/adapter.go` — extracted `extractContainmentPidfd` helper
  into `containment_pidfd_linux.go` (returns the fd) and
  `containment_other.go` (returns 0).
- `TestSailCancellationReturnsTaskToPending` no longer hangs the
  Windows test suite; the sail tests are gated `//go:build unix` to
  match the underlying subsystem.

### Documentation

- New `docs/platform-support.md` enumerates which commands are available
  on which OS and why.
- `docs/runtime-interface-plan.md` documents the runtime interface
  design, phases, and follow-up work (codex through `env.Selector`,
  cgroup watcher adapter, migrating `open`/`sail`/`plan` to the
  Selector).

### Notes for operators

- Windows and macOS binaries are buildable from source (`go build ./...`)
  but are **not** produced by the release workflow yet. `goreleaser` still
  ships Linux-only archives. Cross-platform binaries will follow once
  the Claude runtime has soaked in production.
- Codex remains selectable in config, but `env.Selector` returns a
  pointing error for codex: it requires `codexapp.StartOptions`
  (transport, credential isolation) that the base config cannot carry.
  Use `factory.NewCodexWith(ctx, opts)` directly, as the existing
  codex-native commands (`ask`, `live`, `sail`, `fleet`, `ship`,
  `server`) already do.
