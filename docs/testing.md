# Autonomous Fleet testing

Shipmates' Fleet acceptance suite runs locally and in CI without external AI,
audio, task-graph, or cloud services. It uses the production Fleet HTTP mux and
a real authenticated remotedialer websocket. Only the captain and optional
upstream services are deterministic in-process fakes.

## Single-plan voyage contract

The primary acceptance criterion is one plan completing a whole voyage:

```text
discovered -> assigned -> delivered -> blocked -> approved
           -> working -> completed -> closed + done + event evidence
```

`TestAutonomousFleetSinglePlanVoyageSuccess` verifies every transition through
operator-visible interfaces:

| Stage | Evidence |
|---|---|
| Discovered | Plan appears in `GET /api/beads`. |
| Assigned | Fleet updates the bead to `builder@voyager`. |
| Delivered | The mate receives a dispatch containing `bd show voyage-001`. |
| Blocked | Fleet-wide pending and status APIs expose the safety gate. |
| Approved | The operator resolves the gate through Fleet Command. |
| Working | Fleet status changes to `working`. |
| Completed | The fake mate closes the plan and changes to `done`. |
| Observable | Open-plan aggregation is empty, bead detail is closed, and SSE emits `voyage:completed`. |

A successful HTTP response alone is not voyage success. The graph, status, and
event stream must converge on the same completed outcome.

## Coverage layers

The autonomous suite has three top-level tests:

- `TestAutonomousFleetSinglePlanVoyageSuccess` - critical-path acceptance.
- `TestAutonomousFleetFeatureMatrix` - authentication, registry, direct and
  aggregated bead reads/mutations, policy, PTY controls and streaming,
  attachments and auto-tell, voice capabilities, OpenAI-compatible tool
  calling, STT, TTS, and beads nudge.
- `TestAutonomousFleetFailurePaths` - invalid plan IDs, unknown captains,
  offline dispatch queuing, disabled optional services, tunnel disconnect, and
  the correct gateway status after loss of connectivity.

Existing package tests continue to cover lower-level permission rules, event
cursors, attachment bounds, policies, PTY state, parsers, and session metadata.

## Running it

Linux, macOS, or WSL:

```sh
bash scripts/test-fleet-autonomous.sh
```

PowerShell:

```powershell
./scripts/test-fleet-autonomous.ps1
```

Direct Go invocation:

```sh
go test -count=1 -timeout=90s -run '^TestAutonomousFleet' -v ./internal/fleet
```

Both scripts emit Go's structured JSON event stream. Set
`SHIPMATES_TEST_REPORT` to choose its destination. The default is the operating
system temporary directory, so autonomous runs never dirty the repository.

## Diagnostics

The JSON report preserves test names, timestamps, logs, elapsed time, and the
first failing assertion. CI uploads it even when the voyage succeeds. A failure
can normally be classified by its last completed contract stage:

- no connected captain: websocket/auth/registry boundary;
- plan discovered but not assigned: bead proxy or validation;
- assigned but not delivered: dispatch or tunnel routing;
- blocked but not working: pending/resolve path;
- working but not closed: mate completion behavior;
- closed but no SSE evidence: event cursor or stream proxy;
- correct captain state but wrong aggregate: Fleet fan-out/merge logic.

When extending Fleet Command, add the new surface to the feature matrix. When
changing dispatch semantics, update the voyage contract first so success
remains an externally observable product outcome rather than an internal call
sequence.
