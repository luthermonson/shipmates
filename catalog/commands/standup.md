---
description: Fleet standup — per-worker breakdown of open issues, open PRs, and what each is waiting on.
---

Run a quick fleet status by worker. Read-only — this command does NOT
claim, comment, merge, or modify state. Just looks at the queue and reports.

Requires GitHub routing (`routing: github` in shipmates.yaml). Without it the
verdict/merge-state lines below won't apply.

## 1. Fetch state

In parallel (`gh` calls are independent — fire them in one assistant turn):

    gh issue list --state open --limit 200 --json number,title,labels,assignees,author,createdAt,updatedAt,comments,body
    gh pr list   --state open --limit 100 --json number,title,labels,author,headRefName,isDraft,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,comments,createdAt,updatedAt

Do NOT fall back to scraping `--web` HTML — the JSON output is the source of truth.

## 2. Group by worker

The bucket labels are the **active crew** for this project: read the persona
names from `shipmates.yaml` (the `crew:` map, or the personas present in
`.codex/agents/`). Bucket every issue and PR by its matching fleet label.

An item with no fleet label goes in a final **Unowned** bucket so it's visible —
those are recruitment candidates or stale claims. If an item has multiple fleet
labels (rare — co-owned), put it in both buckets and mark `(co-owned with <other>)`.

## 3. Classify what each item is waiting on

For each **issue**, use the shortest fit:

- `unclaimed` — no claim comment from any persona yet
- `claimed by <persona>, in-flight` — claim comment exists, no PR yet
- `PR open: #<n>` — a PR with `Closes #<this>` is open
- `blocked: <one-line why>` — body or recent comment says it's blocked
- `stale (<N>d)` — no activity in 14+ days, no claim, no PR

For each **PR**:

- `draft` — `isDraft: true`
- `needs review` — open, not draft, no review comments yet
- `Verdict: Needs changes from <persona>` — the most recent verdict on the PR is
  a `Needs changes` (parse the `Verdict:` anchored lines per the GitHub routing
  conventions)
- `awaiting peer LGTM` — author has no peer `Verdict: LGTM` yet
- `ready to merge` — peer LGTM landed, checks green, mergeable, no open
  Needs-changes verdict — author just hasn't merged yet
- `conflicts` — `mergeStateStatus: DIRTY` or similar
- `checks failing` — `statusCheckRollup` has any FAILURE

## 4. Print the standup

Format (markdown, terse — one line per item):

    ## 🛸 Fleet standup — <today's date YYYY-MM-DD>

    ### <persona> (<one-line domain summary>)
    **Issues** (<count> open):
    - #135 — <title> [unclaimed]
    - #142 — <title> [claimed by <persona>, in-flight, PR #146]
    **PRs** (<count> open):
    - #146 — <title> [ready to merge — <peer> LGTM 2d ago]

    ### Unowned
    - #87 — <title> [no fleet label, 30d stale — assign or close?]

    ### Headline numbers
    - Total open issues: <N> (per-persona breakdown)
    - Total open PRs: <N> (ready-to-merge: <r>, needs-review: <nr>, needs-changes: <nc>, conflicts: <co>)
    - Oldest unclaimed in-domain issue per persona (flag staleness)

## 5. End with a one-line takeaway

A single sentence: the most useful thing to look at *right now*. E.g.
*"<persona> has 3 PRs ready to merge — biggest unblock today is clearing those."*
Keep it short and concrete — a pointer to **the next action**, not "we're doing great."

## Notes

- This command is read-only. If something looks wrong (a label is missing, a
  verdict line isn't parsing), **say so in the report** but don't fix it inline —
  file an issue.
- If `gh` errors, surface the error and stop; don't synthesize a fake standup
  from cached memory.
- Don't spend more than ~30s fetching. If you're wandering into per-comment
  fetches for every PR, you're overdoing it.
