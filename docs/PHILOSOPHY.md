# Why Shipmates

The persona-catalog tools — claude-skills, VoltAgent, Agency Agents — ship **headless experts**. A system prompt, a role description, a tool allowlist, and that's it. Every invocation starts from zero. There is no memory of the previous review, the rejected pattern, the constant that hurt to land, the gotcha somebody learned at 2am.

The result: you spend ~800 input tokens setting up the project context on every single call, and the expert spends ~300 output tokens telling you generic best-practice things you mostly already knew. High input, low output, **low signal-to-noise** — and the expert never gets better at your project specifically. They can't. There's no place for them to put what they'd have learned.

Shipmates flips that. Each persona has a per-project memory directory. They read it on session start. They write to it as they learn. Over weeks of work on one codebase, the architect persona accumulates a picture of *this project's* shape — the patterns it uses on purpose vs by accident, the approaches it tried and rejected, the constants whose values are load-bearing for reasons the code doesn't explain.

That changes what an AI reviewer can do. A headless reviewer can only catch things visible in the diff. A reviewer with memory can catch things visible only against **the diff's history** — silent regressions, drift back toward rejected approaches, edits to load-bearing constants whose load-bearing-ness isn't documented at the call site.

## A worked case: a load-bearing constant

One project on this fleet runs an expensive classification pipeline over a live input stream. Buried in it is a "stability freeze": once the classifier is confident, the whole pipeline short-circuits — no re-detection, no cascade, no re-normalization. The cached result is re-emitted until the input drifts past a threshold.

From that project's `AGENTS.md`:

> When `frozen`, the pipeline short-circuits: no detect, no cascade, no normalize. We re-emit the cached `Result`... This is what stops near-identical classes from cycling — their inputs DO change subtly sample to sample as ambient conditions shift, but if we don't run the cascade on those samples, the cascade can't flip its mind.

And the gotcha right below it:

> **If you change the cascade weights or the freeze thresholds, regression-test against the hard-case fixture set specifically** — that's the canonical stress test.

Now imagine a PR that nudges `FREEZE_AFTER` from `90` to `60`, or relaxes `DRIFT_THRESHOLD`. The author thinks they're tuning responsiveness — the pipeline feels sticky, they want it snappier.

### What a headless reviewer sees

A diff. Two integer constants moving. Unit tests pass. There's no name in the code that says "load-bearing for near-identical classes." There's no comment at the call site explaining why these numbers are conservative. The reviewer suggests adding a benchmark, maybe asks for a config flag instead of a constant, signs off. It looks like a reasonable tuning change.

### What a Shipmates persona with memory catches

The architect persona reads `.shipmates/memory/architect/` at session start. In it, among other things:

- `decisions/stability-freeze.md` — "Freeze constants were widened after the hard-case fixture regression caused label swapping between near-identical classes. The cascade can't separate them because the sample deltas come from ambient noise, not from any real change in the subject. The conservative freeze is the fix."
- `gotchas.md` — a pointer back to the AGENTS.md note about regression-testing against the hard-case fixtures.

The reviewer's response writes itself:

> This change tightens `FREEZE_AFTER`. We deliberately widened it after the hard-case regression, because shorter freeze windows let the cascade flip between near-identical classes — the sample deltas come from ambient noise, not from a real change in the subject. The unit tests won't catch the regression because the failure mode only shows on the hard-case fixtures. Before merging this, please run the pipeline against that set and confirm no label swapping. If snappiness is the goal, the right place to tune is probably drift-detection sensitivity, not the freeze window — but worth checking with whoever filed the original responsiveness complaint first.

That review isn't *smarter* than the headless one. It's reviewing **on a different axis**: architectural consistency over time. It catches what a fresh reader, however expert, cannot — because the relevant signal isn't in the diff. It's in the project's history with the diff.

## A humbler example

The stability-freeze case is dramatic. The pattern shows up in smaller ways too, all day.

The architect persona needs to look up an issue thread in a public dependency repo. It runs `gh issue view 42 -R foo/bar`. That call uses the team's `GITHUB_TOKEN` and consumes one request from the shared 5000/hour bucket. On a busy day with several personas fanning out across cross-repo browsing, that bucket runs down faster than you'd think.

A headless reviewer can't fix this. It has no place to put what it learned. So every invocation pays the same call.

The shipmates architect persona writes one line to `.shipmates/memory/architect/known-repos.md` the first time it encounters `foo/bar`: *public*. The next time it needs a read from that repo — and every time after that — it prefixes the call with `GH_TOKEN=` and hits the unauthenticated per-IP bucket instead. The team token is preserved for writes and private-repo reads that actually need it.

This is not a clever optimization. It's the persona getting fractionally faster and more efficient every session at navigating the specific GitHub neighborhood of this specific project. The savings per call are tiny. The behavior is impossible without memory.

That's the whole pitch in miniature: not 10× smarter, just continuously adapting to *this project* in ways a headless catalog has no place to store.

## What this changes about the cost calculus

The headless model has the user paying input tokens to reload context every call. The user is the project's memory; the AI is rented attention.

The memory model puts the project's memory in the *persona*. The user stops paying tokens to reload, and the persona pays a small fixed cost on session start to load its memory dir — once, not per question. The dollars-per-real-review-finding ratio shifts hard. More importantly, the **kind** of finding shifts: the headless reviewer can only find things detectable from the snapshot; the persona-with-memory can find things only detectable across history.

That's not a 10% better reviewer. That's a reviewer doing a job the headless one was structurally incapable of doing.

## What memory does not solve

- **Memory is per persona, per project.** Architect's memory doesn't help security review an auth change. We're not building a unified knowledge graph. Cross-persona sharing is a Phase 2 question.
- **Memory rots.** A persona that wrote down a decision in week one might still be acting on it in week thirty after the decision was reversed. Phase 2: summarization and pruning. Phase 1: trust that early users won't hit the wall for months, and document the failure mode honestly.
- **Memory doesn't replace tests.** It captures *why* and *what was rejected*, not *what currently works*. The test suite remains the source of truth for behavior. Memory is the source of truth for intent.
- **Memory is only as good as what gets written.** The persona's default behaviors (in the markdown body) prompt it to write learnings down, but if you don't tell it the reason for a decision, it can't memorize the reason. The partnership requires the human to occasionally narrate intent. That's a feature — it's what makes the memory worth reading.

## The compounding effect

Week one, the architect persona is no better than a headless one — its memory is just the seed files the catalog shipped with. Week two, it's noticed two patterns and written them down. Week eight, it has thirty decision notes, ten rejected-pattern entries, a `crew-notes.md` describing what the security persona tends to miss, and a `partner-prefs.md` about how the human handles disagreement.

A new contributor's PR lands. The architect reviews. It cites three relevant decisions, flags one drift back toward a pattern the team rejected in week three, and asks about a constraint encoded only in the memory dir. None of that is in the diff. None of it is in the codebase. None of it is in CLAUDE.md.

That's the bet. **The reviewer compounds.** Not because the model got better — because the memory got denser.

That's the only reason to build this.
