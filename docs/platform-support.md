# Platform support

Shipmates compiles on Linux, macOS, and Windows. Feature availability by
platform is not uniform: some subsystems depend on unix primitives that
have not been ported to Windows. This page enumerates what works where
and why.

## Command availability

| Command | Linux | macOS | Windows | Notes |
|---|---|---|---|---|
| `init`, `add`, `list`, `remove`, `update` | ✅ | ✅ | ✅ | Every persona/policy mutation takes the project's policy write lock, which is `flock` on unix and `LockFileEx` on Windows. Windows before v0.4.1 failed here with "secure policy mutation locking is unsupported on this platform" and left a half-created project behind; both are fixed. |
| `policy`, `render`, `routing` | ✅ | ✅ | ✅ | `policy validate` / `explain` capture a real snapshot on all three (`openat`+`O_NOFOLLOW` on unix, reparse-point refusal plus pinned directory handles on Windows). `routing apply` additionally needs an atomic directory-entry exchange and is Linux/macOS only — see below. |
| `beads` | ✅ | ✅ | ✅ | Requires the external `bd` binary on `PATH` — and only on `PATH`; a `bd` next to the `shipmates` executable is deliberately not used. Verified against `bd` 1.1.2 on Linux and Windows. Sail's Beads *mirroring* is unix-only, but only because `sail` is: it persists through `voyage.SaveState`, which fsyncs a directory. |
| `plan` | ✅ | ✅ | ✅ | Planning subsystem (voyage plan validation). |
| `open`, `ask`, `live` | ✅ | ✅ | ⚠️ | The selected runtime's CLI must be installed and authenticated. Windows: `sail` is unavailable but these dispatch commands work when the runtime CLI is present. |
| `tell`, `feed`, `interrupt` | ✅ | ✅ | ⚠️ | Same as `ask` — depend on the local server. |
| `show` | ✅ | ✅ | ⚠️ | Same as `ask`. File validation is platform-specific but complete on all three: `openat` + `O_NOFOLLOW` on unix, `FILE_FLAG_OPEN_REPARSE_POINT` refusal on Windows. Delivery into a running live turn needs the local server, so it shares `live`'s constraints; without one it falls back to a one-shot turn. |
| `fanout`, `drain`, `drain-many`, `autonomous` | ✅ | ✅ | ⚠️ | Codex-native execution in this release; will follow the runtime interface migration. |
| `sail` | ✅ | ✅ | ❌ | PID-file dispatch locks + unix signal semantics; returns a clear error on Windows. |
| `fleet`, `server` | ✅ | ❌ | ❌ | Fleet Commander M1-M3; unix-only because of `openat`, `O_NOFOLLOW`, `flock`. Absent from the CLI on non-unix. |
| `ship serve`, `add`, `status`, `install`, `uninstall` | ✅ | ✅ | ✅ | The per-host supervisor: one daemon per machine, keeping a project server alive in every dir listed in `~/.shipmates/ship.yaml`. Depends on nothing unix-only. `ship install` registers it as a Windows Scheduled Task or a macOS launchd user agent; on Linux it is still unimplemented (write a systemd *user* unit by hand). |
| `ship observe` | ✅ | ❌ | ❌ | Fleet Commander's tunnel/steer surface, unix-only for the same reason as `fleet`. Absent from the `ship` command tree on non-unix. |

## Why some things are unix-only

### Routing apply — atomic directory-entry exchange

`shipmates routing apply` commits through an atomic *exchange* of two
directory entries (`renameat2(RENAME_EXCHANGE)` on Linux, `renamex_np`
on macOS) so a persona file is never observable in a half-written state.
Windows has no equivalent primitive — `ReplaceFileW` is close but not an
exchange — and neither do the remaining unix platforms, so
`project.RoutingTransactionsSupported()` reports false there and
`routing apply` refuses with `atomic routing transactions are
unsupported on this platform`. Everything else about routing (`routing
show`, composition at `add`/`update` time) works everywhere.

### Fleet Commander (M1-M3) — `fleet`, `server`, `ship observe`

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

Only `ship observe` is affected. The rest of the `ship` tree is the
per-host supervisor, which shares no code with Fleet Commander and runs
on every platform — see below.

## The supervisor — `ship install`

One install per machine, not per project. The supervisor is a single
daemon that reads `~/.shipmates/ship.yaml` and keeps a project server
alive in every dir listed there, so five projects on one host means one
`ship install` plus five `ship add <dir>`.

### Windows — Scheduled Task

The task is registered from generated task XML rather than through
`schtasks /Create` flags, because the settings that matter are not
reachable from the command line and their defaults are wrong for a
long-lived supervisor:

| Setting | schtasks default | What shipmates registers |
|---|---|---|
| `ExecutionTimeLimit` | `PT72H` | `PT0S` (no limit) — otherwise the supervisor is killed after three days, silently |
| `DisallowStartIfOnBatteries` | `true` | `false` — a UPS taking over during an outage must not stop the ship |
| `StopIfGoingOnBatteries` | `true` | `false` — same |
| `RestartOnFailure` | none | 3 restarts, 1 minute apart — the Windows equivalent of launchd's `KeepAlive` |
| `MultipleInstancesPolicy` | `IgnoreNew` | `IgnoreNew` — kept explicit so the boot and logon triggers cannot race |
| `StartWhenAvailable` | `false` | `true` — run a missed trigger late rather than never |

### Windows — surviving a power cut

The default registration is logon-triggered with an interactive token,
which means it needs somebody to log in. A machine that boots to the
lock screen after an outage never starts the ship.

`ship install --unattended` swaps that for a boot trigger plus a
non-interactive logon type, so the supervisor is running before anyone
touches the console. No auto-logon and no extra daemon are involved —
a daemon could not help, because what is missing on an unattended boot
is a logon session, not a trigger.

Two principals are available:

- **S4U** (default). Stores no password. The task gets a token with no
  network credentials and no access to the user's DPAPI store, neither
  of which shipmates needs: the runtimes authenticate over outbound
  HTTPS, and Claude Code's credentials on Windows are a plain file at
  `%USERPROFILE%\.claude\.credentials.json`. Registering an S4U task
  needs the account to hold the "Log on as a batch job" right.
- **Password** (`--store-password`). A full token, at the cost of a
  password stored as an LSA secret. `schtasks` prompts for it directly;
  shipmates never reads it and it never appears on a command line.

Both keep the supervisor running as the operator's own account with
their profile loaded, which a session-0 service would not.

### macOS — launchd user agent

`ship install` writes a `~/Library/LaunchAgents` plist with `KeepAlive`,
so launchd restarts the supervisor if it dies. `--unattended` is
refused rather than ignored: a LaunchAgent starts at login by design.
Boot-time supervision on macOS means a `LaunchDaemon` running as root
without the user's profile — a different design, not a flag.

### Linux — not implemented

`ship install` returns an error. Run `shipmates ship serve` from a
systemd **user** unit so the supervisor inherits the login environment.
Two things are easy to get wrong by hand:

- `loginctl enable-linger <user>` is mandatory. Without it the unit does
  not start at boot and is killed when the last session ends — the Linux
  analogue of the logon-trigger problem on Windows.
- `After=network-online.target` is inert in a user unit, because the
  user manager has its own unit namespace. Use `Restart=always` with
  `RestartSec=10` to handle a supervisor starting before the network is
  up.

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

Mediating an approval needs an immutable policy snapshot, and shipmates
now captures one on Windows as well as unix, so `shipmates ask --runtime
claude` is mediated against real project policy on all three platforms.
`policy.SecureLoadSupported()` still exists and still reports the
capability; it returns false only on a platform with neither
`openat`-class primitives nor the Win32 handle APIs, and callers still
fail closed there (deny every request, say so on stderr) rather than
degrade silently. The security properties of each implementation are
described in [Security → Policy snapshot capture](security.md#policy-snapshot-capture).

## Where to next

- Migrate the remaining dispatch surface (`open`, `sail`, `plan`, the
  queue workflows) onto the runtime interface. `ask` landed first, then
  `show` and the live-session surface (`live`, `tell`, `feed`,
  `interrupt`). Once done, the same commands work with either runtime on
  any platform where the underlying CLI is installed.
- Port the per-persona dispatch lock to Windows. `project.AcquireDispatchLock`
  releases by unlinking a path whose handle is still open, and Go opens
  files on Windows without `FILE_SHARE_DELETE`, so the unlink fails
  silently and the next dispatch for that persona finds a lock file
  naming a live PID. This is the same gap that keeps `sail` off Windows.
- Give `routing apply` a Windows/portable commit strategy so it does not
  depend on an atomic directory-entry exchange.
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
