# Shipmates — Architecture

> First draft. Working doc. Edit aggressively.

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
| **Captain** | The human + AI partnership in the chair. Optional but recommended. The captain has its own long-running session, doesn't claim work, files issues for the crew, pushes back when other personas drift. |
| **Articles** | The rendered persona file installed in a project — the persona's "contract." Lives at `.claude/agents/<name>.md` so Claude Code's existing subagent machinery sees it natively. |

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
domain_glob:
  - "**/*.yaml"
  - "Dockerfile"
  - "package*.json"
  - "go.mod"
memory_dir: .shipmates/memory/security-sam
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
| `domain_glob` | shipmates | Files this persona considers their territory (for review routing) |
| `memory_dir` | shipmates | Where this persona's persistent memory lives |
| `tools` | optional | Standard subagent allowlist if you want to restrict tool access |

Personas that don't care about fleet mode (memory + byline) just drop those fields and remain valid Claude Code subagent files.

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
| **Claude Code subagent** | The persona file IS the artifact. Vendor into `.claude/agents/<name>.md`. Memory dir seeded alongside. | Default. Solo subagent mode and full fleet mode both work from this. |
| **Crew member** (full fleet) | Same persona file PLUS a `shipmates.yaml` entry, CLAUDE.md identity-section snippet, and a GitHub label suggestion for routing | Multi-agent project with captain-and-crew pattern. |
| **AGENTS.md section** (Phase 1) | Render a section in `AGENTS.md` summarizing the persona for tools that respect the multi-vendor convention | For non-Claude-Code tools that read AGENTS.md as a fallback. |
| **Cursor rule** (Phase 2) | Render `.cursor/rules/<name>.mdc` — degraded; static rules, no memory dynamics | Cursor users. |
| **Windsurf rule** (Phase 2) | Render `.windsurf/rules.md` section | Windsurf users. |

Thin targets degrade gracefully: drop the memory-load instructions, condense the markdown body into a static rules section.

### Captain-and-crew pattern

The opinionated full-fleet shape. Optional — solo subagents work without it — but documented as the recommended "advanced" mode.

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
| Gas Town | ❌ (generic polecats) | ❌ | ⚠️ (Mayor exists but as orchestrator process, not human-AI partnership) | partially |
| Anthropic Memory API (Opus 4.6) | ❌ | ✅ | ❌ | n/a |
| LangGraph / Burr | ❌ (build your own) | ✅ (build your own) | ❌ | n/a |
| **Shipmates (this project)** | ✅ small opinionated | ✅ persistent per-project | ✅ first-class | ✅ designed to |

The combination is the gap. No existing tool ships persona + persistent project memory + captain pattern + plug-into-existing-orchestrators.

## Phase 1 scope

What we ship to find out if anyone cares. Time budget: 1-2 weeks.

**Deliverables:**

1. **`shipmates` Go CLI binary** (~500 lines, cross-platform: windows/darwin/linux)
   - `shipmates init` — scaffold `shipmates.yaml` + `.shipmates/memory/` into the current project
   - `shipmates add <persona>` — vendor the persona file into `.claude/agents/` + seed its memory dir
   - `shipmates list` — show installed personas + last-modified time of their memory
   - `shipmates update <persona>` — pull latest persona file from catalog; preserve memory
   - `shipmates remove <persona>` — remove persona file from `.claude/agents/`; keep memory unless `--purge`
   - `shipmates render <persona> --target <agents-md|cursor|windsurf>` — render a thin-target version

2. **Starter persona catalog** — 6 personas, opinionated, each as a real Claude Code subagent markdown file plus a `memory-seeds/` dir of starter context:
   - `captain` — human + AI partnership template (Mayor pattern)
   - `architect` — design / cross-cutting / docs
   - `security` — application security + dep hygiene
   - `frontend` — UI / a11y / performance
   - `backend` — APIs / database / lifecycle
   - `tester` — QA / regression / coverage

3. **Documentation** — `README.md`, `docs/architecture.md` (this file), `docs/PHILOSOPHY.md` going deep on why persistent memory changes review quality, with Card Cannon as a worked example of accumulated-context behavior.

4. **Two installation modes at launch:**
   - **Solo subagent mode:** `shipmates add security` → file lands in `.claude/agents/`, memory seeded, use via the Agent tool inside any Claude Code session
   - **Full fleet mode:** `shipmates init --crew captain,architect,security,frontend,backend` → scaffolds the whole captain-and-crew shape

5. **A "captain" persona that ships as a Claude Code skill** (`.claude/skills/captain/`) — lets anyone using Claude Code try the captain partnership pattern without adopting the rest of shipmates. The minimum-viable evangelism vehicle.

**Explicitly NOT in Phase 1:**

- Cursor / Windsurf / other harness exports (Phase 2)
- Memory summarization / pruning
- A `shipmates serve` daemon (we stay file-based)
- Auto-spawn of agents (that's the routing layer's job)
- The catalog growing past 6 starter personas
- A web UI

## Open design questions

Honest list of things that aren't decided yet.

1. **Memory dir location and git-tracking.** `.shipmates/memory/<persona>/` lives in the project root. Gitignored by default or committed? Arguments both ways: gitignored gives each developer their own learnings (some are personal style); committed gives the team a shared knowledge base (more powerful for code review consistency). Phase 1: ship gitignored-by-default with documented opt-in for shared via a `shipmates.yaml` flag.

2. **Persona conflict resolution.** Two personas claim overlapping `domain_glob`. Which one reviews? Probably: both, and let the conflict surface in their reviews ("Security flagged X; Backend flagged differently"). Better than picking one and silencing the other.

3. **Captain integration.** Should the captain pattern be a *persona* in the catalog or a *separate kind of thing* with its own lifecycle? Leaning toward "captain is a persona with `mode: captain` in frontmatter — same plumbing, opinionated defaults that disable claim/work behaviors and emphasize push-back-and-file-issues."

4. **Memory format evolution.** Plain markdown is simple but unstructured. Phase 1 stays plain markdown. If we ever need querying ("show me all rejected-pattern memories across all personas"), we'd add a thin index. Defer until the pain shows up.

5. **Catalog hosting.** Single monorepo of personas? Per-persona repos? GitHub topics? Phase 1 starts as a single repo (`shipmates-catalog`, or maybe just `catalog/` inside this one repo) since 6 personas is small; revisit at 30+ if it ever happens.

6. **Persona signing / supply chain.** Eventually some personas might be official, others community-contributed. Phase 1 doesn't care; all personas treated equal. Bring up if/when concerns emerge.

7. **Cross-persona memory sharing.** Should personas read each other's memory? E.g., should Architect see what Security has learned? Phase 1: no, isolation by default. Phase 2: optional `shared/` memory subdir personas can read but not all can write.

8. **Where the captain runs.** Captain is a Claude Code session you start. But what's the install/spawn UX? A `shipmates open captain` command that wraps `claude --agent captain --resume captain`? Worth deciding before Phase 1 ships, because it shapes how the captain pattern feels to a new user.

## Glossary

(For the README and onboarding docs.)

- **Persona:** a role-specialized AI identity. Lives as a Claude Code subagent file (`.claude/agents/<name>.md`) with shipmates frontmatter conventions.
- **Memory:** per-project persistent context for a persona. Markdown files in `.shipmates/memory/<persona>/`. Auto-loaded on session start; persona writes to it.
- **Crew:** the assembled personas working on one project. Defined in `shipmates.yaml`.
- **Captain:** the human + AI partnership at the strategic level. Files issues, sets direction, doesn't ship code.
- **Articles:** the rendered persona file installed in a project — the persona's "contract."
- **Memory seeds:** starter markdown files the catalog ships alongside each persona; copied into the project's memory dir on install so the persona starts with structure, not a blank slate.

---

*Working doc, not a spec. Edit before sharing, then edit more after the first user reads it.*
