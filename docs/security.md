# Security model

Shipmates coordinates privileged local AI work. Its model is narrow authority,
exact targets, conservative filesystem ownership, and closed protocols. It does
not make an untrusted repository or host safe.

## Trust boundaries

The operator trusts the local OS account and filesystem, the installed
Shipmates binary, the installed agent-runtime CLI(s) that Shipmates drives
(`claude` and/or `codex`), project content exposed by the runtime's
sandbox, the account(s) those runtimes are authenticated against, and any
enabled Fleet authority. Persona separation provides distinct instructions,
memory, policy, and continuity; it is not an OS security boundary.

## Runtime execution

Shipmates starts a known agent-runtime CLI entry point directly, without
shell interpolation, using argument arrays. Which CLI runs is decided by
runtime selection (see [Configuration](configuration.md#runtime-selection-shipmatesconfigyaml)):
codex CLI on the codex-native command path, or claude CLI when a command
is dispatched through the runtime interface. Runtime binary lookup honors
the operator's `runtimes.<name>.binary` override; persona configuration
may select model and reasoning effort but cannot name an arbitrary
executable.

### Codex execution (current production path)

One-shot dispatch consumes structured JSON events; live dispatch uses a
bounded app-server adapter. Raw frames, prompts, unrestricted tool
arguments, credentials, and Codex stderr are excluded from public event
projections. Unknown protocol shapes fail closed. The effective Codex
sandbox and approvals still matter; review them before work in
repositories containing secrets or production credentials.

### Claude execution (runtime interface)

The `runtime.claude` adapter spawns one persistent `claude -p
--input-format stream-json --output-format stream-json --verbose
--permission-prompt-tool stdio` process per session and parses the JSONL
event stream into the normalized `runtime.Event` channel. Session state is
scoped to the runtime and cleaned up on `Close`. As with the codex path,
the persona instructions never enter a shell, and only normalized events
are exposed to feeds and Fleet.

Shipmates does not silently fall back from one runtime to another. If the
configured runtime's CLI is missing or unauthenticated, dispatch fails
closed with an actionable error rather than trying the other runtime.

#### Claude tool permissions

`--permission-prompt-tool stdio` is what makes a persona's tool
permissions visible to shipmates at all. Without it Claude Code resolves
prompts on its own and the operator never sees them. With it, every tool
call that Claude Code's own permission flow does not resolve arrives as a
`can_use_tool` control request on the session's stdout, and the turn
blocks until shipmates answers on stdin. Verified against claude 2.1.153;
the exact frames are in
[the runtime interface plan](runtime-interface-plan.md#claude-approvals).

Consequences that matter for the security posture:

- **Nothing is auto-approved by shipmates.** A request is either resolved
  by the project's policy snapshot, decided by an operator within the
  30-second window, or denied. There is no implicit allow.
- **Answers are single-call.** Claude Code offers `permission_suggestions`
  that would persist a rule into the project's own settings; shipmates
  never echoes them back. `/allow-once` still creates no durable grant on
  either runtime.
- **Requests are named identically on both runtimes.** A `Bash` or
  `PowerShell` approval contributes its command verbatim — the same string
  codex passes for `item/commandExecution/requestApproval` — so a single
  `process.exec` policy rule governs the same command whichever runtime is
  configured. Tools with no command line are named `Tool(argument)` (e.g.
  `Write(/etc/passwd)`); such a descriptor matches no command rule, so it
  can never inherit an allow written for a shell command and always
  reaches the operator.
- **Unanswered means wedged, so everything answers.** An unanswered
  request stalls the turn indefinitely. A request that cannot be bound to
  the live turn and its immutable policy snapshot is never forwarded into
  the mediation path — the unbindable tuple would be a protocol violation
  — but it *is* denied back to the runtime.
- **`ask` has no operator.** One-shot dispatch has no feed and no
  controller lease, so policy is the entire authority: allow when a rule
  matches, deny otherwise, always answer. Where the secure policy loader
  does not exist (non-unix — it needs `openat`-class primitives) there is
  no authority at all, and every request is denied with an explicit
  warning on stderr. Use `shipmates live` when a human should decide.
- **Claude Code's own permission settings still apply first.** Anything
  its allow rules, `acceptEdits`, or `bypassPermissions` approve is
  resolved before shipmates is consulted and never reaches this path.
  Review `.claude/settings.json` and the user-level settings the same way
  you would review the codex sandbox.

#### Claude session-start hook

`shipmates init` / `add` / `update` write a `SessionStart` hook into
`.claude/settings.json` running `shipmates hook load-memory`, which prints
the persona's `.shipmates/memory/<persona>/` files into the session
context. It is a local command with no arguments derived from untrusted
input, it is bounded (8 KiB total) and it never fails a session. Existing
settings are merged, never replaced; an unparseable settings file is
reported rather than overwritten. Only the configured runtime is wired, so
a codex-only project never grows a `.claude/` directory.

## Filesystem ownership

Manifest baselines prevent silently overwriting or deleting edited managed
files. Sensitive state is created with restrictive permissions and checked for
unsafe type, links, ownership, identity change, and path escape. Multi-file
operations validate before publishing a new manifest and roll back before their
commit boundary when possible.

Memory is user-owned durable content. Ordinary removal preserves it; `--purge`
is the explicit destructive boundary.

## Policy snapshot capture

Every approval decision is made against an immutable snapshot of three
policy sources — `.shipmates/policy.yaml`, `.shipmates/policy.local.yaml`
(optional), and `.shipmates/policies/<persona>.yaml`. The capture is the
security boundary: a snapshot that mixes versions which never coexisted,
or that reads a file an attacker substituted mid-load, is an authority
forgery. Both supported platforms enforce the same five properties, with
different primitives.

| Property | unix (`loader_unix.go`) | Windows (`loader_windows.go`, `internal/winsec`) |
|---|---|---|
| No links anywhere in the path | `openat` + `O_NOFOLLOW` + `O_DIRECTORY` per component | `CreateFile` with `FILE_FLAG_OPEN_REPARSE_POINT` per component, then outright refusal when `FILE_ATTRIBUTE_REPARSE_POINT` is set — this covers junctions, which unlike symlinks need no privilege to create |
| Ancestors cannot be swapped mid-walk | descend from a directory descriptor that was already validated | there is no `openat`; instead every ancestor handle is held open *without* `FILE_SHARE_DELETE`, which makes a validated directory un-renameable and un-deletable, and `GetFinalPathNameByHandle` must return the path that was asked for |
| One atomic capture interval | `flock(LOCK_SH)` on the `.shipmates` directory descriptor; mutations take `LOCK_EX` | `LockFileEx` (shared for readers, `LOCKFILE_EXCLUSIVE_LOCK` for mutations) on `.shipmates/.policy.lock`. Windows cannot lock a directory handle, so this zero-length lock object is the one piece of state the platform must create; it is never read or written, only locked |
| TOCTOU detection | `fstat` before and after the read plus `fstatat(AT_SYMLINK_NOFOLLOW)` on the name, comparing dev, ino, size, mtime, ctime | `GetFileInformationByHandle` (volume serial, file index, size, link count, attributes, write and creation time), `GetFileInformationByHandleEx(FileBasicInfo)` for `ChangeTime` — the ctime analogue — and `FileIdInfo` for the ReFS-safe 128-bit id, compared before and after the read and again against a fresh open of the name |
| Private permissions on what it creates | `0600` files, `0700` directories | a protected DACL granting only the process user and LOCAL SYSTEM, written *and read back* before the object is trusted; a lock object planted with a permissive ACL is repaired, not honored |

Two Windows properties are deliberately stronger than the unix ones. A
policy source with more than one hard link is refused outright, because a
second directory entry is a rewrite path that never touches
`.shipmates`. And because handles are held without `FILE_SHARE_DELETE`,
an attempt to rename or delete a policy file the loader currently holds
open is refused by the kernel rather than detected afterwards — the
after-the-fact identity check is retained anyway.

All the same properties apply to the mutation side
(`project.AcquirePolicyWriteLock`), which holds the exclusive lock across
the complete mutation including any rollback.

`policy.SecureLoadSupported()` reports whether this platform has a real
implementation. A platform with neither gets no snapshot, and callers
that mediate runtime approvals refuse every request rather than waving
any through.

`shipmates init` is all-or-nothing for the artifacts it creates: if any
step fails, every directory and file it created is removed, and anything
that predated the run is left untouched. A failed init cannot leave a
project that looks initialized and is not.

## Offline runtime installer

`sudo shipmates install` is a closed, offline, manifest-verified operation. It
accepts no destination, source, command, service-manager, credential, or Fleet
endpoint override; it performs no internal sudo, network access, credential
read, service start, or qualification. It stages fixed regular assets,
verifies byte digests and modes, fsyncs before activation, refuses drift and
unsafe parents, and retains the install journal/state across uninstall.

Capability detection selects the hardened systemd/M3 asset composition only
when systemd, delegated cgroup v2, pidfd, the pinned launcher, and the required
filesystem conditions are visible. Limited WSL, non-systemd containers, read-
only roots, user namespaces, and missing delegation retain ordinary Shipmates
operation rather than weakening containment. The optional profile is a plan,
not a credential or authority grant. Production M3 remains NO-GO until the
separately authorized unrestricted host qualifier passes.

## Exact-turn authority

Steer, interrupt, and approval bind to an exact persona, project session,
runtime thread/session, and turn — on both runtimes. Local controller
actions also bind to a current lease. Stale identifiers are rejected,
never redirected to current work.

Approvals are single-request decisions. `/allow-once` creates no durable grant.
Timeout, lease loss, policy change, ambiguous delivery, and stale state fail the
request closed.

## Voyage authority

Voyage plans are regular files confined to the canonical project root. Strict
JSON rejects unknown fields and malformed trailing content. Approval is set only
after the skipper shows the complete plan to the human captain. Plan hashing
separates runtime state for every revision.

Before dispatch, `sail` validates the acyclic graph and every installed persona.
Concurrency, task count, prompt size, and task duration are bounded. Downstream
work never runs after dependency failure. State is written atomically with
private permissions, and failure or cancellation cannot become success through
presentation alone.

Beads is optional. If enabled, the external `bd` CLI owns `.beads/` graph
storage and schema; Shipmates passes bounded arguments and stores only opaque
IDs. Ordinary projects do not require Beads.

The planning TUI does not derive authority from conversation text. `/sail`
reloads and validates the on-disk approved plan. Architect consultations remain
advisory, and plan amendments require renewed Captain approval. Captain-input
requests use a bounded final-result marker persisted as `needs_input`; they are
not treated as successful task completion.

## Local server

The server listens on an ephemeral loopback address. Clients discover it through
private authenticated atomic state. Routes are allowlisted for health, shutdown,
live control, normalized feed, and exact local Fleet adapters.

There is no generic terminal, file upload, arbitrary hook, graph mutation,
credential manager, or catch-all process execution endpoint. Loopback is not a
substitute for request authentication.

## File input

Attachments are accepted only from the local filesystem, only inside the
canonical project root, and only as regular files. Shipmates bounds count
(8 per batch) and size (20 MiB per image, 10 MiB per other file, 64 MiB per
batch), rejects symlinks, Windows reparse points, and path escape, pins
filesystem identity, and revalidates immediately before the bytes are read —
so a file swapped between validation and use is refused rather than sent.

Content kind is sniffed from a bounded prefix of the bytes, never inferred
from the extension. Images are recognized by magic bytes; text is valid
UTF-8 with no NUL in that prefix; everything else is binary. Binary files are
never base64-encoded into a prompt — they are referenced by project-relative
path with size and detected kind, so an arbitrary byte stream never enters
the model context. Inlined text is bounded and truncated with a notice.

`shipmates show` may inject an attachment into an already-running turn. That
is a turn-scoped message on the exact tuple captured under the session lock,
so it cannot land on a different turn than the one observed. There is still
no URL fetch, attachment store, upload endpoint, or remote image capability.
Events may report only image count — never a path, a name, or content.

When a coordination server is running it revalidates every supplied path
against its own project root before reading a byte; the client's validation
exists to produce good error messages and carries no authority.

## Fleet separation

Observer credentials read bounded projections and may be scoped to a ship
allowlist. Steer and interrupt use separate short-lived capabilities bound to
operation, ship, opaque active target, deadline, and replay protection. Private
local turn identifiers remain on the ship.

Fleet cannot start work, answer approvals, upload files, open terminals,
broadcast, mutate graphs, or run generic commands.

### M2 local delegation boundary

The local Commander policy is disabled by default and accepts only configured
Ed25519 trust anchors for one exact Fleet and protocol version. It verifies the
closed M1 envelope before binding it to the locally approved voyage, task,
recovery request, and blocker fingerprint. Expiry is checked both at decode and
at the locked durable reservation; revocation before or after assessment is
fail-closed and never grants authority.

The isolated owner-only delegation journal is append-only, bounded, no-follow,
and separate from ordinary recovery state. It records only opaque digests,
fixed lifecycle/reason codes, policy and Skipper provenance, and a
domain-separated provenance digest. A fresh empty Codex home, no inherited
credentials, and an immutable read-only/tool-less overlay constrain the single
Sol advisory. Sail remains the only execution authority: an accepted advisory
does not execute work, modify a plan, write Beads, or establish Fleet
authority. M2 has no transport, listener, public command/API, or remote
credential path; M3 must introduce those as a new reviewed boundary.

## Secrets

- Keep Codex and Fleet secrets outside repository files.
- Prefer environment variables or protected service stores.
- Never place tokens in command arguments, remote URLs, logs, memory, prompts,
  or Git history.
- Give observer and operator identities different credentials.
- Rotate and revoke Fleet credentials independently.
- Treat persona memory and prompts as sensitive project content.

## Threat and response matrix

| Risk | Primary control | Operator response |
|---|---|---|
| Shell injection through prompt | Direct argument execution | Report any shell-mediated launch as a defect |
| Continuity race | Per-persona serialization | Attach or read feed; do not delete live locks |
| Stale control targets new work | Exact immutable tuple | Refresh and intentionally target the new turn |
| Managed file clobber | Manifest hashes and explicit resolution | Inspect and choose ours or theirs |
| Path or symlink escape | Containment and identity checks | Remove unsafe path; do not bypass validation |
| Approval replay | Exact request and lease binding | Re-issue from the current dashboard |
| Fleet replay | Expiry, scope, deduplication, audit | Revoke identity and inspect authority audit |
| Secret leakage in events | Closed normalized projections | Treat raw protocol exposure as an incident |
| Orphaned runtime child (codex or claude) | Cancellation and reaping via the runtime adapter's containment watcher | Verify process owner before cleanup |

## Deliberate non-features

The absence of generic backends, remote task start, remote approvals, terminals,
uploads, hooks, unapproved graph execution, broadcast, rescue, conversation, and voice is a
security property. Adding one requires a new threat model, protocol,
authorization scope, failure policy, tests, and documentation.
