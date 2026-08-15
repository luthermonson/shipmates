# Shipmates — Architecture

> First draft. Working doc. Edit aggressively.
>
> **Scope note (August 2026):** this doc covers the original single-ship core (personas,
> memory, captain-server protocol) and predates Fleet Command, voyages, the Brig, persona
> berths, and multi-runtime support. For those, see [`fleet-architecture.md`](fleet-architecture.md),
> [`sailing.md`](sailing.md), [`brig.md`](brig.md), [`persona-berths.md`](persona-berths.md),
> and [`runtime-interface.md`](runtime-interface.md).

## What Shipmates is

**A toolkit for assembling AI agent personas with persistent project memory.**

You install a small set of role-specialized AI personas into your project. Each one accumulates context about *your* codebase — patterns, decisions, rejected approaches, gotchas — every time you talk to them. Over weeks they get qualitatively better at reviewing, planning, and pushing back on your work because they remember the project's history.

The on-ramp is a single persona running as a Claude Code subagent. The full pattern is a *crew* — multiple personas plus a human-AI partnership in the captain's chair — coordinating work for a project.

## The core insight

The existing persona catalogs (claude-skills, VoltAgent's 100+ subagents, Agency Agents' 100+ experts) ship **headless experts**: system prompts that run in isolated context windows, no memory between invocations.

Every invocation needs you to reload all the project-specific context. You spend ~800 tokens of input setting up the situation so the headless expert can spend ~300 tokens of output giving you generic best-practice advice. **High input, low output, low signal-to-noise.**

Shipmates personas have **persistent per-project memory**. They accumulate context across sessions: what patterns this codebase deliberately uses (vs accidental), which approaches have been tried and rejected, what the team's pushback patterns are, the history of the file they're reviewing, the architectural decisions encoded in earlier PRs.

After two weeks, an architect persona reviewing your PR doesn't just see the diff. It sees the diff against a remembered project: *"This re-introduces the pattern we explicitly rejected in #45 because X — has anything changed?"* That's not 10% better review — that's reviewing on a different cognitive axis (architectural consistency over time) the headless catalog tools can't reach.

**The slogan is: subagents that remember.**

## What Shipmates is NOT

Honest positioning. Adjacent tools and where they live:

| Concern | Tool category | Shipmates' position |
|---|---|---|
| **Routing & lifecycle** (who works on which issue, branch+worktree management, claim-by-label) | [Code Conductor](https://github.com/ryanmac/code-conductor), [Anthropic Agent Teams](https://code.claude.com/docs/en/agent-teams), Gas Town | Out of scope. Use one of these underneath. |
| **Big persona catalog** (51-100+ headless experts) | [claude-skills](https://www.techtimes.com/articles/318518/20260616/ai-coding-agent-skills-library-gives-any-tool-51-senior-engineer-personas.htm), [VoltAgent subagents](https://github.com/VoltAgent/awesome-claude-code-subagents) | Out of scope. We ship a small opinionated starter set; the value is the *memory layer*, not catalog breadth. |
| **Build-your-own agent runtime** | LangGraph, Burr, Temporal | Out of scope. Wrong abstraction level. |
| **Single opinionated persona portable across harnesses** | [Ponytail](https://github.com/DietrichGebert/ponytail) | Adjacent. Borrow the multi-harness export idea. |

**Shipmates fills exactly one gap:** persona + persistent project memory + opinionated assembly pattern. Plug it into whatever routing layer you want.

## Concepts

The vocabulary the project will use consistently. Each term means one thing.

| Term | Definition |
|---|---|
| **Persona** | A role-specialized AI identity. Defined as a single Claude Code subagent file (`.claude/agents/<name>.md`) with shipmates-specific frontmatter conventions. Has a name (default, fully renamable), a byline, a description, a domain glob, and a body containing role + expertise + default behaviors. |
| **Memory** | Per-project persistent context for a persona. Lives as markdown files in `.shipmates/memory/<persona>/`; auto-loaded into the persona's context on session start; written to by the persona as it learns. |
| **Crew** | The assembled set of personas working on one project. Listed in `shipmates.yaml`. May be one persona (solo subagent mode) or many (full fleet mode). |
| **Captain** | The human + AI partnership in the chair on a ship. Optional but recommended. The captain has its own long-running session, doesn't claim work, files issues for the crew, pushes back when other personas drift. Rename to "skipper," "PM," "lead," or whatever fits your team — the role is what matters. |
| **Mate** | An individual crew persona process on a ship (architect, security, etc.) under the captain's coordination. |
| **Fleet Command** | The optional central coordinator node (`shipmates fleet serve`) — web UI, ship tunnels, voice interface (the **Commodore**), and cross-ship graph views. One per fleet; see [`fleet-architecture.md`](fleet-architecture.md). |
| **Admiral** | The human operator commanding the whole fleet. Sets strategic intent; the Commodore reports up to the Admiral. |
| **Commodore** | The AI voice persona embedded at Fleet Command that the Admiral speaks with. Whisper.cpp STT → LLM → Kokoro TTS pipeline; the Commodore is the identity, not a separate component. |
| **Persona artifact** | The rendered persona file installed in a project — the persona's "contract." Lives at `.claude/agents/<name>.md` so Claude Code's existing subagent machinery sees it natively. (Not to be confused with the Brig's Ship's **Articles** — see [`brig.md`](brig.md).) |

## Architecture

### Persona format — Claude Code subagent files, full stop

A shipmates persona **is** a Claude Code subagent file. Standard markdown + YAML frontmatter, written to `.claude/agents/<name>.md`. No separate source format. No compile step for the primary target.

This means:

- Claude Code reads them natively — no new parsing
- `claude --agent <name>` already does what we'd otherwise build a CLI command for
- Compatible with the existing claude-skills / VoltAgent / Agency Agents ecosystem (their personas are already in this format)
- Markdown body reads way better than YAML for the prose-y parts (role descriptions, behaviors, review focus)
- Anyone who knows what a Claude Code subagent is already knows what a shipmate is

Example: a Security persona

```markdown
---
name: security-sam
description: Application security review and dependency hygiene.
byline: "Security-Sam here, …"
domainGlob:
  - "**/*.yaml"
  - "Dockerfile"
  - "package*.json"
  - "go.mod"
memoryDir: .shipmates/memory/security-sam
---

# Role

Application security expert. Owns the security perspective on diffs,
dependency hygiene, secret detection, and authentication patterns.

## Expertise

- OWASP top 10
- Dependency CVE awareness
- Secret detection in diffs
- Authentication & authorization patterns

## Default behaviors

When reviewing peer PRs:
- Run `npm audit` / `go mod audit` before approving any dep change
- Flag hardcoded credentials in the diff
- Ask about input validation on API endpoints

## Memory

This persona reads accumulated project context from
`.shipmates/memory/security-sam/` on session start. Notable files:

- `auth-decisions.md` — past auth/authz decisions with rationale
- `rejected-patterns.md` — security patterns the team has rejected
- `preferences.md` — team's pushback patterns the persona has learned

The persona writes learnings to these files as the project evolves.
Before suggesting a new pattern, reference relevant memory.
```

**Frontmatter fields shipmates conventions add on top of stock subagent format:**

| Field | Required | Purpose |
|---|:---:|---|
| `name`, `description` | yes | Standard subagent file fields, read by Claude Code |
| `byline` | shipmates | Persona's GitHub-comment / chat-message prefix in fleet mode |
| `domainGlob` | shipmates | Files this persona considers their territory (for review routing) |
| `memoryDir` | shipmates | Where this persona's persistent memory lives |
| `tools` | optional | Standard subagent allowlist if you want to restrict tool access |

Personas that don't care about full-crew mode (memory + byline) just drop those fields and remain valid Claude Code subagent files.

### Memory model

Each installed persona gets a directory: `.shipmates/memory/<persona-name>/`. The directory is gitignored by default but easy to commit if a team wants shared memory.

**On session start:** the persona's memory directory is auto-loaded into context. The persona reads it as their accumulated project knowledge.

**During work:** the persona is encouraged (by their default behaviors in the markdown body) to write learnings back. Things like:

- "Decision X was made because Y." → `memory/decisions/auth-method.md`
- "Tried approach A, rejected because B." → `memory/rejected-patterns.md`
- "Recurring pushback from human: prefers Z." → `memory/preferences.md`

**Pruning:** memory grows over time. Phase 2 problem: periodic summarization (the persona condenses old memory into denser knowledge). Phase 1 just lets it grow — early users won't hit the wall for months.

**File format:** plain markdown. Persona can read and write directly with its Read/Edit tools. No custom storage layer.

**Memory seeds:** the catalog ships starter memory files alongside each persona — `catalog/security-sam/memory-seeds/auth-decisions.md` etc. On install, shipmates copies these into `.shipmates/memory/<persona-name>/` so the persona starts with structure rather than a blank slate.

### Export targets

Because the primary target IS the source format, there's no compile pipeline for Claude Code. The persona file goes straight into `.claude/agents/`. Other targets are *renderers* — one-way transformations from the markdown persona file into a thinner format for tools that don't natively read subagent files.

| Target | What happens | When you'd use it |
|---|---|---|
| **Claude Code subagent** | The persona file IS the artifact. Vendor into `.claude/agents/<name>.md`. Memory dir seeded alongside. | Default. Solo subagent mode and full crew mode both work from this. |
| **Crew member** (full crew) | Same persona file PLUS a `shipmates.yaml` entry, CLAUDE.md identity-section snippet, and a GitHub label suggestion for routing | Multi-agent project with captain-and-crew pattern. |
| **AGENTS.md section** (Phase 1) | Render a section in `AGENTS.md` summarizing the persona for tools that respect the multi-vendor convention | For non-Claude-Code tools that read AGENTS.md as a fallback. |
| **Cursor rule** (Phase 2) | Render `.cursor/rules/<name>.mdc` — degraded; static rules, no memory dynamics | Cursor users. |
| **Windsurf rule** (Phase 2) | Render `.windsurf/rules.md` section | Windsurf users. |

Thin targets degrade gracefully: drop the memory-load instructions, condense the markdown body into a static rules section.

### Captain-and-crew pattern

The opinionated full-crew shape. Optional — solo subagents work without it — but documented as the recommended "advanced" mode.

```
        ┌─────────────────────────────────────┐
        │  Captain (human + AI partner)       │
        │  - Has its own long-running session │
        │  - Doesn't claim work               │
        │  - Files issues, sets direction     │
        │  - Pushes back when crew drifts     │
        └─────────────────────┬───────────────┘
                              │
                              ▼
                  ┌───────────────────────┐
                  │  Routing layer        │
                  │  (Code Conductor,     │
                  │   Agent Teams, etc.)  │
                  └───┬───────────┬───┬───┘
                      │           │   │
            ┌─────────┘           │   └─────────┐
            ▼                     ▼             ▼
    ┌────────────┐        ┌────────────┐  ┌────────────┐
    │  Security  │        │  Frontend  │  │  Backend   │
    │   Sam      │        │   Fiona    │  │   Bran     │
    │  + memory  │        │  + memory  │  │  + memory  │
    └────────────┘        └────────────┘  └────────────┘
```

The captain is the strategic layer (decides what's worth doing). The routing layer is the dispatch primitive (who's working what). The crew personas are the executors, each with accumulated context for their domain.

Shipmates doesn't ship the routing layer — that's Code Conductor / Agent Teams / whatever the user picks. Shipmates ships the persona files, the memory infrastructure, and the captain primitives.

### Persona lifecycle

```
install   → persona file vendored into .claude/agents/<name>.md
seed      → memory-seeds/ from the catalog copied into
            .shipmates/memory/<name>/
resume    → on session start, persona's identity (from frontmatter +
            body) + memory dir loaded into context
work      → persona does its job; writes new learnings into its memory
update    → user runs `shipmates update <persona>` to pull the new
            persona file from the catalog (existing memory preserved;
            persona file refreshed)
uninstall → persona file removed from .claude/agents/; memory dir
            preserved unless --purge
```

The memory dir is the persona's accumulated wisdom about your project. Treat it the way you'd treat a senior engineer's notes — don't blow it away just because you're updating their tool.

### Embedded catalog & update flow

The whole catalog — persona files, memory seeds, slash commands, the settings.json snippet — is **embedded into the Go binary at build time** via `//go:embed`. There is no runtime catalog fetch, no separate catalog repo to pin, no network dependency. The binary version is the catalog version.

**Repo layout (source of truth):**

```
catalog/
  captain/
    .claude/agents/captain.md   → vendored to .claude/agents/captain.md
    memory-seeds/               → copied to .shipmates/memory/captain/ on first install
    policy.yaml                 → vendored to .shipmates/policies/captain.yaml
  architect/
  security/
  first-mate/
  ...
  charters/             (drain / autonomous charter templates)
  commands/
    standup.md          → vendored to .claude/commands/standup.md
    sync-routing.md
  routing/
    github.md           (routing-conventions template, composed into personas)
  skills/               (.claude/skills/<name>/ for distribution)
  ARTICLES.md           → vendored to .shipmates/ARTICLES.md (the Ship's Articles)
```

In the Go source:

```go
//go:embed catalog
var catalog embed.FS
```

**Update flow.** `shipmates update` reads every embedded file, computes its SHA, and compares against `.shipmates/manifest.json` (recorded at install time). Four cases:

| Case | Action |
|---|---|
| File missing in project | Add it (only for personas/commands the user previously `add`'d; orphans stay gone) |
| Present, unchanged from baseline | Overwrite with new shipped version; update manifest |
| Present, user-edited, catalog unchanged | Leave alone |
| Present, user-edited AND catalog updated | **Prompt with diff** (see below) |

**Conflict prompt.** When a file has diverged AND a new catalog version exists, render a unified diff between the user's current file and the new shipped version, then prompt:

```
Conflict: .claude/agents/security.md
  Your version (sha: a3f8…) diverges from baseline (sha: 1e4c…).
  Catalog has a new version (sha: 7b29…).

  --- your version
  +++ shipped 1.4.0
  @@ -12,7 +12,9 @@
  -## Expertise
  +## Domain expertise
   …

  [k] keep your version              (default)
  [t] take the new shipped version
  [s] save shipped as security.md.new (sidecar; merge manually)
  [d] re-show diff
  [a] keep yours for all remaining conflicts
  [T] take theirs for all remaining conflicts
```

Diff renderer is an in-process unified diff (e.g. `sergi/go-diff`), ANSI-colored when stdout is a TTY, plain otherwise. No external `git` dependency.

**Non-TTY behavior.** In CI / piped runs, default to **keep yours** for every conflict and exit with a summary. Never silently stomp user changes in non-interactive mode. `--accept ours|theirs` resolves all conflicts non-interactively when the caller knows what they want.

**Memory is sacred.** `shipmates update` never touches `.shipmates/memory/<persona>/`. Memory seeds are copied **only on first `shipmates add <persona>`** and never overwritten thereafter — the persona's accumulated knowledge is the user's, not ours. Updates to seed templates only affect *new* installs.

**Orphans.** If a persona or command is removed from the catalog in a future binary version, the user's installed copy stays on disk. They can `shipmates remove` it themselves; we never delete user files unprompted. (`shipmates list` today prints catalog personas and whether each is installed — it does not flag orphans.)

**Versioning.** `shipmates --version` prints the binary/catalog version (same value). `.shipmates/manifest.json` records the version at last successful `update`, so the CLI can detect "you're on 1.4 but your project was last updated against 1.2" and offer to bring it current.

### Captain-server protocol

When the captain session needs live coordination with one or more crew, it spawns a transient local HTTP server. **One shared server per project**, lifetime bounded by captain-session presence + worker activity. Memory remains file-based; the server is transient IPC for live coordination, not persistent state.

**Lifecycle.**

```
captain session starts
  └─ `shipmates ask <persona>` call:
       1. server up? if no, pick a free port (bind 127.0.0.1:0),
          write .shipmates/sessions/server.port + server.pid
       2. wait for GET /health
       3. ref-count++
       4a. FIRST delegation to this persona (no session yet):
           exec claude -p --session-id <uuid> --name <repo>-<persona>
                --agent <persona>
                --settings <hooks pointing at the recorded port>
                "<prompt>" < /dev/null
       4b. SUBSEQUENT delegations (session exists):
           exec claude -p --resume <repo>-<persona> --agent <persona>
                --settings <hooks ...> "<prompt>" < /dev/null
       5. wait for crew exit
       6. ref-count--
       7. if ref-count == 0: POST /shutdown
```

> **Verified empirically (June 2026).** `--session-id <uuid>` is **create-only** — reusing it errors with "Session ID already in use." To continue a worker's session, use `--resume <value>`, where `<value>` is either the session UUID *or* the `--name` set at creation (both confirmed working in `-p` mode). The resumed session retains full memory of prior turns — a worker recalls what it was told in earlier delegations. **This is turn-based**, not mid-flight injection: each delegation runs one turn and exits; you cannot inject into an in-progress turn, only queue the next one. Also: `claude -p` blocks ~3s waiting on stdin unless you redirect it (`< /dev/null`).

Server stays up across back-to-back delegations (warm path). It shuts down when no crew is running and the captain's `SessionEnd` fires — or via watchdog if the captain died ungracefully.

**Endpoints.**

| Endpoint | Caller | Purpose |
|---|---|---|
| `POST /register` | server-driven on crew spawn (`SessionStart` hooks don't fire for `-p` crew — see finding 9 below) | register run, ref-count++ |
| `POST /deregister` | crew `SessionEnd` hook | ref-count-- |
| `POST /events` | crew `PreToolUse`, `PostToolUse`, `Stop`, `MessageDisplay` hooks | activity firehose |
| `POST /permission/<persona>/<id>` | crew `PermissionRequest` hook | **blocking** allow/deny |
| `POST /tell/<persona>` | captain (via `shipmates tell`) | inject a message into a live crew process's stdin |
| `GET /feed` | captain (via Bash) | tail recent activity |
| `GET /pending` | captain | currently-waiting permission requests |
| `POST /resolve/<id>` | captain / external relay | answer a pending permission |
| `GET /health` | wrapper script | wait-for-ready |
| `POST /shutdown` | wrapper or `SessionEnd` hook | graceful drain + exit |

**Talking to a live crew member (`shipmates tell`).** The server brokers messages in both directions: crew→server via hooks, and **captain→crew** via `shipmates tell`. The captain never touches stream-json:

```
shipmates tell security "double-check PR 10 for auth regressions"
  └─ CLI reads .shipmates/sessions/server.port
  └─ POST /tell/security  { "message": "double-check PR 10 ..." }
       └─ server wraps it as a stream-json user message:
          {"type":"user","message":{"role":"user",
            "content":[{"type":"text","text":"double-check PR 10 ..."}]}}
       └─ writes that line to the live `security` process's stdin
       └─ crew receives it (even mid-work — verified) and streams
          its response back out, which the server tees to /feed
```

This requires the crew member to be running as a **live stream-json process** (`claude -p --input-format stream-json --output-format stream-json`), held open by the server, rather than a one-shot. Two execution models coexist:

- **One-shot** (`shipmates ask <persona> "..."`): transient `--resume` subprocess, one turn, exits. Cheap, fire-and-forget.
- **Live** (`shipmates open <persona>` then `shipmates tell <persona> "..."`): server spawns and holds a persistent stream-json process you can steer conversationally while it works. The server owns its stdin/stdout.

Mid-work messages are received and processed in the same session (verified June 2026); a `tell` is queued/steered rather than hard-cancelling the in-flight turn.

**Signal handling & lifecycle teardown.**

The server has to die when the captain session dies — including ungraceful deaths (Ctrl-C, crash, kill -9, OOM, terminal closed). Two complementary mechanisms:

1. **Primary: `SessionEnd` hook.** The captain's `settings.json` registers a `SessionEnd` hook that runs `shipmates server stop`, which POSTs `/shutdown` and escalates to OS kill if the server didn't exit within the drain window. Handles `/exit`, `Ctrl-D`, and (we believe) `Ctrl-C`.

2. **Backup: parent-watchdog inside the server.** At spawn time, server records the captain's PID. A goroutine polls every ~3s:
   - Unix: `kill(pid, 0)` returns `ESRCH` → parent dead → drain → exit
   - Windows: `OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, …, pid)` returning `ERROR_INVALID_PARAMETER` → parent dead → drain → exit

The watchdog is unconditional and handles every "ungraceful death" case regardless of whether `SessionEnd` fired.

**Per-platform signal handling inside the server.**

| Platform | Catch | Action |
|---|---|---|
| Linux/macOS | `SIGTERM`, `SIGINT` | drain (~3s): respond to pending permission requests with `deny`, flush event log, close listener, exit 0 |
| Windows | `os.Interrupt` (Go maps `CTRL_C_EVENT` → `os.Interrupt`; `CTRL_BREAK_EVENT` → `syscall.SIGBREAK`) | same drain |

**Windows quirk: spawn detached.** Spawn `shipmates-server` with `CREATE_NEW_PROCESS_GROUP` so the user's Ctrl-C doesn't double-deliver to the server via console event broadcast. Server's only kill paths become the `SessionEnd` hook and the parent-watchdog. Predictable behavior on the platform that least supports "just SIGTERM it."

Optional hardening (Phase 2 unless we hit problems): assign the server to a Windows Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Kernel kills the server when the parent handle closes — belt-and-suspenders for "what if the watchdog itself wedges."

**Port allocation.** Server binds `127.0.0.1:0`; kernel picks a free port. Port is written to `.shipmates/sessions/server.port`. The hook config inlined into each crew's `--settings` reads from there so two shipmates projects on the same machine don't collide.

**Crash recovery for ref counts.** If a crew member is killed mid-run, its `SessionEnd` hook never fires and the server holds a phantom ref forever. Two safety nets:

- Each crew run heartbeats on every `PostToolUse` (effectively a "still alive" ping); server expires refs whose last heartbeat is older than N seconds (default 30).
- Overall idle timeout: if no events received in M minutes (default 5), server shuts down regardless of ref count.

### Per-persona permission policy

Different personas warrant different levels of trust. A `tester` running on a developer's laptop might run freely; a `security` persona on a production-adjacent box should ask before every mutating action. Shipmates handles this with a **thin translation layer** over Claude Code's existing permission machinery — we do not invent a parallel allowlist vocabulary.

**Where allow/deny lists live: `.claude/settings.json`, unchanged.** Crew subprocesses inherit the project's `permissions.allow` / `permissions.deny` from Claude Code's normal user→project→local settings cascade. The same allowlist your interactive sessions use. Shipmates does not duplicate or override this.

**What shipmates adds: two per-persona knobs.**

| Knob | Effect when shipmates spawns crew |
|---|---|
| `mode` | sets `--permission-mode <default\|acceptEdits\|bypassPermissions\|plan>` |
| `dangerouslySkipPermissions` | sets `--dangerously-skip-permissions` (trust-the-agent escape hatch) |

`dangerouslySkipPermissions` and `mode: bypassPermissions` are **operator-only**
— they come from `~/.shipmates/personas.yaml`, never from the checkout, because
a cloned repository must not be able to waive the human gate. See
[docs/security.md](security.md#persona-execution-config-is-operator-owned).

That's it. Allow/deny patterns stay in `.claude/settings.json` because that's where Claude Code already reads them and where the team's other tooling already writes them.

**Layered policy resolution.** Last writer wins:

1. **Persona default** (catalog frontmatter)
2. **Project override** (`shipmates.yaml`)
3. **Operator override** (`~/.shipmates/personas.yaml`) — the only layer
   outside the checkout, and so the only one that may bypass the gate

```yaml
# persona frontmatter (catalog default)
permissions:
  mode: acceptEdits

# shipmates.yaml (project override) — mode nests under permissions:
crew:
  security:
    permissions: { mode: ask }

# ~/.shipmates/personas.yaml (operator override)
personas:
  backend:
    dangerouslySkipPermissions: true
```

**Catalog default modes** — `acceptEdits` for anything that edits code, `ask` for non-executors and strategic personas:

| Persona | Default mode | Why |
|---|---|---|
| captain | `ask` | rarely executes; defensive when it does |
| architect | `acceptEdits` | edits docs/READMEs constantly; Bash still asks |
| security | `acceptEdits` | edits configs/lockfiles in audits; Bash still asks |
| frontend | `acceptEdits` | UI iteration churns files |
| backend | `acceptEdits` | edits handlers all day; Bash for migrations asks |
| tester | `acceptEdits` | test edits constantly; Bash still asks |

Defaulting all executors to `ask` would create approval fatigue (captain auto-clicks yes; gating becomes theater). `acceptEdits` lets file edits flow while still gating `Bash`, `WebFetch`, and other side-effecting tools — which is where the real risk lives.

**Composition at spawn time.**

```
shipmates ask security "review the diff"
  └─ resolve mode: persona default → shipmates.yaml crew override
  └─ exec claude
        --agent security
        --session-id <uuid>
        --permission-mode acceptEdits                # from resolved policy
        [--dangerously-skip-permissions]             # if dangerouslySkipPermissions
        --settings <json: HTTP hooks ONLY>
        "review the diff"

  Crew subprocess loads:
    - user/project/local .claude/settings.json     (cascade) → allowlist respected
    - our --settings additive payload              (hooks)   → server visibility
```

**`PermissionRequest` hook is conditional.** Registered only when there's something to ask — i.e., not in `dangerouslySkipPermissions` mode. In skip-permissions mode, the hook never fires inside Claude Code; we still emit `PostToolUse` events to `/events` so the human operator can see what got auto-run. The escape hatch is "don't block me," not "hide what's happening."

**One verification before code lands.** `--settings <json>` must be **additive** (merges with the discovered settings cascade), not **replacing**. The help text reads "load *additional* settings from," which strongly implies merge — but worth a 30-second test. If it replaces, shipmates has to read the cascade first, deep-merge, and pass the merged JSON.

### Per-persona session options

Beyond permissions, a persona's frontmatter (overridable per project via `shipmates.yaml` `crew:`) can carry session-spawn options:

| Knob | Effect when shipmates spawns crew |
|---|---|
| `remoteControl` | enables `--remote-control [name]` on `shipmates open <persona>` |
| `model` | passes `--model <model>` to every spawn of this persona (`ask`, `tell`, `open`, `fanout`, live server) — run cheap personas (e.g. `tester`) on a faster model and deep ones (`architect`) on a stronger one. Empty = claude's configured default. |
| `effort` | passes `--effort <low\|medium\|high\|xhigh\|max>` to every spawn of this persona — dial reasoning depth per role (e.g. `architect: high`, `tester: low`). Empty = claude's default. |

```yaml
remoteControl: false           # default — off
remoteControl: true            # on; auto-name = persona name
remoteControl: "mobile-lead"   # on; custom session name for findability in the app
```

**Applies to interactive sessions only.** Compatible with `shipmates open <persona>` (long-running interactive); **incompatible** with `shipmates ask <persona>` (one-shot `--print` mode — non-interactive and ephemeral). If the user invokes `shipmates ask <p>` while `<p>` is actively remote-controlled, the wrapper refuses with a clear message ("session is being driven from the app; close it there first").

**Trust posture.** `--remote-control` routes session traffic through **Anthropic-hosted relay infrastructure** so the desktop / mobile app can drive it. That's a meaningfully different trust posture than the hooks-only path (which is all local IPC). Shipmates surfaces this in the `shipmates open` output when remoteControl is on, so users opt in knowingly.

**Catalog defaults.** Off for every persona. Sensible project-level override: `captain: { remoteControl: true }` — the captain is the long-running strategic session a user might genuinely want to talk to from their phone.

## Comparison to existing tools

Honest table.

| Tool | Persona library | Per-project memory | Captain pattern | Plugs into existing orchestrators |
|---|:---:|:---:|:---:|:---:|
| claude-skills (51 personas) | ✅ | ❌ | ❌ | ❌ |
| VoltAgent subagents (100+) | ✅ | ❌ | ❌ | ❌ |
| Agency Agents (100+) | ✅ | ❌ | ❌ | ❌ |
| Ponytail (1 persona, portable) | ⚠️ one | ❌ | ❌ | ❌ |
| Code Conductor | ❌ (generic agents) | ❌ (per-task scratch) | ❌ | (it IS the orchestrator) |
| Anthropic Agent Teams | ❌ (generic teammates) | ❌ (shared task list only) | ❌ (Conductor is orchestrator-coded, not partner-coded) | (it IS the orchestrator) |
| Gas Town | ❌ (generic polecats) | ❌ | ⚠️ (Mayor exists as orchestrator process, not human-AI partnership) | partially |
| Anthropic Memory API (Opus 4.6) | ❌ | ✅ | ❌ | n/a |
| LangGraph / Burr | ❌ (build your own) | ✅ (build your own) | ❌ | n/a |
| **Shipmates (this project)** | ✅ small opinionated | ✅ persistent per-project | ✅ first-class | ✅ designed to |

The combination is the gap. No existing tool ships persona + persistent project memory + captain pattern + plug-into-existing-orchestrators.

## Phase 1 scope

What we ship to find out if anyone cares. Time budget: 1-2 weeks.

**Deliverables:**

1. **`shipmates` Go CLI binary** (cross-platform: windows/darwin/linux). Catalog is embedded via `//go:embed catalog`, so the binary is the single distribution artifact.
   - `shipmates init` — scaffold `shipmates.yaml` + `.shipmates/memory/` + manifest into the current project
   - `shipmates add <persona>` — vendor the persona file into `.claude/agents/` + seed its memory dir
   - `shipmates list` — show catalog personas and which are installed
   - `shipmates update [<persona>]` — refresh installed files from the embedded catalog, with diff-on-conflict prompt; preserves memory
   - `shipmates remove <persona>` — remove persona file from `.claude/agents/`; keep memory unless `--purge`
   - `shipmates render <persona> --target <agents-md|cursor|windsurf>` — render a thin-target version
   - `shipmates open <persona>` — launch a long-running interactive session for that persona (uses recorded `--session-id`)
   - `shipmates ask <persona> "<prompt>"` — one-shot delegation through the captain-spawned server
   - `shipmates tell <persona> "<message>"` — send a string to a live crew process (CLI → JSON → server → crew stdin); talk to it while it works
   - `shipmates fanout <p1,p2,…> "<prompt>"` — parallel delegations
   - `shipmates server stop` — graceful server shutdown (invoked by the captain's `SessionEnd` hook)

2. **Starter persona catalog** — 6 personas, opinionated, each as a real Claude Code subagent markdown file plus a `memory-seeds/` dir of starter context:
   - `captain` — human + AI partnership template (Mayor pattern); rename to `skipper` / `lead` etc. if preferred
   - `architect` — design / cross-cutting / docs
   - `security` — application security + dep hygiene
   - `frontend` — UI / a11y / performance
   - `backend` — APIs / database / lifecycle
   - `tester` — QA / regression / coverage

3. **Documentation** — `README.md`, `docs/architecture.md` (this file), `docs/PHILOSOPHY.md` going deep on why persistent memory changes review quality, with a worked case study of accumulated-context behavior.

4. **Two installation modes at launch:**
   - **Solo subagent mode:** `shipmates add security` → file lands in `.claude/agents/`, memory seeded, use via the Agent tool inside any Claude Code session
   - **Full crew mode:** `shipmates init --crew captain,architect,security,frontend,backend` → scaffolds the whole captain-and-crew shape

5. **A "captain" persona that ships as a Claude Code skill** (`.claude/skills/captain/`) — lets anyone using Claude Code try the captain partnership pattern without adopting the rest of shipmates. The minimum-viable evangelism vehicle.

**Explicitly NOT in Phase 1:**

- Cursor / Windsurf / other harness exports (Phase 2)
- Memory summarization / pruning
- A *persistent* `shipmates serve` daemon. (The captain-spawned transient server IS Phase 1 — see "Captain-server protocol." We stay file-based for memory; the server is transient IPC only.)
- Auto-spawn of agents (that's the routing layer's job)
- The catalog growing past 6 starter personas
- A web UI
- Windows Job Object hardening for the server (parent-watchdog covers it; Job Object is Phase 2 if needed)

## Open design questions

Honest list of things that aren't decided yet.

1. **Memory dir location and git-tracking.** `.shipmates/memory/<persona>/` lives in the project root. Gitignored by default or committed? Arguments both ways: gitignored gives each developer their own learnings (some are personal style); committed gives the team a shared knowledge base (more powerful for code review consistency). Phase 1: ship gitignored-by-default with documented opt-in for shared via a `shipmates.yaml` flag. (`.shipmates/manifest.json` should always be committed regardless, so `shipmates update` semantics are consistent across the team.)

2. **Persona conflict resolution.** Two personas claim overlapping `domainGlob`. Which one reviews? Probably: both, and let the conflict surface in their reviews ("Security flagged X; Backend flagged differently"). Better than picking one and silencing the other.

3. **Captain integration.** Should the captain pattern be a *persona* in the catalog or a *separate kind of thing* with its own lifecycle? Leaning toward "captain is a persona with `mode: captain` in frontmatter — same plumbing, opinionated defaults that disable claim/work behaviors and emphasize push-back-and-file-issues."

4. **Memory format evolution.** Plain markdown is simple but unstructured. Phase 1 stays plain markdown. If we ever need querying ("show me all rejected-pattern memories across all personas"), we'd add a thin index. Defer until the pain shows up.

5. **Catalog hosting.** Single monorepo of personas? Per-persona repos? GitHub topics? Phase 1 starts as a single repo (`shipmates-catalog`, or maybe just `catalog/` inside this one repo) since 6 personas is small; revisit at 30+ if it ever happens.

6. **Persona signing / supply chain.** Eventually some personas might be official, others community-contributed. Phase 1 doesn't care; all personas treated equal. Bring up if/when concerns emerge.

7. **Cross-persona memory sharing.** Should personas read each other's memory? E.g., should Architect see what Security has learned? Phase 1: no, isolation by default. Phase 2: optional `shared/` memory subdir personas can read but not all can write.

8. **Where the captain runs.** ~~Worth confirming `--session-id` resumes-if-exists.~~ **RESOLVED (verified June 2026).** `--session-id` is create-only (reuse errors). The spawn path: first run `claude --session-id <uuid> --name <repo>-<persona> --agent <persona>` to create; every subsequent run `claude --resume <repo>-<persona> --agent <persona>` to continue. `--resume` accepts the `--name` value as its key (confirmed in `-p` mode), so the repo-prefixed name is the stable resume handle and no per-persona UUID file is strictly required for the common path (though recording the UUID as a collision-proof fallback is cheap insurance).

9. **Claude Code hook surface.** **RESOLVED for the permission story (verified June 2026).** `type: "http"` hooks are real and work in `-p` stream-json mode: a `--settings` config routes `PreToolUse`/`PostToolUse` to the shipmates server's `/hook/<persona>/<event>` endpoint, tagged with persona. Two key findings:
   - **There is no separate `PermissionRequest` event in headless `-p` mode** — `PreToolUse` *is* the gate. It returns `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"|"deny"|"ask"}}`. An empty `200` is treated as **allow** (so an observe hook silently bypasses gating unless it returns a decision). `"ask"` blocks the tool (nothing can prompt in `-p`).
   - **The operator-approval flow works via a blocking `PreToolUse`.** For gated tools (currently `Bash`/`PowerShell`), the server holds the request, surfaces it (`shipmates pending`), and blocks until a human runs `shipmates allow <id>` / `shipmates deny <id>`, then returns the decision. Verified both ways: allow → tool runs (`whoami` → `luthe`); deny → tool blocked, crew reports the denial. The hook's `timeout` (set to 120s) bounds how long it waits.
   - **Session lifecycle hooks (verified June 2026):** in `-p` mode, `Stop` (end of a response) and `SessionEnd` (session teardown) **fire**, but `SessionStart` does **not** reach the hook. Implication: the ref-count can't be driven by a `SessionStart` hook for crew (which run in `-p`); it must be server-driven (increment when `ensureLive` spawns a process, decrement on process exit). The current idle-timeout is the working lifecycle bound regardless; ref-count is informational/not yet wired to hooks.
   - **Mid-work steering (verified June 2026):** a `tell` sent while a crew member is working is injected into the running turn and the model adapts at the **next step boundary** (after the in-flight tool completes) — it does NOT hard-cancel a tool already executing, and does NOT wait for the entire multi-step plan to finish. Test: a 3-step task interrupted after step 1 ran only step 1, then complied with the new instruction. So "talk to it while it works" means cooperative steering, not preemption.

10. **Does `SessionEnd` fire on Ctrl-C / SIGINT?** Open. If `SessionEnd` only fires on graceful exit (`/exit`, `Ctrl-D`), then the parent-watchdog becomes the *primary* server-shutdown mechanism rather than a backup — still works, just with ~3s detection latency instead of immediate. If it does fire on SIGINT, immediate shutdown is the common path. Worth testing during Phase 1 implementation.

11. **Permission timeout for sleeping humans.** `PermissionRequest` hook has a finite timeout (~30s default per the docs research). If the operator is asleep / not at the desk when a crew member fires a permission request, the hook times out and Claude denies. Phase 1 default: deny on timeout; operator re-runs the delegation later. Phase 2 options: an "away mode" with a stricter pre-approval policy, or long-poll / SSE if Claude's hook layer ever supports it.

12. **`--settings <json>`: additive or replacing?** The captain-server hook injection and per-persona permission policy both assume the flag *adds to* the discovered user/project/local settings cascade rather than replacing it. The help text reads "load additional settings from," which implies merge — but if it turns out to replace, shipmates must read the cascade itself, deep-merge our payload, and pass the merged JSON. Cheap to verify; cheap to fix either way.

### Claude Code CLI integration

The flags shipmates leans on (from `claude --help`):

| Flag | Use |
|---|---|
| `--agent <name>` | Launch a session directly as a persona. `shipmates open <persona>` wraps this. |
| `--session-id <uuid>` | **Create-only.** Sets a known UUID at session creation (first delegation). Reusing it on a later call errors ("already in use"). Recorded under `.shipmates/sessions/`. |
| `--resume <value>` | **Continue a session.** `<value>` is the session UUID *or* the `--name` set at creation — both verified in `-p` mode. This is how every delegation after the first reaches the worker. Resumed sessions retain full memory of prior turns. |
| `--name <name>` | Session name — shown in prompt box, `/resume` picker, terminal title, **and usable as the `--resume` key**. Format is `<prefix>-<persona>` (e.g. `shipmates-security`). `shipmates init` writes the repo directory name into `sessionPrefix` in `shipmates.yaml`; the value is used verbatim, and an **empty** `sessionPrefix` means no prefix (session name is just `<persona>`). Lets you disambiguate two checkouts of the same repo / same-named projects, or opt out entirely. This is the stable resume handle, so no per-persona UUID bookkeeping is needed for the common path. |
| `--add-dir <dirs...>` | If memory ever lives outside cwd, grant the persona tool access. Not needed for the default `.shipmates/memory/<persona>/` layout. |
| `--plugin-dir <path>` | How the Phase 1 captain-as-skill ships for users who haven't installed the full shipmates CLI. |

Gotcha: `claude -p` waits ~3s for stdin before proceeding; shipmates redirects stdin (`< /dev/null`) on every non-interactive spawn to skip the delay.

Flags shipmates explicitly does **not** rely on: `--append-system-prompt` (the persona body handles memory-load instructions; no need to inject from CLI), `--bare` (we want default discovery of `.claude/agents/`).

## Glossary

(For the README and onboarding docs.)

- **Persona:** a role-specialized AI identity. Lives as a Claude Code subagent file (`.claude/agents/<name>.md`) with shipmates frontmatter conventions.
- **Memory:** per-project persistent context for a persona. Markdown files in `.shipmates/memory/<persona>/`. Auto-loaded on session start; persona writes to it.
- **Crew:** the assembled personas working on one project. Defined in `shipmates.yaml`.
- **Captain:** the human + AI partnership at the strategic level on a ship. Files issues, sets direction, doesn't ship code. (Rename to "skipper," "lead," etc. to taste.)
- **Mate:** an individual crew persona process on a ship, working under the captain.
- **Fleet Command:** the optional central coordinator node (`shipmates fleet serve`) — one per fleet. Runs the web UI, ship tunnels, and voice interface. See [`fleet-architecture.md`](fleet-architecture.md).
- **Admiral:** the human operator commanding the whole fleet; sets strategic intent.
- **Commodore:** the AI voice persona the Admiral talks to at Fleet Command (whisper.cpp STT → LLM → Kokoro TTS).
- **Persona artifact:** the rendered persona file installed in a project — the persona's "contract." (The Brig's Ship's *Articles* are a different thing: the fifteen security rules in [`brig.md`](brig.md).)
- **Memory seeds:** starter markdown files the catalog ships alongside each persona; copied into the project's memory dir on install so the persona starts with structure, not a blank slate.

---

*Working doc, not a spec. Edit before sharing, then edit more after the first user reads it.*
