# Sailing projects

A **voyage** is a structured plan that Shipmates executes task by task until everything is done or explicitly blocked. Three roles, held apart on purpose:

- The **first mate** (persona: `shipmates add first-mate`) is the voyage execution lead: it plans the voyage, writes the draft, presents it, and reports status. It never authorizes execution.
- The **admiral** — the human operator — **commissions** the voyage. Commissioning is the execution authorization, and it is structurally reserved to the human: `shipmates commission` refuses to run inside any agent turn.
- The **captain** persona remains the ship's standing coordinator and is unchanged by voyages; it is neither the voyage lead nor the approver.

`shipmates plan` validates and inspects the draft; `shipmates commission` records the admiral's authorization; `shipmates sail` executes the commissioned plan with bounded concurrency, persisting state after every transition so a canceled or crashed run resumes exactly where it stopped.

Sailing needs **no external dependencies**: task tracking uses plain markdown files in your repository by default. Installing the external `bd` CLI ([Beads](https://github.com/gastownhall/beads)) is an optional upgrade, not a requirement.

**Runtime scope.** Sail launches crew turns through Claude Code's CLI and session protocol — the same launch path as `ask`. When a non-Claude runtime is selected (`--runtime codex`, project or user config), sail refuses with an error naming the runtime and what is missing, exactly like `ask` does; see [runtime-interface.md](runtime-interface.md).

## Workflow

1. Install the first mate and the crew personas the plan will name:

   ```bash
   shipmates add first-mate
   shipmates add backend
   shipmates add tester
   ```

2. Plan the voyage with the first mate (`shipmates open first-mate`, or `shipmates ask first-mate ...`). The first mate writes the structured draft to `.shipmates/voyage.json` with `"commissioned": false` — always.
3. Inspect and validate the draft:

   ```bash
   shipmates plan
   ```

   `shipmates plan` prints the plan joined with any persisted voyage state — status, objective, per-task state, acceptance verdict, structured-recovery ledgers — and reports the exact validation defect when the draft is invalid. `shipmates plan --fresh` clears the draft (persisted voyage state and reports are never touched).
4. Commission it. This is the admiral's act, at the admiral's own terminal:

   ```bash
   shipmates commission
   ```

   The command validates the draft, flips `commissioned` to true atomically, and prints the final summary. It refuses to run inside a sail-managed crew session or any Claude Code tool subprocess, so no persona turn can commission a voyage through Shipmates. (Editing the JSON by hand is the documented manual alternative — and equally an admiral act.) An uncommissioned voyage always refuses to sail.
5. Execute:

   ```bash
   shipmates sail
   ```

   `sail` validates the plan and installed crew, executes ready tasks with bounded concurrency (at most one in-flight turn per persona), persists state after every transition, and resumes unfinished work on a later invocation. Press `Ctrl+C` once to cancel: active crew turns stop and their tasks return to pending, resumable state.

   If a previous run left failed or blocked tasks, review them (`shipmates plan`) and resume with `shipmates sail --retry-failed`. Use `--verbose` to see each task's full prompt and final report, `--dry-run` to validate and display the execution order without dispatching, and `--no-color` for plain output.

The reference design's interactive planning room (a TUI with `/consult` and `/sail`) is not part of this port: main has no in-process interactive planning attach. `plan` is deliberately the honest subset — validation and truthful status — and planning conversations happen in ordinary first-mate sessions.

## Task tracking: markdown by default, Beads as an upgrade

Sail mirrors every voyage task into a task tracker so humans and crew can see and annotate the work. The tracker is a mirror for context — the authoritative execution state is always `.shipmates/voyages/<plan-hash>.json`.

**Markdown backend (default).** With no Beads workspace, tasks are tracked as plain files under `.shipmates/voyage/`, one per task (`<plan-hash-prefix>-<task-id>.md`): a small YAML frontmatter block (id, status, assignee, dependencies) over a human-readable description and an append-only `## Log` section. The files are git-diffable on purpose — a PR diff shows the voyage's task state. Writes are atomic (temp file + durable rename), so a mid-run crash leaves either the previous or the new record, never a torn file. Hand edits are welcome: fix a typo, add a note, even flip a status — the tracker parses and keeps going, and only actual garbage (a broken frontmatter block, an invented status) gets an error naming the file. Crew turns are told where their task file is and asked to append durable findings under its Log heading.

**Beads backend (optional).** If the external `bd` CLI is installed and the project has an initialized workspace (`shipmates beads init`), sail automatically uses Beads instead: one Bead per task, real dependency edges, `bd prime` guidance and the live Bead record injected into crew prompts, and lifecycle status synchronized (`in_progress`, `closed` with the crew report as a comment, `blocked` with the reason).

Selection is automatic and announced with one line at the start of a run. Pin it explicitly in `shipmates.yaml`:

```yaml
voyage:
  tracker: markdown   # or beads
```

Explicitly configuring `beads` when bd is missing or the workspace is not initialized is an error naming what to install — never a silent fallback.

Both backends satisfy the same contract and the same tests. Task creation and dependency linking are durable prerequisites: sail stops before dispatch if it cannot establish the graph. The one exception is a dependency edge onto an **inherited** prerequisite (see amendments below): that record belongs to the predecessor voyage, is read-only, and may not be resolvable in this workspace at all, so the edge is recorded when present and skipped when not — lost provenance never refuses a successor voyage.

### The bd CLI, when you opt in

`bd` is resolved from `PATH` only; a file named `bd` next to the shipmates executable is deliberately not used. bd runs with a bounded, allowlisted environment — PATH, HOME/USERPROFILE, TMP/TEMP, SystemRoot/windir — so credentials held by the parent process cannot leak into an external binary (APPDATA/LOCALAPPDATA are withheld on purpose; that is where Windows keeps credential stores). `shipmates beads <args...>` passes through to bd inside the project, adding `--non-interactive --skip-agents --skip-hooks` to `init`.

The integration targets **bd 1.1.2**. Its verified output contract (object from `bd create --json`, array from `bd show <id> --json`, `--` end-of-options honored, close refused while blockers are open) is pinned by opt-in contract tests: set `SHIPMATES_TEST_BD=<path>` or `SHIPMATES_TEST_BD_REQUIRED=1` (CI) to run them against the real binary. They never run merely because bd is on PATH — every bd invocation boots an embedded Dolt engine.

Note the coordination server also integrates with Beads for live sessions (`internal/server`); sail's tracker and the server's handlers are independent consumers of the same workspace.

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
  "commissioned": false,
  "tasks": [
    {
      "id": "implement-export",
      "persona": "backend",
      "summary": "Implement account export",
      "prompt": "Implement the approved export flow. Run focused tests.",
      "depends_on": [],
      "models": ["claude-sonnet-4-6", "claude-opus-4-7"],
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

`commissioned` is the admiral's execution authorization: the first mate always writes `false`, and only `shipmates commission` (or the admiral's own manual edit) sets it to `true`. Unknown fields, duplicate task identifiers, missing personas, cycles, empty prompts, and uncommissioned plans are rejected before work starts. Task identifiers use lowercase letters, digits, and hyphens.

**Models and escalation.** A task with no `models`/`efforts` runs with its persona's own configured model and effort — no extra configuration needed. Optional `models` and `efforts` arrays define an effort-first escalation matrix of at most eight tiers: sail exhausts the effort ladder for the current model before increasing model capability, advances one tier only after failure, and starts each attempt as a fresh session (model and effort are baked into a Claude session at creation, so a tier change cannot silently resume a session created under different settings). Multi-tier tasks must set `retry_safe: true`. Any plan that escalates models requires a `modelLadder` in `shipmates.yaml`, ordered least to most capable; task models outside the ladder, or descending ladders, are rejected:

```yaml
modelLadder: [claude-sonnet-4-6, claude-opus-4-7]
```

Runtime state is stored separately beneath `.shipmates/voyages/`; the commissioned plan remains human-readable project content. Completed tasks are not rerun when the same plan resumes. Changing a commissioned plan creates a new voyage identity.

A crew turn that genuinely needs an admiral decision returns `SHIPMATES_NEEDS_INPUT:` followed by one bounded question; sail records the task as `needs_input` and moves on. A launch failure (the claude CLI itself could not start) is infrastructure, not a task result: it does not consume the task's escalation ladder, and `--retry-failed` resets such tasks to their first tier.

## Amendments and predecessor lineage

An amended plan must be commissioned by the admiral before migration. Supply both the immutable predecessor plan and its exact persisted state explicitly; sail never guesses a predecessor:

```bash
shipmates sail \
  --plan .shipmates/amended-voyage.json \
  --predecessor-plan .shipmates/original-voyage.json \
  --predecessor-state .shipmates/voyages/<original-plan-hash>.json
```

The successor state is stored under its own plan hash. Migration validates both commissioned plans, the predecessor hash and exact state bytes, bounded regular-file paths, task sets, and completion evidence before publishing. It fingerprints the full task execution contract and the conservative global voyage contract. Only a completed task with unchanged local and dependency-closure fingerprints is marked `INHERITED`; a changed task and every transitive dependent remain pending. Inherited status retains the original summary, timestamps, evidence references, and opaque tracker id in bounded provenance; the predecessor plan, state, and tracker records are read-only and never recreated. Repeating the same migration is idempotent; a different existing successor state, malformed input, uncommissioned predecessor, symlink, or ambiguous path fails closed. Use `--dry-run` with the same flags to inspect inheritance without publishing or dispatching.

## Acceptance verdicts

Task completion is not voyage acceptance. A plan may designate one `acceptance_gate_task`; only that task may emit the exact closed `SHIPMATES_ACCEPTANCE_V1: {"verdict":"pass"|"no_go","evidence":[...]}` marker. The verdict is persisted separately from task evidence, bound to the plan hash and global fingerprint. Malformed, conflicting, unsupported, or unauthorized markers fail closed and invalidate the emitting task's result.

`shipmates plan` renders acceptance truthfully: a completed voyage without an explicit verdict — including any legacy or hand-edited state — displays acceptance **unknown**, never PASS. `no_go` displays completed work with acceptance failed; only an explicit valid `pass` verdict displays acceptance criteria passed.

## Structured recovery (crew.result.v1)

Tasks may opt into structured recovery with an explicit `recovery` contract:

```json
{
  "id": "implement-export",
  "persona": "backend",
  "summary": "Implement account export",
  "prompt": "...",
  "retry_safe": true,
  "recovery": {
    "enabled": true,
    "max_attempts": 4,
    "max_infrastructure_retries": 2,
    "max_tokens": 200000,
    "models": ["claude-sonnet-4-6", "claude-opus-4-7"],
    "efforts": ["medium", "high"],
    "approved_criterion_ids": ["export-scoped"],
    "corrective_templates": [
      {
        "id": "fix-scope",
        "summary": "Correct the export scoping",
        "prompt": "...",
        "verification_summary": "Re-verify export scoping",
        "verification_prompt": "...",
        "criterion_ids": ["export-scoped"],
        "retry_safe": true
      }
    ]
  }
}
```

A structured task's crew turn must return exactly one JSON object matching the closed `crew.result.v1` contract (outcome, reason, bounded summary, token accounting, verifier result, redacted evidence digests), bound to the plan hash, global fingerprint, task fingerprint, and attempt id. Every attempt is recorded in a **hash-chained append-only ledger** (`.shipmates/voyages/<plan-hash>-<task>.attempts.jsonl`): reservation, dispatch, result, validation, transition, action. Each record carries the previous record's hash; tampering, truncation, or reordering fails closed. An attempt that was dispatched but has no terminal record is indeterminate and is refused rather than silently replayed.

Outcomes drive a deterministic state machine: `completed` closes the task; `retryable_failure` advances one effort-first tier within the closed budget; `infrastructure_retry` retries the same tier within `max_infrastructure_retries`; `authority_blocked`/`input_required` pause for the admiral; `no_go` from the designated verifier is authoritative and — when the verifier cites approved criteria matched by a corrective template — records a frozen corrective successor task plus its verification task in the ledger as reviewable lineage. `shipmates plan` shows a bounded projection of the ledger (tiers, budgets, outcomes, transitions, verifier status); it never renders prompts, paths, or raw records.

## Deferred: the reference branch's advisory stage

The reference implementation carried a Codex-specific advisory stage ("auto-captain"/Sol): a tool-less advisory turn that classified blockers against an append-only journal and could propose machine-attested derivative plans inside an approved change envelope. That machinery is **deferred, not ported**: it was an advisory frill on an obsolete transport, and main's launch path cannot provide the isolated turn it assumed. Its transport-agnostic contracts (the recovery journal, blocker fingerprints, and the derivative/change-envelope policy engine) live fully tested in `internal/recovery` for whenever a future runtime can honor the isolation contract; no command consumes them today.

## Safety

- The admiral's commission is required before dispatch, and nothing a persona can do through Shipmates can grant it.
- Crew tasks cannot recursively invoke sail: sail refuses to run inside a managed crew session.
- One sail per voyage: a lock file under `.shipmates/voyages/` refuses a concurrent second process (a crashed run's stale lock is named in the error and safe to delete once no sail process is running).
- Concurrency and per-task duration are bounded; dependency failure blocks downstream tasks.
- Cancellation and failures persist; they are never rewritten as success.
- Plan and state paths must be regular files inside the project; symlinks and escapes fail closed.
- Do not put credentials or raw external evidence in a plan, prompt, task record, or command argument.
