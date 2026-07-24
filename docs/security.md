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

The `runtime.claude` adapter spawns `claude -p --session-id
<uuid> --output-format stream-json` per turn and parses the JSONL event
stream into the normalized `runtime.Event` channel. Session state is
scoped to the runtime and cleaned up on `Close`. As with the codex path,
the persona instructions never enter a shell, and only normalized events
are exposed to feeds and Fleet.

Shipmates does not silently fall back from one runtime to another. If the
configured runtime's CLI is missing or unauthenticated, dispatch fails
closed with an actionable error rather than trying the other runtime.

## Filesystem ownership

Manifest baselines prevent silently overwriting or deleting edited managed
files. Sensitive state is created with restrictive permissions and checked for
unsafe type, links, ownership, identity change, and path escape. Multi-file
operations validate before publishing a new manifest and roll back before their
commit boundary when possible.

Memory is user-owned durable content. Ordinary removal preserves it; `--purge`
is the explicit destructive boundary.

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

Steer, interrupt, and approval bind to an exact persona, project session, Codex
thread, and turn. Local controller actions also bind to a current lease. Stale
identifiers are rejected, never redirected to current work.

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

## Image input

Images are accepted only at local turn start. They must be regular PNG, JPEG,
GIF, or WebP files inside the canonical project root. Shipmates bounds count and
size, rejects links and escape, checks magic bytes, pins filesystem identity,
and revalidates before handoff.

There is no URL fetch, attachment store, remote image capability, arbitrary file
input, or mid-turn image steering. Events may report only image count.

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
