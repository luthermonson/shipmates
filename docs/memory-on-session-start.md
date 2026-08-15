# Memory on session start — making "auto-loaded" deterministic

> **Status (August 2026): shipped.** The option this doc recommends — a single managed
> `SessionStart` **command hook** — is implemented: `shipmates init` wires the hook into
> `.claude/settings.json`, and it shells out to `shipmates hook load-memory`, which reads the
> active persona's `.shipmates/memory/<persona>/` and injects it as session context
> (`internal/commands/install.go`, `internal/commands/hookcmd.go`). The live-server / PTY
> stream-json path loads memory separately via `--append-system-prompt`, as recommended.
> The body below is the original proposal and empirical analysis, kept as the record.
>
> Working doc / proposal. Companion to `architecture.md` (persona/memory fundamentals) and
> `persona-berths.md` (worktree isolation, a separate concern). **Updated after two empirical tests on the
> current Claude Code resolved every hook-behavior question this doc turned on.** The first drafts assumed
> `SessionStart` never fires in `-p` and recommended a per-mode split with `--append-system-prompt` as the
> primary `-p` mechanism — over-generalizing an in-repo note that is actually **correct but narrow**
> (http hooks on the stream-json/server path). The tests show the picture is path-specific: a `command`
> `SessionStart` hook **does** fire in plain `-p` (with persona identity free via `agent_type`), while an
> `http` `SessionStart` hook does **not** fire on the server's stream-json path (but `UserPromptSubmit`/
> `Stop` do). So a single `command` `SessionStart` hook is the primary mechanism for the dispatch crew and
> interactive surfaces — **zero shipmates code changes** — and the live-server/PTY path loads memory
> separately (`--append-system-prompt`, or an optional `UserPromptSubmit` http hook).

## The promise, and the gap

`architecture.md` defines **Memory** as *"per-project persistent context for a persona … **auto-loaded
into the persona's context on session start**."* (`architecture.md:45`, `:127`, `:584`.) That's the whole
value proposition — "subagents that remember."

Today that promise is kept by a **prompt instruction**, repeated near-verbatim in every persona's agent
file:

> **Read your memory first.** Every session start, read everything in `.shipmates/memory/<persona>/`.

(`catalog/captain/.claude/agents/captain.md`, `backend.md`, `architect.md`, `security.md`, `frontend.md`,
`tester.md`.) The captain's copy is the sharpest: *"If you skip the read, you become a generic advisor and
the partnership degrades."*

**That instruction is discretionary.** It relies on the model choosing to obey — nothing enforces it. And
it fails in practice. The captain session that prompted this doc read **3 of its 20** memory files before
acting, became precisely the "generic advisor" `captain.md` warns about, and only recovered when the human
forced the read. "Auto-loaded" is aspirational; the mechanism is a hope. The fix is to make the harness
load the memory, not the model.

## The verified mechanism (empirical, current Claude Code)

The captain ran a `type: command` hook harness that logs each event's stdin payload while driving real
`claude -p` invocations. Results on the **currently installed** Claude Code:

- **`SessionStart` fires in `-p`.** Confirmed across every launch shape shipmates uses: plain
  `claude -p "prompt"` (`source: startup`), `-p --resume <id>` (`source: resume`),
  `-p --output-format stream-json --verbose` (startup), and
  `-p --input-format stream-json --output-format stream-json` (startup). Every one fired `SessionStart`.
- **Persona identity is free.** Launched with `--agent backend`, **every** payload — `SessionStart`,
  `UserPromptSubmit`, `Stop`, `SessionEnd` — carried `"agent_type":"backend"`, alongside `cwd` and (for
  `UserPromptSubmit`) the full `prompt`. shipmates passes `--agent <persona>` on every launch
  (`internal/project/sessionlaunch.go:33`, `:36`), so the hook reads `.agent_type` from stdin and knows the
  persona with **no env var, no cwd/basename trick, no berths**.
- `UserPromptSubmit` and `Stop` also fire in plain `-p`, on both first-run (startup) and `--resume`.

This overturns the earlier drafts' working assumption (that `SessionStart` never fires in `-p`) — not by
proving the in-repo note wrong, but by showing that note is **path-specific** (it describes http hooks on
the server's stream-json path; see the next section) and does not apply to command hooks on plain `-p`. The
two hook questions the first drafts flagged as unknown are now **resolved: yes and yes.** The mechanism is a
`type: command` `SessionStart` hook keyed on `.agent_type`, loading `.shipmates/memory/<agent_type>/`.

### Reconciling with the in-repo note — both findings are true, precisely scoped

shipmates' own source says, in three places, that `SessionStart` doesn't fire in `-p`
(`internal/server/server.go:600-601`, `:619-620`, `internal/server/beads.go:26-28`; echoed in
`architecture.md:553`). **That note is correct — not stale.** A second empirical test (below) confirmed it,
precisely scoped: it describes `type: http` hooks on the server's stream-json path, which is a *different*
path from the plain-`-p` command hooks the primary mechanism uses. Both results hold at once:

| Hook type | Launch path | `SessionStart` | `UserPromptSubmit` / `Stop` |
|---|---|:---:|:---:|
| `command` (discovered `.claude/settings.json`) | plain `-p`, `--resume` — the **dispatch crew** | **fires** (startup + resume) | fire |
| `http` (`--settings`, `server.go:550-568`) | persistent stream-json input — the **live/PTY server mate** | **does not fire** | **fire** (carry `agent_type`) |

So the note is **correct-but-narrow: http/stream-json specific.** It does not threaten the primary
recommendation, because the dispatch crew, `open`, and the interactive captain all use **command** hooks on the
plain-`-p`/interactive path — where `SessionStart` fires. The http miss constrains only the live-server/PTY
stream-json path (next section).

## Why this needs zero shipmates code

Everything the hook depends on already exists:

- **shipmates already passes the identity.** `--agent <persona>` on every spawn
  (`sessionlaunch.go:33`, `:36`, the single source of truth `SessionLaunch` shares across ask/drain/
  fanout/open/live/PTY). CC surfaces it to the hook as `agent_type`. No env plumbing (the earlier "export
  `SHIPMATES_PERSONA`" idea is now unnecessary).
- **The memory path is a stable convention.** `project.MemoryDir(persona)` is
  `.shipmates/memory/<persona>` (`project.go:33-34`), resolved relative to the session's cwd. The
  `memoryDir:` frontmatter every catalog persona carries is decorative — **no Go path consumes it**
  (`render.go:16-18` drops it; the launch parser `project.go:318-329` omits it). Every catalog file sets it
  to exactly the conventional path anyway, so the hook should key on the convention
  (`.shipmates/memory/<agent_type>/`), not parse frontmatter.
- **Hooks are discovered, not suppressed.** shipmates never passes `--bare` (`LaunchFlags` emits only
  `--dangerously-skip-permissions`/`--permission-mode`/`--model`/`--effort` —
  `internal/project/project.go:296-313`), so CC's normal `.claude/settings.json` discovery is live.

So the hook is pure configuration: read `.agent_type` from stdin, `cat .shipmates/memory/<agent_type>/*`
(or emit a "read your memory dir" directive), let CC inject it as `SessionStart` context. No new Go.

## Wiring — where the hook lives

The hook fires in whatever project the session's cwd resolves to, so wiring follows cwd:

- **Plain-`-p` dispatch crew (`ask`, `drain`, `drain-many`, `fanout`) launch from shipmates' own cwd — the
  repo root.** No spawn site sets `cmd.Dir` (`run.go:57` `dispatchTo`, `fanout.go:100` `oneShotDelegate`;
  `drain`→`dispatch` `charters.go:120`, `drain-many`→`dispatchTo` `charters.go:179`), so the child inherits
  the repo root and CC discovers **repo-root `.claude/settings.json`** as its project settings. That is
  exactly where the hook belongs for the crew. Their `.shipmates/memory/<agent_type>/` (relative to the
  repo root) is the canonical, populated copy — no worktree, no empties.
  - This **resolves the earlier ancestor-merge worry.** CC settings precedence has no ancestor-directory
    merge, but none is needed here: the crew's project dir *is* the repo root, so root-level wiring is
    already at their project root.
- **The interactive captain runs inside its berth** (`.shipmates/berths/captain`, a hand-maintained worktree —
  see `persona-berths.md`), so its project dir is the berth. Its hook belongs in the **berth's own**
  `.claude/settings.json`, loading the captain memory that lives in the berth. (This is coherent because the
  captain's memory-location tracks its session cwd — the general berth-vs-gitignored-memory trap in
  `persona-berths.md` §4 bites only if you run a persona in a berth while its memory lives *only* in the
  root checkout; it does not bite the crew, who run at the root, nor the captain, whose memory lives in its
  berth.)
- **`shipmates open <persona>`** is interactive `claude` from the repo root (`open.go:50`, no `cmd.Dir`), so
  it uses the same repo-root `.claude/settings.json` as the dispatch crew. Covered by the same wiring.

**Shipping it.** The mechanism needs no Go, but the wiring is a `.claude/settings.json` fragment shipmates
does not manage today (the catalog ships no `settings/` subtree; `init`/`add` write only
`.claude/agents/`, `.claude/commands/`, and memory seeds — `install.go:262`, `:347-352`). Two delivery
options:

1. **Hand-added (zero shipmates change):** document the hook fragment; users paste it into repo-root
   `.claude/settings.json`. Works immediately for the downstream fleet.
2. **Managed (the durable form):** shipmates vendors the hook + settings fragment through the catalog and
   merges it on `install`/`update`, SHA-preserving user edits as it already does for commands. Cost:
   shipmates gains a `settings.json` **merge** surface it deliberately lacks (it only writes whole files it
   owns) and authors an *executed* artifact — a bigger blast radius that wants a human-review story (open
   question C).

## The live-server / PTY stream-json path (resolved, with two options)

The live stream-json mate (`ensureLive`, `server.go:588-605`) and PTY mate (`ensurePTY`, `ptyproc.go:147`)
are the surfaces the in-repo note describes: they run under the server's `type: http` hooks and, for the
live mate, a **persistent** `--input-format stream-json` process handling many turns. A second empirical
test — a Python driver that mimics the live server exactly (persistent stream-json input, stdin held open,
stdout read, a full turn driven, `type: http` lifecycle hooks pointed at a local listener) — settles both
previously-open questions:

- **A — resolved: an `http` `SessionStart` hook does *not* fire in stream-json mode.** The listener was up
  and caught `UserPromptSubmit` moments later, so this is a real negative, not a startup race. This
  **confirms** the in-repo note (`server.go:600-601`, `:619-620`, `beads.go:26-28`) for its exact context.
- **B — resolved: `http` `UserPromptSubmit` and `Stop` *do* fire across a full turn in stream-json mode**,
  and both payloads carry `agent_type` (verified `agent_type:"backend"`). Only http `SessionStart` is the
  miss.

So this path can't use a `SessionStart` hook, but it has two viable memory-load options:

**Option (default) — `--append-system-prompt`.** The server already uses it here for beads prime
(`server.go:602-603`, `ptyproc.go:152-153`) and the Commodore (Fleet Command's AI officer) uses it for the captain prompt
(`claudebrain.go:97`). It is per-invocation, rides the system prompt rather than the transcript
(`claudebrain.go:93-95`), shipmates has the persona in hand, and it reads canonical (root) memory — a
proven, worktree-independent, one-shot injection. Simplest; recommended default.

**Option (flagged, worth evaluating) — a first-turn `http` `UserPromptSubmit` hook.** Because http
`UserPromptSubmit` *does* fire in stream-json (unlike `SessionStart`) and carries `agent_type`, the server
could inject memory through its **existing** `/hook/<persona>/<event>` plumbing (`server.go:161`,
`hookSettings` `server.go:550-568`, `handleHook` `server.go:357-396`): add `UserPromptSubmit` to
`hookSettings`, and in `handleHook` read `project.MemoryDir(persona)` and return it as `additionalContext`.
Identity is **free** via the endpoint tag (`r.PathValue("persona")`, `server.go:358`) — no
`--append-system-prompt` string-building, no per-spawn plumbing. Two honest caveats before it's proven:

- **(a) Firing is confirmed; the injecting *response* is not.** The test verified the hook **fires** but the
  listener returned an empty `{}` — it did **not** verify that an http hook *response* can return
  `additionalContext` that actually reaches the model on that turn. That is one more verification away from
  proven (it's the same schema question the server already answers for `PreToolUse` permission decisions at
  `server.go:382-390`, which is encouraging but not a guarantee for `UserPromptSubmit`).
- **(b) It fires every turn.** `UserPromptSubmit` is not first-turn-only, so re-injecting full memory each
  turn would bloat context. It needs **server-side first-turn-only guarding** (the server already tracks
  each mate's live proc — `s.live[persona]` — so a "primed" flag is cheap).

**Recommendation for this path:** ship `--append-system-prompt` as the default; treat the first-turn
`UserPromptSubmit` http hook as an option to evaluate once caveat (a) is verified — its payoff is dropping
per-spawn string-building in favor of the server's existing hook plumbing with free identity.

(The `UserPromptSubmit` http hook is *not* a unified primary across all surfaces — it's scoped to the
live/PTY path above. The dispatch crew and `open` don't pass `--settings` and never touch the server
(`dispatchTo`/`oneShotDelegate`/`open` spawn `claude` directly), so a server-mediated hook can't reach them;
they're covered by the command `SessionStart` hook. Two mechanisms, two paths.)

## Berths are a separate concern

The prior draft's "Option 3 (real berths)" — per-persona git worktrees with `cmd.Dir = berth` — was
motivated partly as a way to get identity from cwd. **That motivation is gone:** identity is free via
`agent_type`, and running a persona in a berth would actively *break* a cwd-relative memory hook, because a
fresh worktree carries only tracked files and memory is gitignored by default (`install.go:60-62`;
`architecture.md:125`) — the berth's `.shipmates/memory/<persona>/` would be empty. Berths are now purely a
filesystem-isolation feature, evaluated on their own merits in **`persona-berths.md`** (verdict there: not
worth their own worktree-lifecycle code near-term). They must not be coupled to memory-load, and the hook
must stay cwd-canonical (crew at repo root; captain in its berth where its memory lives).

## Mechanism comparison

| | `SessionStart` command hook *(primary)* | `--append-system-prompt` *(fallback)* |
|---|---|---|
| Fires/works in plain `-p` | **verified** (current CC, startup + resume) | proven (beadsPrime, captain prompt) |
| Reaches dispatch crew (`ask`/`drain`/`drain-many`/`fanout`) | **yes** (repo-root settings discovery) | yes (flag at spawn) |
| Reaches interactive `open` / captain | yes (settings discovery) | yes (flag) |
| Reaches live-server / PTY stream-json | **no** (http `SessionStart` doesn't fire — resolved) | yes (already the beadsPrime path; or a `UserPromptSubmit` http hook) |
| Persona identity | free (`agent_type` in stdin) | free (`persona` is the spawn-fn arg) |
| shipmates Go changes | **none** (config-only; managed wiring optional) | append at server/PTY sites (already present for beads) |
| Fires on resume | yes (`source: resume`) | yes (per-invocation) |
| Blast radius | executed hook (managed form) | none new |

## Recommendation (architect)

**Primary: a `type: command` `SessionStart` hook keyed on `.agent_type`, loading
`.shipmates/memory/<agent_type>/`.** It is verified on the current Claude Code, needs **zero shipmates Go
changes** (shipmates already passes `--agent`; CC already exposes `agent_type`; the memory path is a fixed
convention), and it covers the interactive captain, `shipmates open`, **and** the plain-`-p` dispatch crew
(`ask`/`drain`/`drain-many`/`fanout`) on **both first-run and resume**. This is the deterministic
enforcement the doc set out to find, and it is dramatically cheaper than every option the prior drafts
weighed.

**Wiring:** repo-root `.claude/settings.json` for the dispatch crew and `open` (they launch from the repo
root); the interactive captain's berth carries the hook in its own `.claude/settings.json`. Ship it hand-added
first (zero change, immediate), then optionally as a shipmates-managed catalog artifact (the durable,
single-source-of-truth form) once the human-review-of-executed-artifacts story is settled.

**Second path (live-server / PTY stream-json):** the `http` `SessionStart` hook does not fire there
(resolved), so this path loads memory separately. Default to `--append-system-prompt` (the beads-prime seam,
unchanged); optionally adopt a first-turn `UserPromptSubmit` http hook once its `additionalContext` response
is verified (caveat (a) above). Do not build either for the dispatch crew; the command `SessionStart` hook
already covers them.

**Berths:** out of scope here; see `persona-berths.md`. Identity no longer needs them.

Net: the empirical test turns a three-option, per-mode-split design into a one-line-of-config answer for the
surfaces that were failing. Ship the command `SessionStart` hook.

## Open questions

**All Claude Code behavior questions are now resolved by two empirical tests** (a `type: command` harness on
plain `-p`, and a Python driver mimicking the live server with `type: http` hooks on persistent stream-json
input). Nothing blocking the recommendation remains open.

- ~~Does `SessionStart` fire in `-p`?~~ **Yes** (command hooks) — verified across plain `-p`, `--resume`,
  and both stream-json launch shapes.
- ~~Does CC expose the active `--agent` to a hook?~~ **Yes** — every event payload carries
  `"agent_type":"<persona>"`; shipmates passes `--agent` at `sessionlaunch.go:33,36`.
- ~~(A) Does an `http` `SessionStart` hook fire in the server's stream-json mode?~~ **Resolved: no.** Real
  negative (the listener caught `UserPromptSubmit` moments later). **Confirms** the in-repo note
  (`server.go:600-601`, `:619-620`, `beads.go:26-28`) for its exact context — correct-but-narrow.
- ~~(B) Do `UserPromptSubmit`/`Stop` fire across a full turn under stream-json input?~~ **Resolved: yes**,
  via `http`, both carrying `agent_type` (`agent_type:"backend"` verified). Only http `SessionStart` misses.

**Remaining — design choices and one optional-path verification (none block shipping the primary hook):**

1. **(Optional-path verification) Can an `http` `UserPromptSubmit` hook *response* return `additionalContext`
   that reaches the model?** The test confirmed the hook **fires** but the listener returned empty `{}`, so
   the injecting response is unverified. *Only matters if adopting the flagged `UserPromptSubmit` option for
   the live/PTY path;* the default `--append-system-prompt` needs no such verification. Plausible — the
   server already returns `hookSpecificOutput` for `PreToolUse` (`server.go:382-390`) — but confirm before
   building.
2. **Managed-hook delivery.** If shipmates ships the hook (vs hand-added), it authors an executed artifact
   and needs a `settings.json` merge surface it lacks today (`install.go:262` writes whole owned files
   only). How does that fit a human-review-of-executed-artifacts posture? (Policy, not CC behavior.)
3. **Inject directive vs content.** A "read your memory dir" directive is cheap and lets the model triage;
   emitting concatenated file contents as `SessionStart` context is fully deterministic but unbounded —
   couples to the memory-pruning problem deferred to Phase 2 (`architecture.md:135`). Pick per token budget;
   resolvable in the hook script. (Design, not CC behavior.)
4. **Scope the in-repo note, and fix the diagrams.** The note is **correct** — annotate it as
   http/stream-json-specific so no future reader over-generalizes it to command hooks on plain `-p`
   (`server.go:600-601`, `:619-620`, `beads.go:26-28`, `architecture.md:553`). Separately, the diagrams were
   **wrong**: `docs/diagrams.md` (and the since-removed `.mmd` duplicates) showed `SessionStart hook → POST
   /register` for crew, but crew run under http/stream-json where `SessionStart` does *not* fire — the
   ref-count is server-driven (`server.go:619-620`). The diagram has been corrected.

[^cc]: Claude Code docs on hooks and headless mode: <https://code.claude.com/docs/en/hooks>,
    <https://code.claude.com/docs/en/cli-reference>, <https://code.claude.com/docs/en/headless>. Settings
    precedence (no ancestor-directory merge): <https://code.claude.com/docs/en/settings>. The `SessionStart`
    firing, `agent_type` payload, and http/stream-json event claims are from two empirical tests on the
    currently installed Claude Code — a `type: command` harness on plain `-p`, and a Python driver mimicking
    the live server (persistent stream-json input, full turn driven) with `type: http` hooks — not from
    these docs.
