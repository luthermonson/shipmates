# The Brig — Ship's Articles operator guide

The **Brig** is shipmates' security-and-hardening subsystem. It carries
fifteen rules — the **Ship's Articles** — that every persona is bound by
the moment it's installed. Five are standards-grounded (OWASP, CWE, NIST,
12-Factor); ten are incident-driven (destructive git, prod DB, secrets,
etc.).

The canonical rules document lives at [`catalog/ARTICLES.md`](../catalog/ARTICLES.md).
This file is the *operator's* guide: what commands to run, when to run
them, and how the Brig behaves in practice.

## Concepts

| Term | What it means |
|------|---------------|
| **Article** | One of the 15 numbered rules (Article 1..15). |
| **Category** | Articles of Code (1-5, standards) vs Articles of Conduct (6-15, incidents). |
| **Layer** | Where enforcement happens: `prompt`, `kernel`, or `freeze`. |
| **Overlay** | A persona's `.shipmates/policies/<persona>.yaml` file. |
| **Freeze** | The emergency-stop marker `.shipmates/freeze`. |
| **Denial log** | Append-only JSONL at `.shipmates/brig.log`. |

## Commands

```
shipmates brig list                     # print all 15 rules
shipmates brig list --code              # only Articles 1-5
shipmates brig list --conduct           # only Articles 6-15
shipmates brig explain 7                # full rule text with rationale + source
shipmates brig log                      # every denial recorded in this project
shipmates brig log --tail=20            # last 20 denials
shipmates brig install                  # merge Brig template into every installed persona
shipmates brig install --dry-run        # print what would change; don't touch disk
shipmates brig install --fleet          # write ~/.shipmates/brig.yaml (fleet baseline)
shipmates brig install --code-scanners  # RESERVED — follow-up PR

shipmates freeze --reason="e2e in progress"
shipmates release
```

## Enforcement layers

The Brig enforces at three layers:

- **Prompt.** Every persona's `developer_instructions` gets a marker-
  delimited block reminding it of the Articles. Composed by shipmates at
  install time and refreshed on `update`.
- **Kernel.** Each persona overlay carries the Brig kernel-policy
  template (commented documentation of the exact `command_exact` rules).
  The operator opts each rule in by copying the entry into their overlay's
  `allow`/`ask`/`deny` array. The shipmates policy loader evaluates the
  overlay on every `process.exec` dispatch and refuses any command whose
  effective policy is `deny`.
- **Freeze.** When `.shipmates/freeze` exists, the Brig treats any Write
  or Edit operation as refused regardless of the persona's overlay. This
  is the emergency-stop button.

Prompt-layer enforcement is persuadable — a sufficiently determined
prompt-injection attack can talk a model out of respecting the Articles.
Kernel-layer enforcement is not: the policy loader is a Go function, and
its decision is final at dispatch. Freeze is the sledgehammer — no writes,
period.

## Installing the Brig into a fresh project

```
$ shipmates init                        # standard init
$ shipmates add backend                 # standard add
$ shipmates brig install                # stamp the Brig template into every overlay
merged Brig template into .shipmates/policies/backend.yaml
```

`shipmates brig install` is safe to re-run — it's idempotent. The block
lives inside marker comments (`# <!-- shipmates:brig:start -->` /
`# <!-- shipmates:brig:end -->`) and re-running the command replaces the
block verbatim without touching the rest of the file.

`shipmates add` and `shipmates update` now stamp the Brig block
automatically. You only need to invoke `shipmates brig install` manually
if you edited the template out or want to re-sync after a shipmates
upgrade.

## Opting rules into active enforcement

The Brig block is *documentation*: rules inside it are commented YAML
and the policy loader ignores them. To make a rule active for a persona,
copy the entry from the marker block into the overlay's real
`allow`/`ask`/`deny` array. Example — enforcing Article 7's
`git push --force` denial for the `backend` persona:

```yaml
# .shipmates/policies/backend.yaml
version: 1
allow: []
ask: []
deny:
  - id: brig_a7_push_force
    kind: process.exec
    match: { command_exact: "git push --force" }
    reason: Article 7 — force-push rewrites shared history

# <!-- shipmates:brig:start -->
# … (Brig template stays here as reference documentation) …
# <!-- shipmates:brig:end -->
```

Why opt-in per rule? The shipmates policy loader matches on **exact
command strings** — it does not glob. The template lists the most-common
literal invocations, but every project uses a slightly different set of
commands. Auto-activating everything would block workflows that don't
exist in your project.

## Freeze and release

Engage the emergency stop when you suspect something is off — a persona
misbehaving mid-session, a failing deploy, an operator who needs to step
away without killing the ship:

```
$ shipmates freeze --reason="deploy verification in flight" --admiral=luther
Brig freeze engaged (reason: deploy verification in flight, admiral: luther).
Marker: /home/luther/proj/.shipmates/freeze
Release with: shipmates release
```

The marker is a JSON file with the reason, admiral, and UTC timestamp.
Any code that consults `brig.CheckFreeze(root)` sees a `frozen=true`
result and refuses writes until you clear it:

```
$ shipmates release
Released Brig freeze (was: deploy verification in flight / luther / 2026-07-23T22:15:04Z).
```

Release is idempotent — running it when no freeze is in effect prints
`No freeze in effect.` and exits cleanly.

## The denial log

Every kernel-layer refusal appends one JSON line to `.shipmates/brig.log`:

```jsonl
{"ts":"2026-07-23T22:14:05Z","persona":"backend","rule":7,"command":"git push --force"}
{"ts":"2026-07-23T22:16:11Z","persona":"tester","rule":13,"command":"rm -rf /tmp/build"}
```

Read the log with `shipmates brig log` or `shipmates brig log --tail=20`.

## The fleet baseline

Some rules should be un-overridable across every project on your machine
or in your organization. `~/.shipmates/brig.yaml` is the fleet baseline —
a subset of Articles the admiral marks as always-deny. Install it once
with `shipmates brig install --fleet`; missing subset is created,
existing subset is left alone (the fleet baseline is authoritative).

The default fleet baseline denies:

- `git push --force origin main` and `git push --force origin master`
- `git filter-repo`
- `touch .env` (representative Article 8 write)
- `sudo tee /etc/hosts` and `chmod 600 ~/.ssh/authorized_keys`
  (representative Article 15 writes)

Extend this file by hand; the shipmates policy loader consumes it in
addition to the project overlays.

## Waivers

A persona overlay can grant an `allow` for a command the Brig `ask`s.
`deny` rules always win — that's the policy loader's precedence
contract (`deny > ask > allow`). If you genuinely need to disable an
Article for a persona (e.g. a persona whose sole job is to bulk-delete
build artifacts), add an explicit `allow` entry and document the reason
in the entry's `reason` field.

## Code scanners (follow-up)

`shipmates brig install --code-scanners` is reserved for a follow-up PR.
It will wire semgrep + owasp-dependency-check into the project as a
pre-commit hook, giving Articles 1-5 (the Code articles) automated
static-analysis coverage in addition to the prompt-layer instruction.
The flag currently prints a planning notice.

## Related docs

- [`catalog/ARTICLES.md`](../catalog/ARTICLES.md) — canonical rules document.
- [`docs/security.md`](security.md) — shipmates security posture and threat model.
- [`docs/configuration.md`](configuration.md) — shipmates.yaml reference.
