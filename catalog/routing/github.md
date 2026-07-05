## GitHub routing conventions

This project routes work through GitHub issues and PRs. Follow these conventions exactly — they are how the crew coordinates without colliding.
{{if .Labels}}
### Issues are the work queue

- Your inbox is `gh issue list --label <your-name> --state open`. The label matching your persona name means the work is yours.
- Unlabeled open issues are unclaimed. The first persona to claim one adds their label.

### Claiming binds ownership

- Claim an issue by commenting on it{{if .Bylines}}, leading with your byline: `<byline> picking this up.`{{else}} to state you're taking it{{end}}.
- The claim comment **is** the binding signal. Once an issue is claimed, do not touch it — even if you see a faster path, or it cross-cuts your lane.
{{end}}{{if .Bylines}}
### Byline every GitHub message

- Start every comment, issue body, and PR body you write with your byline. All crew commit as the same GitHub user, so the byline is the only way a human can tell which persona is speaking.
{{end}}{{if .Beads}}
### Beads ↔ GitHub: two layers, one link

GitHub issues are the **human-facing contract layer**; the beads graph (`bd`) is the **agent coordination layer**. They link through `external_ref`.

- When decomposing a gh issue into work beads, stamp every bead with the issue: `bd create "<title>" --external-ref gh-<n>`. One issue may fan out to many beads; each `gh-<n>` ref traces back to the contract.
- `--external-ref` also takes a full URL (e.g. a PR: `https://github.com/<owner>/<repo>/pull/<n>`). Use `bd update <id> --external-ref ...` to link after the fact.
- A bead id is a **context capsule**: when dispatching work to a subagent or crew member, hand them the bead id — they run `bd show <id>` to load the work's context instead of you re-explaining it.
- Close the loop in both layers: when the PR merges, `bd close <bead-id>` AND the issue closes via `Closes #<n>`. A closed issue with open beads means unfinished decomposed work — flag it.
{{end}}
### One worktree per issue

- Branch off `origin/main` (NOT local `main`, which may lag):

  ```
  git worktree add .claude/worktrees/<short-name> -b worktree-<short-name> origin/main
  ```

- The worktree dir mirrors the branch with the `worktree-` prefix stripped. Multiple in-flight issues → multiple worktrees, no collisions on `main`.

### Open the PR with `Closes #<n>`{{if .Labels}} and your label{{end}}

- `gh pr create{{if .Labels}} --label <your-name>{{end}} --body-file -` with a body containing `Closes #<n>`.{{if .Labels}} The PR label must match the issue label; a mismatch is a smell worth flagging.{{end}}
- ⚠️ **Close-keyword footgun:** any English use of "fix", "close", or "resolve" followed by ` #N` — in the PR body **or any commit message** — auto-closes issue #N on merge. This causes real bugs. Do not put those words next to a `#number` unless you intend to close it.

### The merge gate: anchored verdicts

- Every peer review ends with a verdict line on its own:
  - `Verdict: LGTM` — approve (nits are fine)
  - `Verdict: Needs changes — <what blocks>` — do not approve
- Merge **your own** PR only when ALL of these hold:
  - It's yours (never merge a peer's PR),
  - It's mergeable + CLEAN + all checks pass,
  - At least one peer's latest verdict is `Verdict: LGTM`,
  - No peer's latest verdict is `Verdict: Needs changes` (an unresolved objection blocks the merge regardless of other LGTMs).
- A self-LGTM does not count. Wait for a peer.

### Review ritual (every step, in order)

Before posting `Verdict: LGTM` on a peer PR — or before merging your own:

1. **Read the originating issue first** — `gh issue view <n>` for the `Closes #<n>` issue.
2. **Confirm the PR actually closes it** — walk every acceptance bullet against the diff. If there are 4 bullets and 2 are covered, it should not close the issue.
3. **Tests where tractable** — logic / parsing / state machines should have tests; for pure cosmetic UI, say so explicitly. Don't demand tests where there's no real surface.
4. **Cross-reference the project's gotchas doc** if one exists.
5. **Then** read the code for bugs, style, and regressions.

Skipping step 1 is code-skimming, not reviewing.

### Cleanup ceremony after every merge (run in order)

1. `gh pr merge <n> --merge --delete-branch`
2. `git worktree remove .claude/worktrees/<short-name>` — this MUST come before deleting the local branch (git refuses to delete a branch a worktree is checked out on).
3. `git branch -D worktree-<short-name>`
4. ⚠️ **`git pull origin main` on the root checkout — THE MOST IMPORTANT STEP.** Skip it and your next build/test runs stale code. This is the single biggest cause of misleading "I fixed it" reports.
5. Verify: `git worktree list`, `git branch -a`, and `git status` on root main.

### GitHub CLI gotchas

- Use `gh ... --body-file -` for heredoc input — NOT `--body @-`, which `gh` stores as the literal string `@-` (silently creates blank issues/PRs).
- `gh pr edit --add-label` needs `read:org` scope, which standard tokens lack. Fall back to the REST API: `gh api -X POST repos/<owner>/<repo>/issues/<n>/labels -f labels[]=<name>` (PRs are issues in the API).
- Use the project's shared `GITHUB_TOKEN`; respect its authorship discipline (no extra "Co-Authored-By" / "Generated with" footers unless the project wants them).
