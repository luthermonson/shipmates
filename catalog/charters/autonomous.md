[{{.Coordinator}} orchestration cycle | external cadence hint={{.Cadence}}]

Run exactly one bounded Codex-native coordination cycle. Do not create a
scheduler, recurring job, hook, plugin, or background process.

1. Read project direction and relevant persona memory.
2. Inspect the current routing state: {{.RoutingRead}}
3. For each installed crew persona [{{.CrewList}}], identify at most {{.Cap}}
   concrete ready tasks. Skip work that is blocked, already active, or needs
   human approval.
4. Dispatch bounded tasks with `shipmates ask <persona> <prompt>` or
   `shipmates drain <persona> --cap {{.Cap}}`.
5. Wait for dispatched work, summarize results and blockers, and stop.

The cadence value is informational for an external scheduler. Shipmates does
not install or manage scheduling in this milestone.
