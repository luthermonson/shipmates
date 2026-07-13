# Security model

Shipmates coordinates privileged local AI work. Its model is narrow authority,
exact targets, conservative filesystem ownership, and closed protocols. It does
not make an untrusted repository or host safe.

## Trust boundaries

The operator trusts the local OS account and filesystem, installed Shipmates and
Codex binaries, project content exposed by the Codex sandbox, the configured
OpenAI account, and any enabled Fleet authority. Persona separation provides
distinct instructions, memory, policy, and continuity; it is not an OS security
boundary.

## Codex execution

Shipmates starts known Codex entry points directly without shell interpolation.
Persona configuration may select Codex model and reasoning effort, but cannot
name an arbitrary executable. One-shot dispatch consumes structured JSON events;
live dispatch uses a bounded app-server adapter.

Raw frames, prompts, unrestricted tool arguments, credentials, and Codex stderr
are excluded from public event projections. Unknown protocol shapes fail closed.
The effective Codex sandbox and approvals still matter; review them before work
in repositories containing secrets or production credentials.

## Filesystem ownership

Manifest baselines prevent silently overwriting or deleting edited managed
files. Sensitive state is created with restrictive permissions and checked for
unsafe type, links, ownership, identity change, and path escape. Multi-file
operations validate before publishing a new manifest and roll back before their
commit boundary when possible.

Memory is user-owned durable content. Ordinary removal preserves it; `--purge`
is the explicit destructive boundary.

## Exact-turn authority

Steer, interrupt, and approval bind to an exact persona, project session, Codex
thread, and turn. Local controller actions also bind to a current lease. Stale
identifiers are rejected, never redirected to current work.

Approvals are single-request decisions. `/allow-once` creates no durable grant.
Timeout, lease loss, policy change, ambiguous delivery, and stale state fail the
request closed.

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
| Orphaned Codex child | Cancellation and reaping | Verify process owner before cleanup |

## Deliberate non-features

The absence of generic backends, remote task start, remote approvals, terminals,
uploads, hooks, graph execution, broadcast, rescue, conversation, and voice is a
security property. Adding one requires a new threat model, protocol,
authorization scope, failure policy, tests, and documentation.
