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

The `cgroup` containment mode is Linux-only. On other platforms
selecting `containment: mode: cgroup` in config transparently degrades
to `watchdog`, so operators who opted-in optimistically still get
bounded processes.

## Runtime × platform matrix

| Runtime | Linux | macOS | Windows |
|---|---|---|---|
| `claude` (via `--runtime=claude`) | ✅ | ✅ | ✅ |
| `codex` (via CLI-native commands) | ✅ | ✅ | ⚠️ requires codex CLI |
| `codex` (via `env.Selector`) | ❌ | ❌ | ❌ requires `NewCodexWith` |

The Claude runtime is cross-platform because it drives the Claude Code
CLI via stdio (`claude -p --session-id --output-format stream-json`) —
no OS-specific primitives.

## Where to next

- Migrate `ask`, `open`, `sail`, `plan` command surface onto the runtime
  interface. Once done, the same commands work with either runtime on
  any platform where the underlying CLI is installed.
- Land the cgroup watcher adapter as a real Linux enterprise
  containment mode (currently degrades to watchdog).
- Extract sail's dispatch-lock + signal handling into a platform
  abstraction so `sail` can work on Windows against a native lock
  primitive (e.g. `LockFileEx`).
- Design a Windows/macOS equivalent for the delegation mailbox to
  unlock Fleet Commander (M1-M3) on those platforms.
