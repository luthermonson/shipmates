# Sailing projects

**Runtime scope.** Sail is codex-native for this release. It launches the
`codex app-server` transport, uses PID-file dispatch locks with unix
signal semantics, and mediates every turn through the managed Codex
adapter. The claude runtime cannot yet drive Sail; when the voyage
executor is migrated onto the `runtime.Runtime` interface (see
[Runtime interface plan](runtime-interface-plan.md)), the same commands
below will accept `--runtime claude`. Until then, `shipmates sail` on
Windows returns a clear unix-only error rather than hanging; see
[Platform support](platform-support.md).

Phase 2 reserves **captain** for the human operator. The **skipper** is the
human-facing execution lead, the **quartermaster** preserves strategic memory
and constraints, and specialist personas perform implementation and review.

## Workflow

1. Install the leadership roles and crew:

   ```bash
   shipmates add quartermaster
   shipmates add skipper
   shipmates add backend
   shipmates add tester
   ```

2. Open the Captain's planning room:

   ```bash
   shipmates plan
   ```

3. Refine the plan until scope, dependencies, acceptance criteria, and risk are
   explicit. The skipper writes `.shipmates/voyage.json` with approval disabled.
4. Review the complete displayed plan. Explicitly tell the skipper it is ready
   to sail. Only then may the skipper set `approved` to `true`.
5. Enter `/sail` in the planning room. For headless operation, leave the
   conversation and start execution with:

   ```bash
   shipmates sail
   ```

   If a previous run preserved failed tasks after an infrastructure problem,
   review the failure and enter `/sail --retry-failed` to resume them.

   Use `/sail --verbose` for a transparent operations-room view. It displays
   each assigned task prompt, model and effort, agent reports, file-change
   details, and exact commands exposed by Codex. Because command arguments may
   contain sensitive values, verbose execution is explicit rather than the
   default. Combine it with recovery as `/sail --retry-failed --verbose`.

The skipper never launches Sail from its managed Codex turn. `/sail` is a local
TUI command dispatched by the host process, avoiding unsupported nested Codex
sessions.

## Skipper-first recovery and optional auto-captain

Sail is the authority for execution. The human operator remains Captain and
must approve the original plan before dispatch; Skipper plans, presents status,
and returns control to the Captain. An optional auto-captain stage is an
advisory Sol turn that helps classify a bounded blocker. Sol cannot approve or
edit a plan, complete a task, mutate predecessor state or Beads, dispatch a
successor, lower acceptance criteria, issue credentials, or widen scope.

Enable it explicitly in `shipmates.yaml`. Sail snapshots the configured
Skipper persona and applies the fixed `gpt-5.6-sol` model override; no `sol`
persona is required. The
continuation envelope is optional but recommended when unattended bounded
recovery is wanted:

```yaml
recovery:
  autoCaptain: true
  continuationEnvelope:
    enabled: true
    maxAttempts: 2
    maxAssessments: 1
    expiresAt: "<future RFC3339 timestamp>"
    allowedReasons:
      - ordinary_failure
      - uncertainty
      - stale_continuity
      - contradictory_evidence
      - exhausted_tiers
      - managed_environment_mismatch
```

The configuration is strict. `maxAttempts` and `maxAssessments` are each
bounded to 1–8, the expiry must be in the future, and reason codes are
allowlisted. `autoCaptain: true` without an envelope enables the per-blocker
deduplication contract but does not create an unbounded retry budget. Sail
refuses to start the stage if `sol` is not installed. Keep the expiry short and
choose only reasons covered by the original approval.

Normal task execution still consumes the approved model/effort ladder in order.
For example, a retry-safe task configured with Luna/Terra tiers exhausts those
ordinary retries before Sail asks Sol once about the resulting blocker. If Sol
is also listed as a task model, that Sol crew tier runs as an ordinary task
retry before the separate advisory stage; auto-captain is never a replacement
task or a completion signal. A Sol advisory turn is fresh, uses the fixed
`gpt-5.6-sol`/high configuration, receives only a bounded recovery request, and
runs with read-only Codex sandbox mode.

Recovery reason codes are stable machine values:
`ordinary_failure`, `uncertainty`, `stale_continuity`,
`contradictory_evidence`, `exhausted_tiers`,
`managed_environment_mismatch`, `physical_access_unavailable`,
`human_credential_required`, and `captain_decision_required`. The blocker
fingerprint is derived from plan/task provenance, the task contract, reason,
attempt/tier facts, and redacted evidence digests. Raw prompts, paths,
credentials, private IDs, backend payloads, and failure text are not journal
fields. The same stable fingerprint receives at most one Sol assessment until
evidence materially changes; a dispatch crash before the assessment is durable
may safely retry it.

The recovery journal is append-only bounded JSONL under
`.shipmates/recovery/<plan-hash-prefix>.jsonl`. Blocker, assessment,
attestation, and accepted-action records are sequence-numbered and synced
before acknowledgement. Malformed, duplicate, oversized, world-accessible, or
symlinked journals fail closed. Evidence is retained as redacted source/code/
digest references. Host or CI results must be independently verified and
referenced by the Captain or an authorized release process; this recovery path
does not cryptographically attest external artifacts and Sol responses never
count as external proof.

The possible outcomes have different authority implications:

- `resume` or `continue`: Sail may reset the affected task to pending and
  continue within the already approved plan and bounded envelope. Use ordinary
  resume for a restart or safe retry; completed tasks remain complete.
- A pre-authorized successor is an already-written, separately Captain-
  approved plan. Run it only by naming the exact predecessor plan and state;
  Sail validates immutable hashes, task/closure fingerprints, and completion
  evidence before publishing successor state. Auto-captain cannot create or
  approve this successor.
- `amendment_required`: Sail records `needs_input`, leaves the proposal inert,
  and returns the planning UI to Captain-Skipper chat. The Captain must review
  and approve any amended plan before `/sail` or a successor command can run.
- `stop`: verified physical access, human-only credential/consent, or another
  genuinely human-only boundary remains. Sail stops with one concise question;
  ordinary failure, uncertainty, exhausted tiers, stale continuity,
  contradictory evidence, and recoverable environment mismatch are not
  human-only by themselves.

Safe operational examples use placeholders only:

```bash
# Inspect and validate the Captain-approved plan without dispatching.
shipmates sail --plan .shipmates/voyage.json --dry-run

# Resume persisted running work after a process restart.
shipmates sail --plan .shipmates/voyage.json

# Retry failed, blocked, or needs-input tasks only after reviewing state.
shipmates sail --plan .shipmates/voyage.json --retry-failed

# Preview, then execute, an explicitly approved successor amendment.
shipmates sail --dry-run --plan .shipmates/successor.json \
  --predecessor-plan .shipmates/original.json \
  --predecessor-state .shipmates/voyages/<original-hash>.json
shipmates sail --plan .shipmates/successor.json \
  --predecessor-plan .shipmates/original.json \
  --predecessor-state .shipmates/voyages/<original-hash>.json
```

Do not put credentials or raw external evidence in a plan, prompt, Bead note,
or command argument. Use protected external stores and sanitized evidence
references. A task retry starts from its task-scoped contract and persisted
state; it does not delete persona memory. The Sol recovery request itself is
always a fresh advisory turn. Existing crew continuity is otherwise governed
by the normal managed-session configuration and stale-session checks; the
recovery contract does not promise arbitrary thread replacement.

The plan sidebar and Sail output show `AUTO-CAPTAIN STATE`, the Sol model,
`authority: Sail, not Sol`, successor/predecessor hashes, blocker reason,
short fingerprint, assessment count, recommendation, evidence digest, and
whether an amendment still needs Captain approval. Beads-enabled voyages show
opaque task Bead IDs. Sail creates and links task Beads before dispatch,
updates lifecycle status, and preserves inherited Bead IDs in successor
provenance; predecessor plans, states, and Beads remain read-only. On restart,
running tasks return to pending, completed tasks are not rerun, and recovery
journal sequence/count state is restored before a new assessment is eligible.

The plan sidebar joins the approved plan with its matching persisted voyage
state. Completed voyages show each job as completed, while acceptance is
rendered only from the separate verdict described below. Beads-enabled voyages
also show each task's opaque Bead ID. Use Page Up/Page Down to move through long plans;
Home and End jump to the beginning and end of the sidebar.

## M2 local Fleet Commander delegation

M2 is the ship-local implementation of the frozen [M1 wire
contract](contracts/fleet-commander-m1.md); it is not Fleet transport. It is
disabled unless `recovery.commanderDelegation.enabled` is true and the local
policy names the expected Fleet, protocol version, bounded offer lifetime, and
permitted Commander Ed25519 public keys. Fleet enrollment, tunnel credentials,
and a future bidirectional substream do not enable this policy.

When a local integration supplies a signed `fleet.delegation-envelope.v1`, the
processor performs this ordered lifecycle:

1. Parse closed JSON, reject duplicate/trailing/unknown fields, verify the
   configured issuer and the frozen JCS/domain-separated Ed25519 signature.
2. Bind Fleet, ship, voyage-plan, task-contract, state, task, blocker, mode,
   response schema, expiry, and one-assessment budget to the already validated
   local recovery case. Any mismatch is rejected before Sol.
3. Under an owner-only lock, append `accepted` and `assessment_started` to the
   plan-hash journal. The expiry is checked again at this durable transition;
   concurrent processors cannot reserve a second turn.
4. Run one fresh pinned `gpt-5.6-sol` Skipper snapshot with an empty temporary
   `CODEX_HOME`, no inherited credentials, a read-only/tool-less overlay, and
   only bounded `recovery.RequestV1` JSON. Sol receives no envelope references,
   prompts, paths, commands, or raw evidence.
5. Sail validates the bounded `recovery.ResponseV1` and exact blocker
   fingerprint. A valid advisory is recorded as
   `locally_accepted_under_existing_policy`; it is accepted advice, never
   executed work or Fleet authority.
6. Append one redacted terminal decision provenance record. It contains bounded
   digests, enums, policy/model/Skipper provenance, and a provenance digest;
   never raw Sol output, credentials, private IDs, paths, or artifact payloads.

The journal is `.shipmates/delegations/<full-voyage-plan-sha256>.jsonl`, with a
matching private lock file. It is append-only, capped at 4 MiB/256 records,
owner-only, no-follow, and isolated from `.shipmates/recovery/`. A malformed,
oversized, duplicate, symlinked, or permission-widened journal fails closed.
After restart, `assessment_started` is indeterminate and is never rerun;
identical completed delivery replays the retained redacted outcome, while the
same delegation ID with a different digest is an `id_conflict`. Revocation or
expiry before assessment yields no Sol turn; revocation after start is terminal
and non-authorizing.

There is intentionally no public M2 command or diagnostic endpoint. Operators
diagnose through normal Sail output and the protected journal path; bounded
codes include `opt_in_disabled`, `provenance_mismatch`, `expired`,
`issuer_revoked`, `revoked`, `response_invalid`, `restart_after_assessment`,
and `id_conflict`. To disable or roll back M2, set `enabled: false`, stop
starting new local delegation processing, and preserve the journal for
investigation. Existing records do not authorize work after disablement.

M2 does not add Fleet transport, a listener, a CLI/API, remote start or
approval, planning, answers, steer/interrupt, Beads/voyage mutation,
derivative activation, shell/terminal, file or artifact transfer, web UI, or
Playwright history. M3 may add the separately reviewed transport and mailbox
composition; it must retain M1 schemas and M2's local validation boundary.

## Beads integration

Install the external `bd` CLI and initialize the project once with
`shipmates beads init`. Beads is optional: projects without `.beads/` retain the
ordinary voyage-state workflow. When enabled, Sail:

1. Creates one Bead for every voyage task and stores only its ID in voyage state.
2. Adds Beads dependency edges matching the approved voyage DAG.
3. Marks a task in progress when its Codex crew turn starts.
4. Injects bounded `bd prime` guidance and the task's current Bead record.
5. Adds the final crew report and closes successful work, or marks terminal
   failures and Captain-input blockers as blocked.

When Beads is enabled, creation and dependency linking are durable
prerequisites: Sail stops before dispatch if it cannot establish the graph. The
one exception is a dependency edge onto an **inherited** prerequisite. That Bead
belongs to the predecessor voyage, is read-only, and may not be resolvable in
this workspace at all — a predecessor state carried in from another checkout, or
a `bd prune`/`bd gc` that reclaimed the closed Bead. Real `bd` rejects
`bd dep add <id> <unresolvable-id>`, so that edge is recorded when the
predecessor Bead is present and skipped when it is not; lost provenance never
refuses a successor voyage. Without Beads, no external graph is required. Later
status synchronization is best-effort and visibly warns without rewriting
successful code work as failed. Beads owns its Dolt database, schema,
synchronization, and CLI behavior; Shipmates does not vendor or reproduce them.

`bd` is resolved from `PATH` only. A file named `bd` next to the `shipmates`
executable is deliberately **not** used: that would turn any writable
install directory into an implicit code-execution path for a binary the operator
never chose to install.

### Verified `bd` version and output contract

Shipmates parses `bd`'s stdout to learn a new Bead's ID, so `bd`'s output is an
external contract. The integration is verified against **`bd` 1.1.2** (upstream
[gastownhall/beads](https://github.com/gastownhall/beads), Go, Dolt-backed,
prebuilt release archives for Linux and Windows). What was confirmed by running
the real binary on both platforms:

| Call | Real `bd` 1.1.2 behavior |
| --- | --- |
| `bd create --json …` | single JSON **object** with an `"id"` field; ids look like `ship-8he` |
| `bd show <id> --json` | JSON **array** of one record, not an object |
| `bd prime` | markdown workflow guidance, ~4.7 KiB in a fresh workspace |
| `bd update <id> --status=…` | `in_progress` and `blocked` are real built-in statuses (`bd statuses`) |
| `bd close <id> --reason=…` | refuses while blockers are still open; Shipmates closes dependency-first and does not pass `--force` |
| `bd comments add <id> --author=… -- <text>` | `--` end-of-options sentinel is honored |
| `bd dep add <id> <depends-on>` | fails when either id does not resolve |

Two things worth knowing about the external binary: `bd init` creates and commits
to a git repository, and `bd` reports anonymous usage metrics to a network
endpoint unless `bd metrics off` has been run.

The version, the pinned release digests in `.github/workflows/test.yml`, and the
`Version` constant in `internal/beads/bdtest` are bumped together.

Integration tests run the real `bd`, never a script that imitates it. They are
skipped when `bd` is absent so an ordinary offline `go test` still passes, and
CI sets `SHIPMATES_TEST_BD_REQUIRED=1` so a broken install fails instead of
silently deleting the coverage. Point `SHIPMATES_TEST_BD` at an executable to
use a `bd` that is not on `PATH`.

`sail` validates the plan and installed crew, executes ready tasks with bounded
concurrency, persists state after every transition, and resumes unfinished work
on a later invocation. Press `Ctrl+C` once during execution to cancel active
crew; Sail acknowledges the interrupt immediately, terminates managed turns,
and preserves unfinished tasks as resumable voyage state.
The planning TUI returns to Captain-Skipper chat when Sail reports an incomplete
or blocked voyage. Successful voyages also return to the planning room, where
the Skipper summarizes persisted results and waits for the Captain's next
instruction. The planning command exits only when the Captain enters `/quit`,
cancels it, or encounters an unrecoverable application error.
The Captain can request bounded Architect advice with `/consult <question>`;
the response returns to the same conversation. The Skipper remains the sole
orchestrator and plan owner.

## Plan schema

```json
{
  "version": 1,
  "title": "Ship the account export",
  "objective": "Users can export their account data safely.",
  "scope": ["Authenticated export endpoint"],
  "non_goals": ["Changing retention policy"],
  "blast_area": ["Account API", "Export worker"],
  "risks": ["Cross-account data disclosure"],
  "acceptance_criteria": ["Export contains only the requesting account"],
  "open_decisions": [],
  "approved": true,
  "tasks": [
    {
      "id": "implement-export",
      "persona": "backend",
      "summary": "Implement account export",
      "prompt": "Implement the approved export flow. Run focused tests.",
      "depends_on": [],
      "models": ["gpt-5.6-luna", "gpt-5.6-terra", "gpt-5.6-sol"],
      "efforts": ["low", "high"],
      "retry_safe": true
    },
    {
      "id": "verify-export",
      "persona": "tester",
      "summary": "Verify the completed export",
      "prompt": "Test the export against the plan and report any failure.",
      "depends_on": ["implement-export"]
    }
  ]
}
```

Unknown fields, duplicate task identifiers, missing personas, cycles, empty
prompts, and unapproved plans are rejected before work starts. Task identifiers
use lowercase letters, digits, and hyphens.

The skipper must choose the smallest adequate model from `shipmates.yaml`'s
least-to-most-capable `modelLadder` and the lowest adequate effort first.
Optional `models` and `efforts` arrays define an effort-first escalation matrix
of at most eight tiers. Sail exhausts the effort ladder for the current model
before increasing model capability. For example, Luna/Terra/Sol with
medium/high becomes Luna-medium, Luna-high, Terra-medium, Terra-high,
Sol-medium, Sol-high. Sail advances one tier only after failure, records the
attempt, and starts a fresh Codex configuration. Multi-tier tasks must set
`retry_safe: true`; non-idempotent work cannot be automatically repeated.
Unknown models and descending model or effort ladders are rejected. Tasks with
no overrides start at the first configured model and `low` effort.

Runtime state is stored separately beneath `.shipmates/voyages/`; the approved
plan remains human-readable project content. Completed tasks are not rerun when
the same plan resumes. Changing an approved plan creates a new voyage identity.

## Approved amendments and predecessor lineage

An amended plan must be approved by the Captain before migration. Supply both
the immutable predecessor plan and its exact persisted state explicitly; Sail
never guesses a predecessor or searches for one:

```bash
shipmates sail \
  --plan .shipmates/amended-voyage.json \
  --predecessor-plan .shipmates/original-voyage.json \
  --predecessor-state .shipmates/voyages/<original-plan-hash>.json
```

The successor state is stored under its own plan hash. Migration validates both
approved plans, the predecessor hash/state bytes, bounded regular-file paths,
task sets, and completion evidence before publishing. It fingerprints the full
task execution contract and conservative global voyage contract. Only a
completed task with unchanged local and dependency-closure fingerprints is
marked `INHERITED`; a changed task and every dependent task remain pending.

Inherited status retains the original summary, timestamps, evidence references,
and opaque Bead ID in bounded provenance. The predecessor plan/state and Beads
are read-only. Sail does not recreate or relink inherited Beads; pending
successor tasks use the ordinary optional Beads adapter and may depend on an
inherited prerequisite. Repeating the same migration is idempotent; a
different existing successor state, malformed input, unapproved predecessor,
symlink, or ambiguous path fails closed. Use `--dry-run` with the same flags to
inspect inheritance without publishing or dispatching.

## Display

### Acceptance verdicts

Task completion is not voyage acceptance. A plan may designate one
`acceptance_gate_task`; only that task may emit the exact closed
`SHIPMATES_ACCEPTANCE_V1: {"verdict":"pass"|"no_go","evidence":[...]}`
marker. The verdict is persisted separately from task evidence and Beads.
Malformed, conflicting, unsupported, or unauthorized markers fail closed.

Voyages written before the verdict field existed remain readable. A completed
legacy voyage, or a verdict-aware voyage with `unset`, displays acceptance
unknown; it is never rendered as PASS. `no_go` displays completed work with
acceptance failed, while only an explicit valid `pass` verdict displays
acceptance criteria passed. The verdict timestamp and bounded evidence
references are append-only state provenance and do not alter task lineage.

Interactive terminals receive stable persona colors, task-state marks, and
bounded final summaries. Color is presentation only: logs and
redirected output remain plain and always include the persona name. Use
`shipmates sail --no-color` to force plain output.

## Safety

- The captain's explicit approval is required before dispatch.
- Invoking `shipmates sail` is the local captain's execution confirmation; the
  plan's approval field is project workflow evidence, not a cryptographic signature.
- Crew tasks cannot recursively delegate through Shipmates.
- Concurrency and per-task duration are bounded.
- Dependency failure blocks downstream tasks.
- Shipmates policy is enforced through managed Codex sessions for `ask`,
  fanout, drain, autonomous invocations, and sail. `allow` and `deny` resolve
  automatically; headless `ask` decisions fail closed and require an attached
  dashboard controller for human approval.
- Cancellation and failures persist; they are not rewritten as success.
- `--dry-run` validates and displays the execution order without dispatch.
- Sail does not merge, deploy, or approve policy requests on behalf of the
  captain unless those actions are explicitly represented in approved tasks and
  permitted by project policy.
