[{{.Coordinator}} orchestration cycle | external cadence hint={{.Cadence}}]

Run exactly one bounded coordination cycle. Do not create a
scheduler, recurring job, hook, plugin, or background process.

1. Read project direction and relevant persona memory.
2. Inspect the current routing state: {{.RoutingRead}}
3. For each installed crew persona [{{.CrewList}}], identify at most {{.Cap}}
   concrete ready tasks. Skip work that is blocked, already active, or needs
   human approval.
4. Prepare or refine `.shipmates/voyage.json` with `approved` set to `false`.
   Do not dispatch crew, invoke `ask` or `drain`, approve the plan, or invoke
   `sail`; only the human captain can approve and start execution.
5. Summarize the proposed voyage and blockers, then stop.

The cadence value is informational for an external scheduler. Shipmates does
not install or manage scheduling in this milestone.
