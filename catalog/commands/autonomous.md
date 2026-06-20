---
description: Turn on (or refresh) this project's recurring autonomous lead schedule using Claude Code's cron. Run this in an INTERACTIVE session — it creates a durable scheduled job that fires the lead's read-then-dispatch cycle on a cadence. Durable crons only persist when created interactively, so this can't be a headless CLI flag.
---

Set up the recurring autonomous schedule for this project, idempotently.

1. Get the charter: run `shipmates autonomous --print-charter` (add
   `--persona <lead>`, `--cadence ...`, `--cap N` if the defaults don't fit) and
   capture its full output. That text is the scheduler prompt.

2. Find any existing schedule: use **CronList**. If a job named
   `shipmates-autonomous-<lead>` already exists, remove it with **CronDelete**
   (we are replacing it, not duplicating).

3. Create the schedule: use **CronCreate** to make a **recurring, durable** job
   named `shipmates-autonomous-<lead>` that fires every 5 minutes (or the
   operator's chosen interval), with its prompt set to EXACTLY the charter from
   step 1. Set `durable: true` so it survives session restarts.

4. Confirm back: report the created job id and the firing interval.

To turn autonomy OFF later: run this command's step 2 and stop after the
CronDelete (or just say "stop autonomy" and delete the `shipmates-autonomous-*`
job).

Notes:
- **This must run in an interactive Claude Code session.** A durable cron only
  persists when created interactively; a headless `claude -p` cron is session-only
  and evaporates — which is why `shipmates` can't wire this from a plain CLI flag.
- Once set, the charter self-reschedules each cycle (exponential backoff), reusing
  the same `shipmates-autonomous-<lead>` job name so it stays in lockstep with
  this command.
