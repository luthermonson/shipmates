# Persona berths — per-persona working-tree isolation

> Working doc / proposal. Companion to `architecture.md` (persona/session fundamentals) and
> `memory-on-session-start.md` (which evaluated berths as a *memory-load* mechanism and rejected them for
> that job). This doc treats berths as what they actually are: a **filesystem-isolation** feature,
> orthogonal to memory. It stands alone; read the memory doc only for the orthogonality proof (§4).

## What a berth is

A **berth** is a per-persona git worktree — `.shipmates/berths/<persona>` — that a persona's Claude Code
session runs *inside* (`cmd.Dir = berthPath`), instead of sharing the repo root with every other session.

**Today, berths are a manual captain convention, not a feature.** Only the lead's berth exists, and it is
set up by hand. **shipmates ships zero berth code** — a grep for `berth`/`worktree` across
`internal/**/*.go` turns up only prose (a routing-flow description at `charters.go:51`, a config comment at
`install.go:47`) and unrelated git-pointer parsing for repo-URL detection (`repourl.go:22,39`). No spawn
site sets `cmd.Dir` to a persona directory; no code creates, reuses, or prunes a worktree. Building berths
would make shipmates own its **first git-worktree lifecycle**.

## The honest value proposition

Two claims are made for berths. Weigh them separately, and skeptically.

### Claim 1: filesystem isolation for parallel crew

**True that crew share one tree today.** `drain-many` runs N personas concurrently
(`charters.go:158-181`), each via `dispatchTo`, which spawns `claude` with **no `cmd.Dir`**
(`run.go:48-65`, exec at `:57`). The children inherit the shipmates process's cwd — the repo root. Same for
`ask`→`dispatch` (`run.go:33,42`), `fanout`→`oneShotDelegate` (`fanout.go:89-111`, exec at `:100`), and
interactive `open` (`open.go:50`). So yes: without berths, parallel personas operate in the same working
tree.

**But most of the isolation is already paid — conditionally.** How much depends entirely on the routing
layer:

- **With `routing: github`** (opt-in — default is empty/agnostic, `install.go:46-51`,
  `project.Config.Routing`), every persona is instructed to **claim → branch its own per-issue worktree off
  `origin/main` → PR → cleanup** (`catalog/routing/github.md:33`:
  `git worktree add .claude/worktrees/<short-name> -b worktree-<short-name> origin/main`;
  `routingFlow` at `charters.go:48-54`). The **mutation-heavy** work — edits, builds, test artifacts — all
  happens inside that per-issue worktree, one per issue, already collision-free. Here a persistent berth
  isolates only the **staging window**: the repo-root moments *before* a worktree exists (reading, planning,
  grepping, a warm-up `go build`) and any work that never claims an issue (a review-only pass, a question).
  That window is overwhelmingly **read-heavy and low-collision**. The marginal benefit is a belt beside the
  routing layer's suspenders — real, but thin.

- **Without routing worktrees** (the default routing-agnostic fleet — `routingFlow` returns a generic
  "claim → implement → review → merge" with *no* worktree step, `charters.go:52-53`), parallel `drain-many`
  crew genuinely edit the **same** working tree at once and *can* stomp each other. Here berths would be the
  **primary** isolation, not a marginal one.

**So the value of berths is inversely proportional to how much the routing layer already isolates.** Sell
them to routing-agnostic fleets; do not oversell them to a `github`-routed fleet, where per-issue worktrees
have already done the heavy lifting.

### Claim 2: formalizing the manual lead setup

Real but small. The lead berth is a single, long-lived, low-churn worktree a human maintains by hand.
Turning that one convention into a command (`shipmates berth <persona>` / automatic on spawn) is a genuine
convenience, but it does not by itself justify a general per-persona lifecycle.

## Design surface

### `cmd.Dir` at spawn

Berths are, mechanically, one line at each spawn site: set `cmd.Dir = berthPath(persona)`. The sites that
have **no `cmd.Dir` today** and would need it:

| Site | Surface |
|---|---|
| `run.go:57` (`dispatchTo`) | `ask`, `drain`, `drain-many` |
| `fanout.go:100` (`oneShotDelegate`) | `fanout` |
| `open.go:50` | interactive `open` |
| `server.go:605` (`ensureLive`) | live stream-json mate |
| `ptyproc.go:132` / `:155` (`ensurePTY`) | PTY mate |

(The only existing `cmd.Dir` in the tree is the ship supervisor spawning `server serve` per project —
`ship.go:163` — unrelated.) Note also that the shipmates *process itself* keeps running at the repo root;
only the child `claude` moves into the berth. That matters for §4.

### Lifecycle (the actually-hard part)

None of this exists yet. A berth feature owns all of it:

- **Create-on-first-run:** `git worktree add .shipmates/berths/<persona>` pinned to `origin/main` (see
  branch strategy). Must handle "already exists," "dir present but not a worktree," and a non-git project
  (degrade to no berth — spawn at root as today).
- **Reuse vs recreate:** reuse a clean existing berth; decide policy for a *dirty* berth (uncommitted junk
  from a prior run). Reset? Refuse? Leave and warn? Each has failure modes.
- **Prune:** stale berths after a repo move, after a persona is renamed, or on demand.
- **Cleanup on persona removal:** `shipmates remove` today deletes the agent file and (with `--purge`) the
  memory dir (`remove.go:41-61`). A berth adds a `git worktree remove` step here — and must refuse or warn
  if the berth is dirty or holds a nested per-issue worktree mid-flight.

### Branch strategy: pin to origin/main, never commit to a berth

A berth is a **launch cwd, not a development branch.** Pin it to `origin/main` (detached, or a throwaway
`berth/<persona>` ref that is never merged) and never commit into it. If berths accumulate commits, their
tracked `.shipmates/` tree diverges per persona and `update`/manifest semantics fracture. One worktree per
branch is a git rule, so each berth needs its *own* ref regardless — keep those refs disposable.

### Composition with per-issue routing worktrees (nesting)

Under `github` routing the persona, now launched with `cwd = berth`, runs
`git worktree add .claude/worktrees/<short-name> …` **relative to the berth**, creating
`.shipmates/berths/<persona>/.claude/worktrees/<short-name>/` — a **worktree nested inside a worktree**. Git
tolerates this (all worktrees share one common `.git`), but it deepens paths and means the cleanup ceremony
(`catalog/routing/github.md:70-73`) now runs one level down. Berth prune must not run while a nested
per-issue worktree is live. This is added interaction surface for isolation that, under `github` routing, is
mostly already there (Claim 1).

## The load-bearing interaction with memory — why berths are orthogonal

This is the crux, and it is why berths must **not** be entangled with the memory-load design settled in
`memory-on-session-start.md`.

**Memory is gitignored by default** (`shipmates.yaml: sharedMemory: false` — `install.go:60-62`;
`architecture.md:125`). **A fresh git worktree carries only *tracked* files.** Therefore a berth's
`.shipmates/memory/<persona>/` is **empty** — the persona's accumulated memory lives as untracked files in
the *root* checkout and does not exist in the worktree.

The saving grace is where shipmates reads memory from. `project.MemoryDir(persona)` returns the **relative**
path `.shipmates/memory/<persona>` (`project.go:33-34`, `Dir = ".shipmates"`), resolved against the
**shipmates process's** cwd — the repo root — *not* the child `claude`'s `cmd.Dir`. So any memory shipmates
reads server-/CLI-side (to build a `--append-system-prompt` payload, or seed on `add`) comes from the
**canonical root copy**, regardless of which berth the child runs in. Berths change the child's cwd; they do
not change where shipmates itself resolves memory.

That yields a clean split:

- **`--append-system-prompt` memory-load (the recommendation in `memory-on-session-start.md`) is
  berth-independent.** shipmates reads canonical root memory and injects it as a flag at spawn; the child's
  `cmd.Dir` is irrelevant. Berths and this mechanism compose with zero interaction.
- **A cwd/basename memory hook is exactly the WRONG design under berths.** Such a hook runs *inside* the
  child (cwd = berth) and would read the berth's `.shipmates/memory/<persona>/` — **empty**. Berths would
  silently break a cwd-based memory hook. This is the concrete reason the memory doc rejected "identity
  falls out of `basename`, memory works naturally": under the default gitignored posture, memory does **not**
  live in the berth.

**Orthogonality proof, stated plainly:** the memory-load mechanism must stay worktree-independent (read
canonical root memory), and the recommended mechanism already is. Given that, berths neither help nor hurt
memory load — they are a separate concern about *where file operations happen*, not *whether memory is
present*. Build (or skip) berths on their isolation merits alone; the memory question is already closed and
unaffected. (If a team sets `sharedMemory: true` and commits memory, it *would* appear in the berth — but
then there are two tracked copies that can diverge, which is worse, not better. Canonical-root reads remain
the right design either way.)

## Resume / session semantics

shipmates keeps **one long-term session per persona**, resumed by UUID (`sessionlaunch.go:20-38`;
`architecture.md:548`: `--session-id` is create-only, `--resume` continues by UUID or `--name`). Session
*naming* is computed by the shipmates process at the repo root (`RepoName`/`SessionName`,
`project.go:102-108`, `:252-257`) and is **not** affected by the child's `cmd.Dir` — so berths do not change
resume handles.

**The open risk:** Claude Code may scope a resumable session to the directory it was *created* in. A session
created today at repo-root, later resumed with `cmd.Dir = berth`, could be treated as a different project and
silently fork history — breaking the one-session-per-mate invariant the whole design rests on. This is a
Claude Code behavior question, **not answerable from shipmates source** — flag as **needs-CC-verification**
before any berth work touches an existing session. (Migration concern: introducing berths would move every
persona's cwd from root to berth on the *next* spawn, i.e. mid-session for every existing mate.)

## Costs

- **First worktree-lifecycle code in shipmates** — create/reuse/prune/cleanup, dirty-tree and
  stale-worktree handling, non-git-project degradation, cross-platform. This is the real cost; the
  `cmd.Dir` change is trivial by comparison.
- **Disk** — one working-tree copy per persona (× checkouts).
- **New interaction rules** — nested per-issue worktrees, prune-safety while nested trees are live, the
  session-resume migration above, and a cleanup step wired into `remove` (`remove.go`).

## Recommendation (architect)

**Berths are not worth their own lifecycle code right now.** The verdict turns on the marginal-benefit
accounting: for a `github`-routed fleet — the shape the crew conventions target — per-issue worktrees
(`catalog/routing/github.md:33`) already isolate the mutation-heavy work, leaving berths to cover only a
read-heavy staging window. Paying shipmates' first worktree lifecycle (with its dirty-tree, prune, nesting,
and session-resume-migration hazards) to isolate that window is a poor trade. And berths cannot be
justified by identity or memory: identity is solvable far more cheaply (env / `--append-system-prompt`
already carries the persona), and berths actively *break* a cwd-based memory hook under the default posture
(§4).

**Where they would earn their keep:** a **routing-agnostic** fleet that runs `drain-many` with no per-issue
worktrees, where crew genuinely share one tree and can stomp each other (`charters.go:52-53`). That is the
population to build for, if any.

**Phasing, if pursued:**

1. **Confirm demand.** Only build if routing-agnostic parallel `drain-many` is a real usage pattern that is
   hitting real collisions. If every serious fleet runs `github` routing, berths are largely redundant —
   don't build speculatively.
2. **Resolve the session-resume cwd-scoping question** (needs-CC-verification) *first* — it gates whether
   berths can be introduced without forking every existing mate's history.
3. **Start with the lead berth only** — it is already a manual, single, low-churn convention. Formalize
   that one (opt-in `shipmates berth lead`, pinned to `origin/main`), learn the lifecycle edge cases on one
   worktree, then generalize to per-persona berths *behind a config flag*, off by default.
4. **Keep memory-load on `--append-system-prompt`** regardless — it is worktree-independent and must stay
   that way, so berths remain a pure isolation feature that composes cleanly.

Net: berths are a legitimate Phase-2+ isolation nicety for a specific (routing-agnostic) audience, gated on
a Claude Code verification and on demonstrated need. They are not a near-term priority, and they must never
be coupled to the memory-load path.

## Open questions

1. **Session-resume cwd-scoping (needs-CC-verification).** Does Claude Code bind a resumable session to the
   directory it was created in? If yes, moving a persona's cwd into a berth forks its history. Blocks any
   berth work on existing sessions. *Not answerable from shipmates source — test against Claude Code.*
2. **Dirty-berth policy.** Reset, refuse, or warn-and-reuse when a berth has uncommitted changes from a
   prior run? Resolvable in code once the feature is scoped.
3. **Is routing-agnostic parallel `drain-many` a real, collision-hitting pattern?** The demand signal that
   decides whether berths are worth building at all. Needs field evidence, not code.
