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
| `open`, `ask`, `live` | ✅ | ✅ | ⚠️ | The selected runtime's CLI must be installed and authenticated. Windows: `sail` is unavailable but these dispatch commands work when the runtime CLI is present. `open`'s interactive terminal is portable — see [The interactive dashboard](#the-interactive-dashboard--open-and-plan) for the one console requirement it has on Windows. |
| `tell`, `feed`, `interrupt` | ✅ | ✅ | ⚠️ | Same as `ask` — depend on the local server, which now runs on Windows. See [The coordination server on Windows](#the-coordination-server-on-windows). |
| `show` | ✅ | ✅ | ⚠️ | Same as `ask`. File validation is platform-specific but complete on all three: `openat` + `O_NOFOLLOW` on unix, `FILE_FLAG_OPEN_REPARSE_POINT` refusal on Windows. Delivery into a running live turn needs the local server, so it shares `live`'s constraints; without one it falls back to a one-shot turn. |
| `fanout`, `drain`, `drain-many`, `autonomous` | ✅ | ✅ | ⚠️ | Codex-native execution in this release; will follow the runtime interface migration. |
| `sail` | ✅ | ✅ | ❌ | PID-file dispatch locks + unix signal semantics; returns a clear error on Windows. |
| `fleet` | ✅ | ❌ | ❌ | Fleet Commander M1-M3; unix-only because of `openat`, `O_NOFOLLOW`, `flock`. Absent from the CLI on non-unix. |
| `server serve`, `stop` | ✅ | ✅ | ✅ | The per-project coordination server. It is not part of Fleet Commander and was never unix-only by design — it was merely registered from the same list. On Windows it was absent from the CLI entirely, so the child `<exe> server serve` that `client.EnsureRunning` and the `ship` supervisor both spawn exited instantly as an unknown command. See [The coordination server on Windows](#the-coordination-server-on-windows). |
| `ship serve`, `add`, `status`, `install`, `uninstall` | ✅ | ✅ | ✅ | The per-host supervisor: one daemon per machine, keeping a project server alive in every dir listed in `~/.shipmates/ship.yaml`. Depends on nothing unix-only. `ship install` registers it as a Windows Scheduled Task or a macOS launchd user agent; on Linux it is still unimplemented (write a systemd *user* unit by hand). |
| `ship observe` | ✅ | ❌ | ❌ | Fleet Commander's tunnel/steer surface, unix-only for the same reason as `fleet`. Absent from the `ship` command tree on non-unix. |

## The interactive dashboard — `open` and `plan`

`shipmates open <persona>` and `shipmates plan` are the product's only
interactive surfaces, and answering a tool-approval prompt (`/allow-once`,
`/deny`) is only possible from one of them. They used to be Linux-only for
no architectural reason: `internal/dashboard` hand-rolled termios with the
Linux `TCGETS`/`TCSETS` ioctl names — which is also the only reason macOS
was excluded, since macOS spells the same operations `TIOCGETA`/`TIOCSETA`
— and used `unix.Poll` to get a cancellable stdin read. Everything drawn
on top of that was already portable ANSI.

Raw mode, the pre-mutation snapshot, restoration, and terminal size now go
through `golang.org/x/term`, so all three platforms run the same code.

Two consequences worth knowing:

- **Windows needs a VT-capable console.** `x/term` puts the *input* handle
  into raw mode and enables `ENABLE_VIRTUAL_TERMINAL_INPUT` there, so
  arrow, page, and home/end keys arrive as the same escape sequences a unix
  terminal sends. It deliberately does not touch the output handle, and
  without `ENABLE_VIRTUAL_TERMINAL_PROCESSING` every cursor address and
  color would print as literal text. Shipmates sets that mode itself when
  it enters raw mode and restores the previous mode on exit. A console that
  refuses it — conhost older than Windows 10 1809 — gets an error naming
  the mode rather than a screen full of escape-sequence garbage. Windows
  Terminal, PowerShell 7, and modern conhost are all fine.
- **A resize is now detected.** There is no `SIGWINCH` on Windows, and
  `term.GetSize` is one cheap syscall, so the editor polls the size every
  250 ms while it is waiting for a keystroke instead of maintaining two
  notification paths. Before this, size was sampled once at startup and the
  cursor-addressed renderer kept drawing to the old geometry forever.

Cancelling a read is the one place where the port gave something up.
`unix.Poll` has no Windows equivalent and `CancelIoEx` is not reachable
through an `*os.File` that Go owns, so stdin is drained by a dedicated
goroutine doing blocking reads into a channel, and cancellation abandons
the channel rather than interrupting the read. `Next` still returns
immediately on cancellation — the observable contract is unchanged — but
the reader goroutine stays parked in `Read` until stdin yields a byte or
closes. The caller already runs `Next` on its own goroutine and waits only
a bounded time for it during teardown, and the process is exiting by then,
so the cost is one goroutine plus any bytes it had read ahead. The previous
poll loop dropped read-ahead bytes on cancellation too.

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

### Fleet Commander (M1-M3) — `fleet` and `ship observe`

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
on every platform — see below. `server` was also gated behind the same
build tag for no reason of its own; it is not, and never was, part of
this subsystem.

## The coordination server on Windows

Every command that talks to a live session — `open`, `plan`, `live`,
`tell`, `interrupt`, `feed`, `show` — routes through one HTTP server per
project, bound to loopback, discovered through
`.shipmates/sessions/server.json`, and authenticated with the control
token that file carries. `client.EnsureRunning` starts it on demand and
`ship serve` keeps one alive per registered project. Three separate
things had to be true for that to work on Windows, and none of them were.

### It has to be a registered command

`platformCommands()` returned `{Ship()}` on non-unix, so `server` did not
exist in the CLI. Both spawners launch `<exe> server serve` as a child, so
the child exited immediately with "unknown command", `EnsureRunning` gave
up after its five-second health poll, and the supervisor restarted a
process that could never start. Registering `Server()` is the whole fix;
nothing underneath it was unix-specific.

### Privacy has to mean something other than mode bits

The remote-steer and remote-interrupt durable stores, and the interrupt
audit log, refused to open unless `stat` reported `mode&0o077 == 0`. Go
synthesizes `0666` for files and `0777` for directories on Windows and
maps `Chmod` onto `FILE_ATTRIBUTE_READONLY`, so that condition is not
merely false, it is unsatisfiable — the server died at startup with
"remote steer storage unavailable".

Windows expresses the same intent through the DACL, so that is what these
paths check now (`internal/livesession/private_state_windows.go`, built on
`internal/winsec`, the same primitives the policy loader and server-state
writer already use):

- A directory is walked one component at a time from the volume root with
  `winsec.OpenDirChain`. Each component is opened with
  `FILE_FLAG_OPEN_REPARSE_POINT` and refused if it is a reparse point —
  the `O_NOFOLLOW` equivalent — verified to canonicalize back to the name
  that was asked for, and held open *without* `FILE_SHARE_DELETE` so a
  component already proven to be a real directory cannot be renamed away
  and swapped for a junction while the walk continues below it. That is
  what supplies `openat`'s "resolve relative to something I already
  validated" property, which Win32 has no single call for.
- The leaf directory is then given a *protected* DACL granting full
  control to the process user and LOCAL SYSTEM and to nobody else, marked
  inheritable so files created inside start private. Protected means
  inheritance is severed, so a permissive grant on an ancestor cannot flow
  down. `winsec.PrivateDACL` writes it and reads it back, entry by entry,
  before returning.
- Files are created with `CREATE_NEW` (the `O_CREAT|O_EXCL` equivalent),
  refused if they are reparse points, directories, or carry a second hard
  link, and given the same DACL explicitly rather than by inheritance.
  Reads verify it again.

What that is worth relative to unix: on the leaf it is strictly stronger,
because nine mode bits say nothing about ACLs while this enumerates every
ACE and refuses any trustee it does not recognize. On the ancestry it is
equivalent — no symlink/reparse point anywhere, and additionally pinned
against substitution mid-walk, which the unix `Lstat` loop does not do. It
is *not* the same statement, because Windows has no mode bits to compare;
what it is not is absent. `icacls` on a live project shows exactly two
entries per object and no inherited ones.

The audit log is the one place a sharing mode is relaxed: it is held open
for the process lifetime, so it is opened `FILE_SHARE_DELETE` via
`winsec.OpenShared`, matching unix's "a file can be unlinked while a
descriptor is open". Without it the file would be undeletable until the
server exited. It is opened `FILE_APPEND_DATA` without `FILE_WRITE_DATA`,
so the handle physically cannot overwrite a byte already written.

### A durable rename cannot flush a directory

Every durable write staged a temp file, renamed it, and then fsynced the
containing directory. `FlushFileBuffers` is the only directory-flush
primitive Win32 exposes and NTFS refuses it on a directory handle with
`ERROR_ACCESS_DENIED`, which Go surfaces as `sync <dir>: Access is
denied.` In `project.WriteLiveContinuityBackendAt` that error was returned
*after* the rename had already succeeded, so a write that landed reported
failure.

`project.DurableRename` replaces both halves with one per-platform
primitive:

| | Commit | Durability |
|---|---|---|
| unix | `rename(2)` | `fsync` of the containing directory — `rename` is atomic for observers but the new directory entry is page-cache metadata until the directory itself is flushed |
| Windows | `MoveFileEx` with `MOVEFILE_REPLACE_EXISTING` | `MOVEFILE_WRITE_THROUGH` on the same call — documented not to return until the change has been flushed to disk, and on NTFS the rename is one logged metadata transaction, so flushing it commits the directory entry with it |

Where unix needs two ordered operations to make one rename durable,
Windows needs one. The move must stay within a volume, or `MoveFileEx`
degrades to copy-then-delete; every caller stages its temp file in the
destination directory, so that holds.

### One more thing was broken underneath

`codexapp.openProcessIdentity` returned `os.ErrInvalid` on Windows.
`Factory.Start` treats a failure there as fatal — it kills the child it has
just spawned and reports "the Codex app-server could not be started" — so
*every* live session failed on Windows even once the server was up. It now
returns a real process handle from `OpenProcess`, which is the closest
Windows analogue of a Linux pidfd and carries the property that matters:
the handle names one process object for as long as it is held, so a
recycled PID cannot be signalled by mistake. The graceful half has no
Windows equivalent (SIGINT reaches a child through a shared console, and
these children have none), so it is a documented no-op; `Adapter.Close`
already closes the child's stdin and waits first, and escalates to
`TerminateProcess` if that does not finish.

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
  `.github/workflows/test.yml`. What is left is four packages whose test
  files are not build-tag gated so they do not compile on Windows
  (delegation, fleetcommandermailbox, fleetconfig, installer) plus four
  that keep their own private copies of the two unix assumptions the
  coordination server has now shed — the `mode&0o077` privacy check and
  rename-then-fsync-the-directory: `fleetidentity` and `fleetinterrupt`
  in their durable credential/operation stores, `recovery` in its
  journal, `voyage` in `SaveState`. Porting them is
  `project.DurableRename` plus the `winsec` DACL check applied the same
  way, and it is what would also unlock `sail`'s Beads mirroring.
  `dashboard` came off once the interactive terminal became portable;
  `client`, `codexapp`, `livesession`, `project`, and `server` came off
  with the coordination-server port described above.
