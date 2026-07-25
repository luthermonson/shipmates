# Platform support

Shipmates compiles on Linux, macOS, and Windows. Feature availability by
platform is not uniform: some subsystems depend on unix primitives that
have not been ported to Windows. This page enumerates what works where
and why.

## Command availability

| Command | Linux | macOS | Windows | Notes |
|---|---|---|---|---|
| `init`, `add`, `list`, `remove`, `update` | ✅ | ✅ | ✅ | Pure Go, no OS-specific primitives. |
| `policy`, `render`, `routing` | ✅ | ✅ | ✅ | Pure Go. |
| `beads` | ✅ | ✅ | ✅ | Requires external `bd` binary on `PATH`. |
| `plan` | ✅ | ✅ | ✅ | Planning subsystem (voyage plan validation). |
| `open`, `ask`, `live` | ✅ | ✅ | ⚠️ | The selected runtime's CLI must be installed and authenticated. Windows: `sail` is unavailable but these dispatch commands work when the runtime CLI is present. |
| `tell`, `feed`, `interrupt` | ✅ | ✅ | ⚠️ | Same as `ask` — depend on the local server. |
| `show` | ✅ | ✅ | ⚠️ | Same as `ask`. File validation is platform-specific but complete on all three: `openat` + `O_NOFOLLOW` on unix, `FILE_FLAG_OPEN_REPARSE_POINT` refusal on Windows. Delivery into a running live turn needs the local server, so it shares `live`'s constraints; without one it falls back to a one-shot turn. |
| `fanout`, `drain`, `drain-many`, `autonomous` | ✅ | ✅ | ⚠️ | Codex-native execution in this release; will follow the runtime interface migration. |
| `sail` | ✅ | ✅ | ❌ | PID-file dispatch locks + unix signal semantics; returns a clear error on Windows. |
| `fleet`, `ship`, `server` | ✅ | ❌ | ❌ | Fleet Commander M1-M3; unix-only because of `openat`, `O_NOFOLLOW`, `flock`. Absent from the CLI on non-unix. |

## Why some things are unix-only

### Fleet Commander (M1-M3) — `fleet`, `ship`, `server`

The delegation mailbox and M3 provisioning validator use filesystem
primitives that shipmates has not yet abstracted:

- `openat` + `O_NOFOLLOW` for symlink-safe directory traversal on the
  ship's inbox
- `flock` for cross-process exclusion during atomic mailbox exchange
- Linux-specific `/proc` + cgroup delegation via
  `shipmates-cgroup-launcher`

Porting to Windows would require re-implementing atomic
exchange semantics against Win32 handle-based file APIs. The path is
open (see `docs/runtime-interface-plan.md`) but not scheduled.

### Sail — voyage executor

`sail` uses PID-file dispatch locks (`AcquireDispatchLock`) that record
the owning PID and check whether it is still live via unix-style
`kill(pid, 0)` — Windows lacks a direct equivalent that works across
sessions and process elevation contexts. `sail` also uses
`signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`, and
`SIGTERM` is not delivered on Windows in the way the executor expects.

Rather than silently hang, `shipmates sail` on Windows returns:

```
shipmates sail is unix-only in v0.4: the voyage executor depends on
PID-file dispatch locks + signal semantics not yet ported to Windows
```

### Cgroup containment mode

The cgroup watcher adapter is not implemented yet, so selecting
`containment: mode: cgroup` in config degrades to `watchdog` on every
platform and logs a warning
(`containment mode cgroup requested; cgroup adapter not yet implemented,
degrading to watchdog`), so operators who opted-in optimistically still
get bounded processes and can see the weaker posture. On Windows the
watchdog additionally programs kernel-enforced Job Object caps for
`memory_limit_mb` and `max_processes`.

## Runtime × platform matrix

| Runtime | Linux | macOS | Windows |
|---|---|---|---|
| `claude` (via `--runtime=claude`, honored by `ask`, `show`, and live sessions) | ✅ | ✅ | ✅ |
| `codex` (via CLI-native commands, the default) | ✅ | ✅ | ⚠️ requires codex CLI |
| `codex` (via `env.Selector`) | ❌ | ❌ | ❌ requires `NewCodexWith` |

The Claude runtime is cross-platform because it drives the Claude Code
CLI via stdio (`claude -p --input-format stream-json --output-format
stream-json --permission-prompt-tool stdio`) — no OS-specific primitives.
`ask`, `show`, and the live-session surface honor `--runtime` / config;
`open`, `sail`, `plan`, and the queue workflows are codex-native pending
migration.

One approval behavior does vary by platform. Mediating an approval needs
an immutable policy snapshot, and the secure policy loader requires
`openat`-class primitives, so it exists on unix only. On Windows and any
other non-unix platform a `shipmates ask --runtime claude` turn still
runs, but with no policy authority every tool permission request is
denied and the degraded posture is printed to stderr. Live sessions are
unaffected in principle — the mediation path is identical — but the
coordination server's state directory is likewise unix-only, so `live`
itself is not yet available on Windows.

## Where to next

- Migrate the remaining dispatch surface (`open`, `sail`, `plan`, the
  queue workflows) onto the runtime interface. `ask` landed first, then
  `show` and the live-session surface (`live`, `tell`, `feed`,
  `interrupt`). Once done, the same commands work with either runtime on
  any platform where the underlying CLI is installed.
- Make the policy loader portable so claude approvals can be mediated
  against project policy on Windows too. The mediation path itself is
  platform-independent; only `policy.Load` is unix-only, and
  `policy.SecureLoadSupported` reports the gap so the non-unix posture is
  a deliberate deny rather than an accident.
- Carry attachments on a codex mid-turn steer. Shipmates sends codex
  steer input as text only, so `show` into a *running* codex turn
  references images by path instead of attaching them.
- Land the cgroup watcher adapter as a real Linux enterprise
  containment mode (currently degrades to watchdog).
- Extract sail's dispatch-lock + signal handling into a platform
  abstraction so `sail` can work on Windows against a native lock
  primitive (e.g. `LockFileEx`).
- Design a Windows/macOS equivalent for the delegation mailbox to
  unlock Fleet Commander (M1-M3) on those platforms.
- Shrink the Windows CI exclusion list in
  `.github/workflows/test.yml`: gate unix-only test files with build
  tags (dashboard, delegation, fleetcommandermailbox, fleetconfig,
  installer) and port the suites that assume unix permission/fsync/
  daemon semantics (client, codexapp, fleetidentity, fleetinterrupt,
  livesession, project, recovery, server, voyage).
