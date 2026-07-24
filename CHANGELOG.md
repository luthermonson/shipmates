# Changelog

All notable changes to Shipmates are documented here. This project follows
[semantic versioning](https://semver.org/).

## Unreleased

### Added

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

- **Cross-platform build.** Shipmates now compiles on Linux and Windows;
  the release workflow ships binaries for both. macOS builds from source
  are partially supported (see Notes for operators). Prior to this
  release, the release workflow was Linux-only.
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

- The release workflow now produces Linux and Windows binaries
  (`amd64` + `arm64` each) alongside the source archive; the archive
  bundles `README.md`, `LICENSE`, `CHANGELOG.md`, `docs/platform-support.md`,
  `docs/installer-platforms.md`, `docs/security.md`, `docs/cli-reference.md`.
- **macOS is not yet in the release workflow.** Two files use Linux-only
  syscalls that don't cross-compile to darwin: `internal/fleetconfig/config.go`
  uses `unix.O_PATH` (part of the M3 credential-open path), and
  `internal/codexapp/process_unix.go` uses `unix.PidfdOpen` /
  `unix.PidfdSendSignal` for atomic process handles. Both need a
  darwin-native port (kqueue / EVFILT_PROC for process handles; a
  descriptor-only open dance for the M3 openat chain) before darwin can
  be added to `.goreleaser.yml`. Tracked as a follow-up.
- Codex remains selectable in config, but `env.Selector` returns a
  pointing error for codex: it requires `codexapp.StartOptions`
  (transport, credential isolation) that the base config cannot carry.
  Use `factory.NewCodexWith(ctx, opts)` directly, as the existing
  codex-native commands (`ask`, `live`, `sail`, `fleet`, `ship`,
  `server`) already do.
- Codex remains selectable in config, but `env.Selector` returns a
  pointing error for codex: it requires `codexapp.StartOptions`
  (transport, credential isolation) that the base config cannot carry.
  Use `factory.NewCodexWith(ctx, opts)` directly, as the existing
  codex-native commands (`ask`, `live`, `sail`, `fleet`, `ship`,
  `server`) already do.
