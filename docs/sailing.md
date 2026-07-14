# Sailing projects

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

The plan sidebar joins the approved plan with its matching persisted voyage
state. Completed voyages mark the objective and acceptance criteria `PASS` and
show each job as completed. Beads-enabled voyages also show each task's opaque
Bead ID. Use Page Up/Page Down to move through long plans;
Home and End jump to the beginning and end of the sidebar.

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
prerequisites: Sail stops before dispatch if it cannot establish the graph.
Without Beads, no external graph is required. Later status synchronization is
best-effort and visibly warns without rewriting successful code work as failed.
Beads owns its Dolt database, schema, synchronization, and CLI behavior;
Shipmates does not vendor or reproduce them.

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

## Display

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
