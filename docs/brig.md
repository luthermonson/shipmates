# The Brig — operator guide

The Brig is shipmates' security-and-hardening subsystem: fifteen rules (the
[Ship's Articles](../catalog/ARTICLES.md)) every persona is bound by the
moment it is installed, an emergency-stop freeze, and an append-only denial
log. This guide covers what the Brig does on your machine, how to operate
it, and exactly what turning it off means.

## The three layers (plus one that isn't the Brig's)

**Prompt.** Every runtime's persona artifact — Claude's
`.claude/agents/<persona>.md`, codex's `.codex/agents/<persona>.md`, the
openai runtime's `.shipmates/runtimes/openai/personas/<persona>.md` —
carries a marker-delimited Articles reminder, spliced in at persona
install/update. Claude sessions additionally get a `shipmates hook
brig-reminder` SessionStart hook that re-states the binding (and announces
an engaged freeze) at the top of every session. `shipmates init` also
vendors the full Articles text to `.shipmates/ARTICLES.md` so the
reminder's pointer resolves inside your project.

**Kernel.** The ten conduct Articles compile into ask/deny rules on the
same permissions evaluator that enforces persona overlays
(`.shipmates/policies/<persona>.yaml`) and project settings
(`.claude/settings.json`). The rules are installed at captain start —
there is no separate `brig install` step; being installed is being bound.
Precedence works in the Brig's favor: deny beats everything, ask beats
allow, so no persona overlay or project rule can shadow a Brig deny or
skip a Brig ask. `shipmates brig list` shows which Articles have kernel
rules; `shipmates brig explain <N>` prints one Article in full.

**Freeze.** `shipmates freeze --reason="..."` writes
`.shipmates/freeze`; while it exists the captain's PreToolUse gate refuses
every Write-class tool call (Write, Edit, MultiEdit, NotebookEdit) —
**including from personas running in bypass mode**. Read-only work
continues; the point is to pause mutation without killing sessions and
losing context. `shipmates release` clears it. `shipmates brig status`
shows the current state. An unreadable or corrupted marker still freezes
(fail closed).

**Fleet (not the Brig's).** The fleet-wide deny list handed down by Fleet
Command is checked before everything else in the permission gate, and —
per Article 14 — it now binds even personas running with
`dangerouslySkipPermissions: true` / `mode: bypassPermissions`. Bypass
still skips the ship-side layers; it no longer shadows the Admiral's deny.
This layer belongs to Fleet Command, not to the Brig, and stays in force
when the Brig is disabled.

## The denial log

Every Brig refusal — kernel Article or freeze — is one decision surfaced
twice: a `permission:auto-deny` event in the captain's feed (what the
bridge and the fleet UI show), and one JSON line appended to
`.shipmates/brig.log` with timestamp, persona, Article number, and the
refused command. Inspect it with:

```
shipmates brig log --tail 20
```

The log is history. Disabling the brig stops new entries; nothing ever
erases old ones.

## Configuration

The Brig is **on by default**: installed means bound; opting out is the
explicit act. The switch lives in the operator's own
`~/.shipmates/config.yaml`:

```yaml
brig:
  enabled: true                            # false turns the Brig off
  disabled_articles: [no-piped-execution]  # waive individual Articles by handle
```

`disabled_articles` takes Article handles (`shipmates brig list` shows
them). A waived Article drops out of both the prompt reminder and the
compiled kernel rules while the other fourteen stay in force. An unknown
handle is warned about and ignored — a typo must not silently waive
something else.

### The trust boundary

The `brig:` block is honored from **user config only**, exactly as
`runtimes:` and `containment:` are: security posture is the operator's
decision. A `brig:` key in a project's `.shipmates/config.yaml` is warned
about and ignored — the project schema structurally has no field for it —
because a repository you clone must not get to un-sign the Articles for
your machine.

### What "off" means, layer by layer

- **Prompt:** the Articles reminder is not spliced at persona
  install/update, and `shipmates update` removes the block from
  previously-composed personas (it lives between markers, so removal is
  surgical). The SessionStart brig-reminder hook stays installed but emits
  nothing. Re-enable and run `shipmates update` to re-inject.
- **Kernel:** brig-sourced rules are not compiled into the evaluator.
  Rules from every other source — fleet policy, persona overlays, project
  settings — are untouched.
- **Freeze:** an active freeze is *suspended*, not erased: the gate stops
  enforcing the marker, but the marker (and its recorded reason) stays on
  disk. Rationale: disabling happens by editing a config file with no
  shipmates process running, so there is nothing that could "release" it
  at that moment, and a read path deleting operator state would be a
  write-on-read surprise. Re-enabling the brig restores the engaged stop
  along with the rest of your previous posture; `shipmates release`
  clears the marker at any time, and `shipmates brig status` calls out a
  suspended marker loudly. `shipmates freeze` refuses to engage while the
  brig is off — writing a marker nothing enforces would be worse than no
  stop at all.
- **Denial log:** new entries stop; the existing log is untouched.
- **Fleet:** unchanged. `brig.enabled: false` returns you to exactly the
  pre-brig security posture; it does not create a new escape from fleet
  policy, and a bypass-mode persona is still denied a fleet-denied tool.

## Command reference

| Command | What it does |
|---------|--------------|
| `shipmates brig status` | Operator posture (enabled/disabled, waived Articles) and freeze state. |
| `shipmates brig list [--code\|--conduct]` | The fifteen Articles with handle, layers, and waived markers. |
| `shipmates brig explain <N>` | One Article in full: rationale, source, layers. |
| `shipmates brig log [--tail N]` | The denial log. |
| `shipmates freeze --reason="..."` | Engage the emergency stop. |
| `shipmates release` | Release it (idempotent; works even with the brig disabled). |

## Scope and honest limits

- Kernel rules match what the evaluator can see: Bash/PowerShell commands
  (compound-split, wrapper-stripped, space-boundary globs) and file-tool
  paths (gitignore-style globs). A persona that writes a secret via
  `bash -c 'echo ... > .env'` redirection is caught only if a rule matches
  the command text; content-based secret scanning is a pre-commit hook's
  job, not the Brig's.
- Article 15 cannot express "any path outside the project root" in the
  pattern language; it denies the representative high-value targets
  (`/etc`, `.ssh`, `.aws`, `.gnupg`). Extend your project settings' deny
  list as new patterns show up.
- Article 11 (No Lies About Failure) is prompt-only by nature —
  truthfulness is not enforceable at dispatch.
- Kernel and freeze enforcement run in the captain's PreToolUse gate. A
  persona launched entirely outside shipmates (bare `claude` in the repo)
  gets the prompt layer only.
