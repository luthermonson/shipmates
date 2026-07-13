# Fleet architecture

Shipmates Fleet observes projects and controls one already-active Codex turn. It
is deliberately narrower than a remote orchestration platform.

## Components

`shipmates ship observe`
: Opens an outbound authenticated WebSocket and publishes bounded snapshots,
  normalized lifecycle events, and heartbeats.

`shipmates fleet serve-observer`
: Hosts the authority, read-only API, and embedded UI. Production use requires
  TLS and durable identity, capability, revocation, replay, and audit storage.

Fleet clients
: Read ships, snapshots, and events with observer credentials. Exact-turn steer
  and interrupt use separate operator capabilities.

Local adapter
: Resolves an opaque Fleet target to one exact active turn. Private project
  session, Codex thread, and turn identifiers remain ship-local.

## Data flow

```text
Codex app-server
    | normalized events
    v
Shipmates project server
    | outbound authenticated tunnel
    v
Fleet observer/authority ----> read-only browser and CLI clients
    | scoped exact-turn operation
    v
same outbound tunnel ----> local exact-target adapter
```

The ship initiates the connection, so projects need no inbound listener. Tunnel
messages use a closed schema, bounded payloads, deadlines, and deduplication.

## Identity and authorization

- Ship identity authenticates one enrolled project endpoint.
- Observer identity reads bounded state and may have a ship allowlist.
- Operator authority issues short-lived steer or interrupt capabilities.
- An operation capability authorizes one operation on one opaque target before
  one deadline.

Read credentials cannot become control. Steer cannot interrupt. Expired,
revoked, replayed, mismatched, or ambiguous operations fail closed.

## Published data

Fleet may receive public ship identity, bounded lifecycle state, sanitized
normalized events, health timestamps, and sanitized operation audit. It does
not receive raw prompts, tool arguments, credentials, policy source, private
local tuples, image paths or bytes, process output, or filesystem contents.

## Exact-turn control

Steer and interrupt apply only to existing work. The ship resolves the opaque
target against current local state immediately before action. Completion,
replacement, deadline expiry, or mismatch rejects the operation rather than
retargeting it. Audits omit unrestricted message and tool content.

## Observer UI

The embedded UI is read-only. It uses bounded APIs, caps retained events,
renders untrusted content as text, and does not persist credentials. Credential
input, status regions, headings, tables, and controls support keyboard and
assistive-technology use.

## Deployment requirements

- Terminate TLS at the observer or a trusted reverse proxy.
- Use durable authority storage for identities, revocation, replay, and audit.
- Keep ship, observer, and operator credentials separate.
- Restrict observer identities to the smallest useful ship set.
- Supervise ship services with a usable Codex environment.
- Monitor tunnel health, rejected operations, revocations, and audit durability.
- Test rotation, reconnect, replay rejection, and bounded shutdown.

See [Operations](operations.md) and [Security](security.md).

## Excluded authority

Fleet has no remote start, approval, transfer, upload, hook, terminal, graph
dispatch, scheduler, rescue, broadcast, conversation, speech, or generic command
execution. Local approval belongs only to the exact dashboard controller.
