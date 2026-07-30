# Persona berths — per-persona working directories

A **berth** is a persona's persistent home: a git worktree at
`.shipmates/berths/<persona>` on branch `berth/<persona>`, which that persona's
runtime session runs *inside* instead of sharing the repo root with every other
session.

Berths are **opt-in and off by default.** A project that configures none behaves
exactly as it did before this document existed, down to identical session
fingerprints.

> **Provenance note.** Berths shipped in v0.4.0 (PR #13) and were then lost —
> not superseded. The merge that brought `main` into the Codex-native branch
> (`2610f92`) produced a tree byte-identical to its first parent, discarding all
> 65 files that arrived from `main`'s side, `internal/berth/` among them. This
> document and the package were restored from `main` and reconciled against the
> rewritten code. Three things changed in the process, each explained below:
> configuration moved out of persona frontmatter (§"Why configuration and not
> persona frontmatter"), the create-time-only rule is now enforced by session
> fingerprint instead of a persisted cwd (§Guardrails), and the spawn seams moved
> (§"How it is wired").

## Why berths exist

Two claims are usually made for per-persona worktrees. Only one of them holds.

**Filesystem isolation: mostly already paid.** Under `routing: github` each
persona is instructed to claim an issue and branch its own per-issue worktree
(`catalog/routing/github.md`), so the mutation-heavy work is already
collision-free. A persistent berth isolates only the staging window — reading,
planning, a warm-up build, work that never claims an issue. That window is
read-heavy and low-collision. For a routing-agnostic fleet, where parallel crew
genuinely edit one tree, berths *are* the primary isolation; for a
`github`-routed fleet they are a belt beside existing suspenders. Do not oversell
them.

**Launch ergonomics: the real reason.** A mate should be launchable from
anywhere and land in its own working tree, without a human `cd`-ing first, and
an operator should be able to provision, inspect and tear that tree down with a
command instead of a remembered `git worktree` incantation. That is what berths
buy, and it is why they are framed as "persistent home + launch ergonomics"
rather than parallel isolation.

The second historical justification — "formalize the manual captain berth" —
**no longer applies.** The captain persona was deliberately removed from the
catalog (`f7c000d`), and `captain` is now reserved for the human operator;
dispatch refuses it by name. Nothing in the catalog ships with a berth policy
today, which is why the shipped default for every persona is `off`.

## Configuration

Berths are configured in `shipmates.yaml`, per persona, under `crew`:

```yaml
crew:
  skipper:
    berth: auto              # create .shipmates/berths/skipper if missing, use it
  quartermaster:
    berth: require           # fail rather than silently run at the root
  backend:
    berth: off               # run at the repo root (the default; may be omitted)
  tester:
    cwd: sandbox/tester      # explicit directory; wins over any berth policy
```

| Value | Meaning |
|---|---|
| `off` / absent | Run at the project root. Today's behavior, and the fleet default. |
| `auto` | Create the worktree from `origin/main` (falling back to `origin/master`, then `HEAD`) if missing; reuse it if present. |
| `require` | Same as `auto`, but a project that is not a git repository is an error instead of a silent degrade. |
| `cwd: <path>` | An explicit working directory that wins outright over `berth`. Relative paths resolve against the project root. Nothing is provisioned. |

Unknown values resolve to `off` — the safe default. `LoadConfig` runs yaml with
`KnownFields(true)`, so a misspelled key (`berthh:`) is a hard parse error rather
than a silently ignored setting.

### Why configuration and not persona frontmatter

v0.4.0 read `berth:` from persona-artifact frontmatter *and* from crew
overrides. That left two spellings of one setting, and after the Codex-native
rewrite only the frontmatter half survived — as an inert field parsed into
`persona.Canonical`, carried through `runtime.PersonaSpec`, and elided by every
runtime installer. Nothing resolved it.

Berths now live in `shipmates.yaml` only, for three reasons:

1. **A berth is a property of the checkout, not of the role.** Persona files are
   shared, versioned definitions of what a mate *is*; which directory it works
   in is local layout. Two clones of one repo can reasonably differ.
2. **Persona artifacts are runtime instructions, not a configuration source.**
   That is an explicit stance of the current code — see
   `project.ResolvePersonaConfigAt`. Reading berth from frontmatter would
   reintroduce the second configuration source the rewrite removed.
3. **No runtime can express a berth.** Carrying it in `runtime.PersonaSpec` —
   the type whose whole job is "what a runtime understands" — was dead weight,
   so `PersonaSpec.Berth` is gone. A catalog file that still carries `berth:` in
   its frontmatter parses into `Canonical.Extra` and round-trips unchanged; it is
   simply not read.

## Command surface

```
shipmates berth ensure <persona> [--policy off|auto|require]   # provision or reuse, print the path
shipmates berth path <persona>                                 # where will this mate work? (no side effects)
shipmates berth list                                           # personas with a registered berth, clean/dirty
shipmates berth remove <persona> [--force]                     # tear one down, keeping the persona
shipmates remove <persona> [--purge] [--force]                 # remove the persona and its berth
```

`berth ensure` prints only the resolved path on stdout (the explanation goes to
stderr), so it composes: `cd "$(shipmates berth ensure skipper)"`. A persona
whose policy is `off` prints nothing, which is a shell-testable answer.

`berth list` reads `git worktree list`, not the filesystem, so a directory a
human parked under `.shipmates/berths/` is never reported as a berth.

## How it is wired

`berth.ResolveSpawnCWD(persona, cfg)` is the single decision point. It returns
an absolute directory, or `""` meaning "no override — run at the project root".
Callers hand the result to `runtime.SessionSpec.WorkingDir` (which documents
itself as defaulting to `ProjectDir` when empty) or to an `exec.Cmd.Dir`.

Wired today:

| Seam | Surface |
|---|---|
| `commands/run.go` `dispatchRuntimeTurn` → `SessionSpec.WorkingDir` | `shipmates ask` on the Claude runtime |
| `commands/codex.go` `dispatchCodexExecInstalledImages` → `cmd.Dir` | the low-level `codex exec` dispatcher (retained for the JSONL parser tests; production codex dispatch goes through the live-session path below) |
| `commands/install.go` (`init`, `addPersona`), `commands/update.go` (`runUpdate`), `commands/remove.go` (`runRemove`) | guardrail R1a refusals |
| `commands/remove.go` | berth teardown on persona removal |

**Not wired yet — the live-session path.** `open`, `live`, `sail`, `drain`,
`fanout` and `ask` on the Codex runtime all funnel through
`livesession.Manager.StartIdle`, which hardcodes `cwd := root` and has no
working-directory field on `StartIdleOptions`. Berthing them is a two-line
change in `internal/livesession/manager.go` (a `WorkingDir` option, and
`cwd = opts.WorkingDir` when set) plus resolution at the two callers. Until that
lands, only the seams in the table above honor a berth; everything else runs at
the project root regardless of policy.

## Guardrails

- **R1a — manifest-mutating commands run in the canonical tree.** `init`, `add`,
  `update` and `remove` refuse when the process cwd is inside a berth.
  `update` in divergent berths is the one action that genuinely fractures the
  tracked `.shipmates/manifest.json`. Detection is a case-folded path-segment
  test (Windows drive-letter and case variance) confirmed by `git rev-parse`,
  and it fires from berth subdirectories too. The refusal is applied inside
  `addPersona`/`runUpdate`/`runRemove` rather than in the CLI action, so it
  covers every caller and reports before the policy write lock — otherwise the
  operator sees a lock error caused by the berth's missing `.shipmates/`.
- **R1b — berth branches stay short-divergence.** Created from `origin/main`,
  committed to, fast-forwarded back. Not long-lived forks.
- **Berth and cwd never enter `PersonaConfig.Fingerprint()`.** If they did,
  giving a persona a berth would auto-`--fresh` the very session the berth is
  meant to keep.
- **A session is never resumed into a directory it was not created in.** The
  resolved working directory is folded into the *session* fingerprint at the
  spawn site by `berth.SessionFingerprint`, so a persona that gains or changes a
  berth auto-freshens into a new session created inside the new directory. An
  empty directory returns the base fingerprint byte-for-byte, so a project with
  no berths configured never freshens anything.
- **Never auto-reset a dirty berth.** `Ensure` warns on stderr and reuses it.
  Discarding uncommitted work to tidy a launch path is not shipmates' call.
- **Never adopt or delete a non-worktree directory.** A directory sitting at the
  berth path that git does not know about is an error on `Ensure` and a refusal
  on `Remove`.
- **Removal refuses while a nested per-issue worktree is live** (see below),
  and that refusal stands even under `--force`. `--force` bypasses the dirty
  check only.
- **Memory and policy stay worktree-independent.** shipmates itself keeps
  running at the canonical root, so `project.MemoryDir`, the policy snapshot and
  the manifest all resolve there no matter where the child works. This matters:
  memory is gitignored by default, so a fresh worktree's
  `.shipmates/memory/<persona>/` is **empty**. A cwd-based memory mechanism
  would silently read nothing inside a berth — which is exactly why the
  SessionStart memory hook resolves memory against the shipmates process cwd and
  injects it, rather than letting the child find it on disk. Berths and memory
  loading are orthogonal, and must stay that way.

## Nested per-issue worktrees

Under `github` routing a berthed persona runs `git worktree add …` *relative to
its berth*, producing a worktree nested inside a worktree. Git tolerates this —
all worktrees share one `.git` — but berth removal must not run while a nested
tree is live, or routing work in flight is fractured.

`HasNestedWorktree` uses two signals:

1. **Authoritative:** `git worktree list` reports a registered worktree at a
   path underneath the berth. Location-agnostic, so a routing convention that
   moves cannot silently disable the guardrail.
2. **Convention directories:** a non-empty `<berth>/.shipmates/worktrees/` or
   `<berth>/.claude/worktrees/`, which catches a half-created or already
   unregistered tree whose files are still on disk.

**Flagging an inconsistency rather than picking a winner:** three conventions
for per-issue worktrees are live in this repo at once.
`catalog/routing/github.md` now emits `.shipmates/worktrees/<short-name>`;
v0.4.0's berth code looked for `.claude/worktrees/`; and this repository's own
agent instructions require `.claude/worktrees/<branch-or-task-name>`. Berth
removal treats both directory spellings as "work in flight" because both
genuinely exist on disk in projects routed before the rename. Choosing one is a
routing-catalog decision, not a berth-teardown decision — it should be made
deliberately, in one place, and this document should be updated when it is.

## Known-open questions

1. **Session-resume directory scoping (needs-runtime-verification).** Whether a
   runtime binds a resumable session to the directory it was created in is a
   question about Claude Code / Codex internals, not about shipmates. The
   fingerprint rule above makes the answer non-load-bearing for correctness — a
   directory change creates a new session rather than moving an old one — but
   migrating an existing session *into* a berth without freshening it stays
   unsupported.
2. **`shipmates berth prune`** for berths orphaned by a repo move or a persona
   rename is not implemented; `berth remove` and `shipmates remove` cover
   deliberate teardown.
3. **`shipmates berth` is not yet in `docs/cli-reference.md`.** The command is
   documented here instead; the reference needs a matching entry.
