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
  hook for durable memory. The hook runs the new hidden
  `shipmates hook load-memory` subcommand, which prints the persona's
  `.shipmates/memory/<persona>/` files into the session context (the
  runtime exports `SHIPMATES_PERSONA` to the spawned process so the hook
  knows which persona is starting; the hook is bounded and never fails a
  session). `SessionSpec.Environment` overrides are applied to every
  claude spawn; the codex transport cannot carry them and rejects them
  with `ErrUnsupported` (`Caps.Environment`).
- **Containment watcher.** Cross-platform process containment with three
  modes: `watchdog` (default), `cgroup` (Linux enterprise; the adapter is
  not yet implemented, so it degrades to watchdog with a warn-level log),
  and `none` (escape hatch). Windows uses a Job Object per spawn:
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` always (atomic tree-kill), plus
  kernel-enforced caps when configured — `JOB_OBJECT_LIMIT_JOB_MEMORY`
  from `containment.memory_limit_mb` and
  `JOB_OBJECT_LIMIT_ACTIVE_PROCESS` from the new
  `containment.max_processes`. CPU-seconds limits are enforced by the
  polling watchdog on every platform (Job Objects don't express them this
  way), which also keeps polling memory as defense in depth. Linux reads
  `/proc/<pid>/statm` and uses `Setpgid` + `kill -PGID` for tree-kill,
  macOS uses `Setpgid` + a `ps` subprocess for RSS/CPU (malformed `ps`
  output now surfaces as a skipped, logged sample instead of a silent
  0.0).
- **`--runtime` CLI flag** and `runtime:` config block. Precedence:
  CLI flag > project `.shipmates/config.yaml` > user
  `~/.shipmates/config.yaml` > default (`codex`). `ask` honors the
  selection; other commands are codex-native pending migration.
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

### CI

- New `.github/workflows/test.yml` runs `go build`, `go vet`, and
  `go test -race` on every push and pull request across ubuntu-latest and
  windows-latest. The `fleetconfig`, `m3provision`, and `m3runtime` test
  suites are excluded on Linux: they exercise the provisioned M3
  authority/qualifier posture a stock runner does not provide and fail
  closed by design. Windows runs the cross-platform product surface
  (commands, runtime interface + adapters, containment, catalog, policy,
  …); the unix-only subsystems whose tests are not yet portable are
  excluded there with an explanation in the workflow — shrinking that
  list is tracked in `docs/platform-support.md`.

### Fixed

- `shipmates ask` now consumes the `--runtime` selection instead of
  ignoring it (see Notes for operators below).
- The claude runtime closes its event stream on `Close` (consumers
  ranging over `Events()` no longer hang), guards session state against
  a data race, and refuses a second concurrent turn on the same session
  instead of orphaning the running process.
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
  `internal/runtime` package defines the interface, and both `codex`
  (default) and `claude` are wired through `internal/runtime/factory`. The
  runtime is selected by `.shipmates/config.yaml` (`runtime: claude` or
  `runtime: codex`), by `~/.shipmates/config.yaml`, or by the global
  `--runtime <name>` CLI flag / `SHIPMATES_RUNTIME` env var. Precedence:
  CLI flag > project > user > default (`codex`).
- **`ask` honors `--runtime` / config; other commands are codex-native
  pending migration.** When the selection resolves to `claude`,
  `shipmates ask` dispatches the turn through the runtime interface
  (StartSession/ResumeSession → SendTurn → streamed events) and persists
  the claude session id under
  `.shipmates/sessions/<persona>.claude.session` so later asks resume;
  when it resolves to `codex` — the default — `ask` uses the codex-native
  dispatcher unchanged. The rest of the command surface (`open`, `live`,
  `feed`, `tell`, `interrupt`, `sail`, `plan`, `fleet`, `ship`, `server`)
  still dispatches codex-native regardless of the flag; migrating it is
  tracked in `docs/runtime-interface-plan.md` (Phase 4+). Installing a
  claude persona through the runtime interface modifies the project's
  `.claude/settings.json` to add the `SessionStart` memory hook
  (`shipmates hook load-memory`).
- When `env.Selector` is asked for codex, it returns a pointing
  `runtime.ErrNotConfigured` because the codex adapter needs
  `codexapp.StartOptions` (transport, credential isolation) that the
  base config cannot carry. Codex-native commands already call
  `factory.NewCodexWith(ctx, opts)` directly.
- Sail, Fleet, and Server remain unix-only for this release; the runtime
  interface does not lift that gate on its own. Sail runs on Linux and
  macOS (returns a clean unix-only error on Windows). Fleet and Server
  remain Linux-supported for M1-M3 operations; they compile on macOS via
  the `//go:build unix` gate but macOS is not an exercised deployment
  target for Fleet — see [Platform support](docs/platform-support.md).
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
