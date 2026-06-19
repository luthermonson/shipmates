# Shipmates

**Subagents that remember.**

A toolkit for assembling a small crew of role-specialized AI personas — architect, security, frontend, backend, tester, lead — that accumulate per-project memory across sessions. Two weeks in, the architect reviewing your PR doesn't just see the diff. It sees the diff against a remembered project: *"This re-introduces the pattern we rejected in #45 because X — has anything changed?"*

That's not a 10% better review. That's review on a different cognitive axis (architectural consistency over time) that headless persona catalogs can't reach.

## Why this exists

The existing catalogs (claude-skills, VoltAgent, Agency Agents) ship **headless experts**: system prompts that run in isolated context windows, no memory between invocations. Every call costs ~800 input tokens reloading the situation for ~300 tokens of generic best-practice. High input, low output, low signal.

Shipmates personas have **persistent per-project memory**. They write down what worked, what got rejected, the team's pushback patterns, the rationale behind earlier decisions. On the next session, they read it back. Over weeks, that compounds.

## 30-second quickstart

```bash
# install one persona into the current project
shipmates init
shipmates add security

# option A: use it inside any Claude Code session via the Agent tool
claude  # then: "have security review the diff"

# option B: launch a session directly as that persona
claude --agent security
```

Or jump to the full crew:

```bash
shipmates init --crew lead,architect,security,frontend,backend,tester

# open the long-running lead session (stable UUID, resumes every time)
shipmates open lead
```

This vendors persona files into `.claude/agents/<name>.md` (Claude Code reads them natively — no new runtime) and seeds each persona's memory directory at `.shipmates/memory/<name>/` with starter context. Personas write to that directory as they learn your project.

## The starter crew

Six personas, opinionated, engineering-department flavored. Rename any of them to fit your team — "lead" can become "captain," "skipper," "PM," whatever.

| Persona | Owns |
|---|---|
| **lead** | Strategy, direction, push-back. The human + AI partnership in the chair. Doesn't ship code. |
| **architect** | Cross-cutting design, docs, consistency over time |
| **security** | OWASP, dep hygiene, secret detection, auth patterns |
| **frontend** | UI, accessibility, perf, browser concerns |
| **backend** | APIs, database, request lifecycle |
| **tester** | QA, regression, coverage |

## What Shipmates is not

Honest positioning — adjacent tools and where they live:

| Concern | Tool category | Shipmates' position |
|---|---|---|
| Routing & lifecycle (claim-by-label, branch/worktree management) | Code Conductor, Anthropic Agent Teams, Gas Town | **Out of scope.** Plug shipmates personas into one of these. |
| Big persona catalog (50–100+ headless experts) | claude-skills, VoltAgent subagents, Agency Agents | **Out of scope.** We ship a small opinionated starter set; the value is the memory layer, not catalog breadth. |
| Build-your-own agent runtime | LangGraph, Burr, Temporal | **Out of scope.** Wrong abstraction level. |

Shipmates fills exactly one gap: **persona + persistent project memory + opinionated assembly pattern.** Bring your own routing layer.

## Deeper reading

- [`docs/architecture.md`](docs/architecture.md) — the working architecture doc (persona format, memory model, lifecycle, lead-and-crew shape, open questions)
- [`docs/PHILOSOPHY.md`](docs/PHILOSOPHY.md) — why persistent memory changes review quality, with a worked case study

## Status

Working draft. Phase 1 scope: Go CLI (~500 LOC) + 6 starter personas + docs. Not shipped yet.
