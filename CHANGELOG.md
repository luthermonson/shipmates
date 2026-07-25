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
- **Claude tool approvals are mediated.** The claude runtime now spawns
  with `--permission-prompt-tool stdio`, so every tool call Claude Code's
  own permission flow does not resolve arrives as a `can_use_tool`
  control request that shipmates answers. Requests surface as
  `runtime.KindApprovalNeeded` and are routed through the *same*
  mediation path codex uses: policy first, then the attached controller's
  30-second window, then refusal — so `open`, the dashboard, and the
  audit feed work unchanged on either runtime. Verified against claude
  2.1.153; the wire shapes are documented in
  `docs/runtime-interface-plan.md`. A shell tool (`Bash`, `PowerShell`)
  is named for policy by its command verbatim, exactly as codex names an
  exec approval, so one `process.exec` rule governs both runtimes; other
  tools are named `Tool(argument)` (e.g. `Write(/etc/passwd)`), which
  matches no command rule and therefore always reaches an operator.
  Decisions are single-call — shipmates never echoes Claude Code's
  `permission_suggestions` back to persist a rule. An unanswered request
  stalls the turn forever, so every path answers: a request that cannot
  be bound to the live turn and its policy snapshot is denied rather than
  dropped, and one-shot `ask` decides from policy alone (allow on a
  matching rule, deny otherwise) and prints the outcome to stderr.
  `runtime.Caps` grows an `Approvals` bit; both runtimes report true.
- **Personas are actually installed for the claude runtime.**
  `InstallPersona` had no production caller: `add` and `update` rendered
  only the codex artifact, so `.claude/agents/<persona>.md` was never
  written and `claude --agent <persona>` found no definition to load —
  the persona's role, instructions and constraints were silently absent
  and the turn ran as a generic Claude session. `add` and `update` now
  install the configured runtime's persona artifact through
  `factory.PersonaArtifact`, rendered from the same composed catalog
  bytes as the codex artifact, and `remove` deletes it (memory preserved,
  as before). Only the runtime in use gets one, so a codex project never
  grows a `.claude/` directory; the canonical
  `.codex/agents/<persona>.toml` is still written on every runtime
  because it is also shipmates' persona inventory. The claude artifact is
  manifest-tracked and obeys the same rules as every other managed file:
  a hand edit is preserved, a deleted one is re-added, `update` is
  idempotent, and a modified one refuses removal instead of being
  destroyed. Switching `runtime:` to claude and running
  `shipmates update` installs artifacts for the personas already present.
  Frontmatter tool names are translated from shipmates' canonical
  capability names to Claude Code's (`read` → `Read, Glob, Grep`, and so
  on), which is load-bearing rather than cosmetic: verified against
  claude 2.1.153, a subagent declaring `tools: [read, edit, bash]` comes
  up with `"tools":[]` — no tools at all — while the translated names
  yield exactly the expected set. A persona that declares no capabilities
  gets no `tools` key and therefore the full toolbox, because shipmates
  governs tool use through policy and the approval path. Canonical
  parsing normalizes CRLF so a Windows checkout renders byte-identical
  artifacts to a Linux one.
- **The session-start memory hook is actually installed.** `init`, `add`
  and `update` now wire the configured runtime's memory mechanism —
  previously `InstallMemoryHook` had no caller at all, so a claude
  project got no `.claude/settings.json` and real sessions ran with no
  durable memory injected. The written shape was also wrong: Claude Code
  silently ignores a flat `SessionStart` entry and only executes the
  matcher-group form
  (`{"SessionStart":[{"hooks":[{"type":"command",…}]}]}`), verified with
  `--include-hook-events` on claude 2.1.153. Any previously written flat
  entry is migrated. Installation merges — unrelated keys, other hook
  events and your own `SessionStart` groups survive, repeats never
  duplicate, and an unparseable `settings.json` is reported rather than
  overwritten. Only the selected runtime is wired, so a codex-only
  project never grows a `.claude/` directory.
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
- **`shipmates show <persona> <file-path>... [--caption <text>]`.**
  Restored from v0.4.0 and reimplemented for all files, not just images.
  Attaches an in-project file — screenshot, log, diff, PDF, source — to a
  persona. If the persona has a turn in flight the file is injected into
  that running turn; if its live session is idle it starts a turn there;
  with no live session it dispatches a one-shot turn. Works on both
  runtimes.
- **Attachment handling by detected kind.** Content kind is sniffed from
  a bounded prefix of the bytes, never from the extension. Images
  (PNG/JPEG/GIF/WebP) ride natively — a `localImage` input on a codex
  turn, a base64 image content block on claude. Text (valid UTF-8, no
  NUL) is inlined into the turn text, bounded at 64 KiB per file and
  128 KiB per batch with an explicit truncation notice. Binary files,
  including PDFs and archives, are **never** base64-encoded into a
  prompt: they are referenced by project-relative path with size and
  detected kind so the agent reads them with its own file tool.
- **`live` works on the claude runtime.** `internal/livesession` now
  consumes `runtime.Runtime` through a narrow `Backend` seam, so `live`,
  `tell`, `feed`, `interrupt`, and `show` all work on either runtime.
  Codex is thread+turn and claude is session+turn; both map onto the
  existing (session, thread, turn) tuple with the runtime session id in
  the thread slot, so exact-turn targeting on `tell` and `interrupt` is
  preserved unchanged — a stale tuple still fails closed. Each runtime
  keeps its own live continuity marker.
- **`--runtime` CLI flag** and `runtime:` config block. Precedence:
  CLI flag > project `.shipmates/config.yaml` > user
  `~/.shipmates/config.yaml` > default (`codex`). `ask`, `show`, the
  live-session surface, and the memory-hook install honor the selection;
  `open`, `sail`, `plan` and the queue workflows are codex-native pending
  migration.
- **`policy.SecureLoadSupported()`.** Reports whether the immutable
  policy snapshot can be captured on this platform. The loader needs
  `openat`-class primitives and so is unix-only; callers that mediate
  runtime approvals consult it and fail closed (deny every request, with
  an explicit warning) rather than degrading silently.
- **End-to-end harness for the claude runtime** at
  `test/e2e/claude/`. Builds the real binary, scaffolds a scratch
  project, and drives `init` / `add` / `update` / `live` / `feed` /
  `tell` / `show` (text and image) / `interrupt` / approvals / `ask`
  against a fake `claude` on PATH that speaks the real stream-json
  protocol including `can_use_tool`. Nothing inside shipmates is stubbed.
  Unix only.
- **Canonical persona catalog.** `internal/runtime/persona` reads and
  writes `persona.md` in a canonical form so the same persona definition
  can be installed for both Claude and Codex without drift.
- **`internal/beads` package.** External `bd` CLI is now wrapped as a
  first-class Go client. `sail` mirrors every voyage task to a Bead with
  lifecycle sync, dependency mirroring, opt-in via `beads.Workspace(".")`,
  and durable prompt injection so personas can read + write task records.

### Changed

- **`internal/turninput` validates arbitrary files, not just images.**
  `FileDescriptorV1` carries a sniffed kind (image/text/binary) alongside
  the existing absolute path, project-relative display path, size and
  content identity; `ImageDescriptorV1` and `ImageBatchV1` remain as
  aliases. Every security property is unchanged on both platform
  implementations: project-root confinement, `openat` + `O_NOFOLLOW` on
  unix, `FILE_FLAG_OPEN_REPARSE_POINT` refusal on Windows, regular-file
  checks, size caps (20 MiB per image, 10 MiB per other file, 64 MiB per
  batch), and TOCTOU revalidation. Reads go through
  `FileDescriptorV1.Bytes`, which revalidates before and after the read.
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
- **The claude memory hook is written in the shape Claude Code
  executes.** The previous flat `SessionStart` entry parsed but was
  silently ignored, so it injected nothing. Existing settings files are
  migrated in place on the next `init` / `add` / `update`.

### Removed

- **`--image` on `ask` and `live`.** Superseded by `shipmates show`,
  which takes any file rather than only PNG/JPEG/GIF/WebP and works on
  both runtimes rather than the codex path only. The flag is still
  parsed but hidden, so typing it produces an error pointing at `show`
  rather than urfave/cli's bare "flag provided but not defined"; no turn
  is dispatched. `live` no longer forwards an `images` array to the
  coordination server.

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
- Windows attachment revalidation no longer double-closes a file handle.
  It wrapped the raw handle in an `os.File` and then closed both, which
  could close an unrelated handle the OS had already reused.

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
- **The live-session server resolves its own runtime.** It is a separate
  process, so the client-side `--runtime` flag on `shipmates live` does
  not reach it: set `SHIPMATES_RUNTIME` in the server's environment or
  `runtime:` in `.shipmates/config.yaml`.
- **Known gaps on the claude live path.** `ResolveApproval` returns
  `ErrUnsupported`, so a claude live session cannot mediate a
  tool-approval request; the gap surfaces as a runtime-scoped error
  rather than being silently allowed. Shipmates also sends codex
  mid-turn steer input as text only, so `show` into a *running* codex
  turn references images by project-relative path instead of attaching
  them (a codex turn started by `show` attaches them natively).
- **`ask`, `show`, and the live-session surface honor `--runtime` /
  config; `open`, `sail`, `plan` and the queue workflows are
  codex-native pending migration.** When the selection resolves to `claude`,
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
