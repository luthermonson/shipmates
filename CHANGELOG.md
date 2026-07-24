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

- **Cross-platform build.** Shipmates now compiles on Linux, macOS, and
  Windows; the release workflow ships binaries for all three
  (`amd64` + `arm64` each). Prior to this release, the release workflow
  was Linux-only.
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

- **Two agent runtimes are now first-class peers in the codebase.** The
  `internal/runtime` package defines the interface, and both `claude`
  (default) and `codex` are wired through `internal/runtime/factory`. The
  runtime is selected by `.shipmates/config.yaml` (`runtime: claude` or
  `runtime: codex`), by `~/.shipmates/config.yaml`, or by the global
  `--runtime <name>` CLI flag / `SHIPMATES_RUNTIME` env var. Precedence:
  CLI flag > project > user > default (`claude`).
- **Command migration is incremental.** The command surface
  (`ask`, `open`, `live`, `feed`, `tell`, `interrupt`, `sail`, `plan`,
  `fleet`, `ship`, `server`) still dispatches through the codex-native
  code path in this release, so the `--runtime` flag is currently plumbing
  in front of that path. The claude runtime is fully constructable via
  `factory.NewFromResolved` / `claude.New`, installs personas as
  `.claude/agents/<name>.md`, wires the `SessionStart` memory hook, and
  is unit-tested; migrating the command surface onto the runtime
  interface is tracked in `docs/runtime-interface-plan.md` (Phase 4+).
- When `env.Selector` is asked for codex, it returns a pointing
  `runtime.ErrNotConfigured` because the codex adapter needs
  `codexapp.StartOptions` (transport, credential isolation) that the
  base config cannot carry. Codex-native commands already call
  `factory.NewCodexWith(ctx, opts)` directly.
- Sail, Fleet, and Server remain unix-only (Linux + WSL) for this
  release; the runtime interface does not lift that gate on its own.
- The release workflow now produces Linux, macOS, and Windows binaries
  (`amd64` + `arm64` each) alongside the source archive; the archive
  bundles `README.md`, `LICENSE`, `CHANGELOG.md`, `docs/platform-support.md`,
  `docs/installer-platforms.md`, `docs/security.md`, `docs/cli-reference.md`.
- **macOS non-Linux syscall shims.** Two paths that previously used
  Linux-only syscalls now dispatch by build tag: `internal/codexapp`
  extracted `process_identity_linux.go` (real `PidfdOpen` /
  `PidfdSendSignal`) with a sibling `process_identity_other.go` that
  uses raw PID + `syscall.Kill(pid, sig)` for non-Linux unix; and
  `internal/fleetconfig` replaced the inline `unix.O_PATH` in
  `openDirNoFollow` with a per-platform `dirOpenFlags` constant
  (`O_PATH | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC` on Linux;
  `O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC` elsewhere). The
  security-critical `O_NOFOLLOW` remains on every platform; the
  darwin fallback loses only `O_PATH`'s belt-and-braces confinement
  of what the fd can do post-open.
