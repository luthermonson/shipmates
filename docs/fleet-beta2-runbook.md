# Fleet v0.4.0-beta.2 runbook

This is the canonical Linux/WSL runbook for the beta.2 Fleet evidence path.
It starts with a public-command bootstrap, observes a real authenticated Codex
turn over verified TLS, then exercises exact-turn steering and interruption.
The later sections cover credential lifecycle, recovery, containerized
cross-node WSL evidence, the read-only UI, bounded fake-Codex checks, and
teardown.

Fleet remains deliberately narrow: it observes bounded state and events and
steers or interrupts one already-active turn. It cannot start work, approve a
request, open a terminal, upload or transfer data, mutate a graph, broadcast,
rescue, converse, speak, schedule, or execute a generic command.

## Evidence labels and prerequisites

All commands below assume Linux, including WSL, a `shipmates` binary on
`PATH`, Codex CLI 0.144.1 (or the release-qualified version), OpenSSL, and a
Git project. The real-Codex path requires `codex login status` to report an
authenticated account. Use a CA-signed certificate or a certificate issued by
an internal CA already trusted by the Linux host; verify the hostname/SAN and
expiry before starting the observer. Do not use `curl -k`, disable hostname
verification, or put secrets in arguments, URLs, logs, reports, or the
repository.

The commands use placeholders such as `<fleet-id>` and `<ship-id>` for
sanitized values. Replace them only with values returned by the preceding
public command. Keep all secret-bearing files outside the project, mode 0600,
and use absolute paths. The commands print metadata only; do not paste their
secret files into evidence.

Reports 01 and 02 record the completed unrestricted UbuntuRojo real-Codex/TLS
observation and exact-turn control proof. Report 05 records the completed
Docker bridge, two-container WSL/Linux proof. Reports 03 and 04 record the
security and deterministic recovery review. Report 06 is bounded UI
source/unit evidence, not live browser inspection. Report 07 is a completed
30-minute synthetic fake-Codex soak, not a real-model soak. Reports 09 record
the current unrestricted UbuntuRojo beta.2 release-gate PASS and the separate
managed-sandbox listener/full-suite limitation; the latter does not invalidate
the host evidence.

## 1. Prepare external state and trusted TLS

Run from the project, but keep every state and secret path outside it:

```bash
set -eu
PROJECT=$(git rev-parse --show-toplevel)
STATE=$(mktemp -d /tmp/shipmates-fleet-beta2.XXXXXX)
chmod 700 "$STATE"
AUTHORITY="$STATE/authority"
SHIP_IDENTITY="$STATE/ship-identity.json"
ENROLLMENT="$STATE/enrollment.json"
OBSERVER_CREDENTIAL="$STATE/observer.credential"
STEER_CREDENTIAL="$STATE/steer.credential"
INTERRUPT_CREDENTIAL="$STATE/interrupt.credential"
FLEET_URL='https://fleet.example.test:8443'
FLEET_ID='<fleet-id>'
```

Before proceeding, verify that the certificate presented at
`fleet.example.test:8443` chains to the intended trusted CA, names the exact
host, and is within its validity period. The observer command below receives
the certificate and private key as protected filesystem paths. The TLS private
key is never a command argument value in evidence; `<tls-key>` denotes a
protected path.

## 2. Bootstrap authority, ship identity, and credentials

Initialize the durable authority and create one-use enrollment material:

```bash
shipmates fleet init \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID"

shipmates fleet enrollment create \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --ttl 15m --output "$ENROLLMENT"
```

Consume the artifact into an external ship identity store. Use the Fleet TLS
URL and the service identity that the certificate verifies:

```bash
shipmates fleet enrollment consume \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --enrollment-file "$ENROLLMENT" --identity-store "$SHIP_IDENTITY" \
  --fleet-destination "$FLEET_URL" \
  --fleet-service-identity fleet-service
```

The enrollment file is single-use and is removed after successful file
consumption. Capture only the returned `<ship-id>` and metadata. Issue
independent observer, steer, and interrupt credentials; the latter two need
distinct operator subject names and ship scope:

```bash
shipmates fleet credential issue \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind observer --ship-id '<ship-id>' \
  --ttl 1h --output "$OBSERVER_CREDENTIAL"

shipmates fleet credential issue \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind steer --subject-id '<steer-operator>' --ship-id '<ship-id>' \
  --ttl 15m --output "$STEER_CREDENTIAL"

shipmates fleet credential issue \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind interrupt --subject-id '<interrupt-operator>' --ship-id '<ship-id>' \
  --ttl 15m --output "$INTERRUPT_CREDENTIAL"
```

Verify permissions and that output contains metadata rather than secret
material. `credential inspect` is also metadata-only:

```bash
test "$(stat -c '%a' "$OBSERVER_CREDENTIAL")" = 600
test "$(stat -c '%a' "$STEER_CREDENTIAL")" = 600
test "$(stat -c '%a' "$INTERRUPT_CREDENTIAL")" = 600
shipmates fleet credential inspect --authority-store "$AUTHORITY" \
  --fleet-id "$FLEET_ID" --kind observer --credential-id '<observer-id>'
```

## 3. Start the observer and an active real-Codex turn

Start the Fleet observer with durable authority and the verified certificate:

```bash
shipmates fleet serve-observer \
  --addr 0.0.0.0:8443 --authority-store "$AUTHORITY" \
  --fleet-id "$FLEET_ID" --fleet-epoch '<fleet-epoch>' \
  --steer-epoch 1 --service-identity fleet-service \
  --tls-cert '<tls-cert>' --tls-key '<tls-key>'
```

For a single-host proof, bind `127.0.0.1:8443` and use a certificate whose
SAN is `127.0.0.1`; for containerized cross-node proof, bind the Fleet
container interface and use a trusted DNS name. Do not expose the Ship
container or project coordination server inbound.

In the project, start one deliberately long, harmless, no-tool Codex turn
using the public command. Do not ask it to edit files:

```bash
shipmates live '<persona>' \
  'Wait without using tools until the operator ends this harmless test turn.'
```

In another terminal, start the project’s outbound observer service with the
identity created above:

```bash
shipmates ship observe --project "$PROJECT" \
  --identity-store "$SHIP_IDENTITY" --steer-epoch 1
```

## 4. Observe through the public read-only surface

Use the observer credential only for reads. The first successful outputs prove
that the enrolled ship is connected and that the public projection is bounded:

```bash
export SHIPMATES_OBSERVER_URL="$FLEET_URL"
export SHIPMATES_OBSERVER_CREDENTIAL_FILE="$OBSERVER_CREDENTIAL"

shipmates fleet ships --json
shipmates fleet status '<ship-id>' --json
shipmates fleet events --limit 100 --json
shipmates fleet follow --limit 100 --json
```

Record sanitized roster/status/event results and the follow cursor, never raw
prompts, tool arguments, credentials, local session/thread/turn identifiers,
paths, or process output. The human-readable `session` and `turn` fields are
bounded status labels, not private identifiers.

The embedded UI is read-only. Open the same verified HTTPS origin in a
supported Linux browser, enter the observer credential without saving it, and
confirm ship status, bounded events, stale/offline state, cursor-gap recovery,
keyboard focus, and narrow/wide layout. The UI must show no start, approve,
steer, interrupt, shell, upload, transfer, or generic command control.

## 5. Discover and exercise exact-turn control

Use separate capability URLs/files and discover fresh opaque targets. Do not
construct target references from local IDs:

```bash
export SHIPMATES_OPERATOR_URL="$FLEET_URL"
export SHIPMATES_OPERATOR_CREDENTIAL_FILE="$STEER_CREDENTIAL"
shipmates fleet steer-targets --json

export SHIPMATES_INTERRUPT_URL="$FLEET_URL"
export SHIPMATES_INTERRUPT_CREDENTIAL_FILE="$INTERRUPT_CREDENTIAL"
shipmates fleet interrupt-targets --json
```

The two outputs must contain distinct, short-lived opaque target references.
The observer credential and the opposite operation credential must be refused
before target disclosure. Using the exact values from the matching target
view, submit one harmless message through stdin and then one confirmed
interrupt:

```bash
printf '%s\n' 'Please continue waiting; this is the single control proof.' |
  shipmates fleet steer --fleet "$FLEET_URL" \
    --credential-file "$STEER_CREDENTIAL" --fleet-id '<fleet-id>' \
    --fleet-epoch '<fleet-epoch>' --ship '<ship-id>' \
    --connection-generation '<generation>' --persona '<persona>' \
    --target '<steer-target>' --message-stdin --json

shipmates fleet interrupt --fleet "$FLEET_URL" \
  --credential-file "$INTERRUPT_CREDENTIAL" --fleet-id '<fleet-id>' \
  --fleet-epoch '<fleet-epoch>' --ship '<ship-id>' \
  --connection-generation '<generation>' --persona '<persona>' \
  --target '<interrupt-target>' --confirm --json
```

Expected evidence is one accepted steer, one interrupted turn, a local
terminal `turn.interrupted` event with operator reason, and no private tuple in
Fleet payloads. Do not retry an indeterminate operation with a new operation;
use the exact interrupt operation ID and the public retry/query mechanism if
the command returns an indeterminate outcome.

## 6. Rotate, commit, expire, and revoke credentials

Rotate one operator credential into a new protected file, prove the replacement
while the configured overlap remains, then commit the generation. Repeat
separately for ship credentials when rotating a ship identity:

```bash
NEW_STEER_CREDENTIAL="$STATE/steer-rotated.credential"
shipmates fleet credential rotate \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind steer --credential-id '<old-steer-id>' --generation '<old-generation>' \
  --overlap 5m --ttl 15m --output "$NEW_STEER_CREDENTIAL"

shipmates fleet credential commit \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind steer --credential-id '<new-steer-id>' --generation '<new-generation>'
```

Revoke old, compromised, expired, or no-longer-needed credentials explicitly:

```bash
shipmates fleet credential revoke \
  --authority-store "$AUTHORITY" --fleet-id "$FLEET_ID" \
  --kind steer --credential-id '<old-steer-id>' --generation '<old-generation>'
```

Verify that revoked credentials fail, that an established tunnel closes when
its ship credential is revoked, and that current credentials remain scoped.
Never print credential file contents. Remove old protected files after the
replacement is committed and verified.

## 7. Recovery and reconnect

Use public lifecycle commands and the existing external stores. If the ship or
local project server restarts, do not edit identity state or target references:

```bash
shipmates server stop
shipmates ship observe --project "$PROJECT" \
  --identity-store "$SHIP_IDENTITY" --steer-epoch 2
shipmates fleet ships --json
shipmates fleet follow --after '<last-known-cursor>' --limit 100 --json
```

Confirm the same enrolled ship returns online with a new connection generation
and that old target references fail closed. On an epoch gap or cursor gap,
accept the authoritative resnapshot, then discover new targets. A lost control
response is indeterminate; reconcile with the exact public operation rather
than submitting a new one.

## 8. Containerized cross-node WSL evidence

This is a separate, bounded Docker-on-WSL/Linux qualification. Run a Fleet
container with the durable authority and trusted TLS listener and a separate
Ship container with the project, ship identity, local coordination server, and
real authenticated Codex runtime. Use a private Docker bridge; publish only
the Fleet endpoint to the host. The Ship container initiates outbound tunnel
connections and publishes no host port.

Mount Codex authentication and configuration read-only at runtime, copy them
only into a mode-0600 tmpfs-backed Codex home, and keep them out of images,
layers, build context, logs, volumes, arguments, and reports. Repeat sections
2–7 with the same public `shipmates` commands, then restart only the Ship
container and verify identity-preserving reconnect and generation advancement.

Report 05 completed this containerized Ubuntu 24.04/WSL2 Docker Engine 29.1.3
scenario: observation, one steer, one interrupt, private-identifier absence,
and reconnect passed. It is not physical multi-host qualification. No claim is
made for two physical hosts, production network policy, external DNS, or a
real-world certificate authority deployment beyond the bounded container
fixture.

## 9. Bounded load, soak, and release contract

Deterministic tests use fake-Codex-shaped state and do not prove real model or
TLS behavior. The completed synthetic soak is:

```bash
GOCACHE=/tmp/shipmates-gocache SHIPMATES_SOAK_DURATION=30m \
  go test -v ./internal/fleetobserve \
  -run '^TestSyntheticFleetGentleSoak$' -count=1 -timeout=35m
```

Report 07 records PASS: 30m0s, goroutines 2→2, file descriptors 9→9, and
allocated bytes 918096 baseline / 3869800 peak / 843072 final. Its ceilings
cover two ships, bounded event ingress, replay, heartbeats, reconnects, pages,
payload sizes, and exact-turn fake-control admission. The GET/poll observer
has no live subscriber-admission mechanism; that residual coverage gap remains
documented. Do not call this real-Codex evidence or rerun it merely to produce
new release text.

The beta.2 real-Codex gates are the unrestricted public-command TLS proof in
reports 01–02, the containerized cross-node proof in report 05, and the
authoritative unrestricted host release gate in report 09. Report 09 records
PASS for the full normal/race suites, vet, formatting, runtime closure, fresh
public TLS observation, exact-turn control, privacy, revocation, and restart
closure. Its companion report also records that the managed worker cannot bind
listeners and therefore cannot reproduce those live assertions; its three
managed listener/server-health failures are environment limitations, not
passing evidence. The focused UI, recovery, and fake-Codex tests are
deterministic support evidence. Physical multi-host qualification, live browser
accessibility matrix, and any real-Codex load/soak are not complete.

The repository release process is tag-driven: the release workflow runs on a
`v*` tag, verifies formatting/tests/race/vet/runtime closure on Ubuntu, and
invokes GoReleaser with the tag version. For beta.2, the Captain may create
`v0.4.0-beta.2` only after reviewing the evidence and unresolved gates. This
runbook does not create a tag, publish artifacts, or claim that the workflow
has run.

## 10. Teardown

Stop the observer and ship processes with their normal process controls, stop
the project server through its public command, revoke temporary credentials,
and remove only the external temporary state after evidence is sanitized:

```bash
shipmates server stop || true
shipmates fleet credential revoke --authority-store "$AUTHORITY" \
  --fleet-id "$FLEET_ID" --kind observer --credential-id '<observer-id>' || true
shipmates fleet credential revoke --authority-store "$AUTHORITY" \
  --fleet-id "$FLEET_ID" --kind steer --credential-id '<steer-id>' \
  --generation '<steer-generation>' || true
shipmates fleet credential revoke --authority-store "$AUTHORITY" \
  --fleet-id "$FLEET_ID" --kind interrupt --credential-id '<interrupt-id>' \
  --generation '<interrupt-generation>' || true
rm -rf -- "$STATE"
```

For the container fixture, remove the Fleet and Ship containers, private
bridge, temporary certificate/key, runtime Codex home, and mounted state after
the processes are stopped. Confirm that no Fleet, Shipmates, Codex, or Docker
process remains and that no secret-bearing file or raw feed was retained.
