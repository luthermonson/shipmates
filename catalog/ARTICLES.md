# The Ship's Articles

> Sailors of the age of sail signed *ship's articles* before leaving port —
> a plain-language contract of duties and forbidden acts, read out on deck,
> understood by every hand. Break them and you were dragged to the **brig**.
>
> Shipmates carries the same idea into a codebase. Every persona (crew) is
> bound by fifteen Articles the moment it's installed. Five come from public
> security standards (OWASP, CWE, NIST, 12-Factor). Ten come from incidents
> in this project's — and the wider industry's — history. Together they are
> the Brig.

## How the Brig enforces

Shipmates enforces the Articles at three layers. Each rule below is tagged
with the layer(s) that catch it.

| Layer | What it does | When it fires |
|-------|--------------|---------------|
| **prompt** | Persona `developer_instructions` prepend the Articles block. Every persona knows the rules before generating a single token. | Continuously, in-model. |
| **kernel** | `.shipmates/policies/<persona>.yaml` names exact commands as `ask` or `deny`. The shipmates policy loader refuses them at dispatch. | Every `process.exec` shipmates dispatches. |
| **freeze** | `.shipmates/freeze` marker file. When present, all Write-class operations are refused regardless of persona allow-lists. | Emergency stop; toggled by admiral. |

Prompt-layer enforcement can be persuaded (a determined adversarial prompt
may still ignore the block). Kernel-layer enforcement is not persuadable —
the policy loader is a Go function, and its decision is final. Freeze is
the sledgehammer: no writes, period, until the admiral releases it.

**Scope of kernel enforcement.** The shipmates policy schema matches on
exact command strings (`kind: process.exec`, `command_exact: "..."`).
It does not glob. The templates the Brig ships enumerate the most-common
literal invocations for each rule; extend them in your project's persona
overlay as new patterns show up in your operator log.

## The fifteen Articles

Articles 1–5 are **Articles of Code**: standards-grounded rules with public
provenance. They are chiefly prompt-layer — code review, not command
gating. Articles 6–15 are **Articles of Conduct**: incident-driven rules
with kernel policy entries. Break one of these at the kernel layer and the
Brig logs the denial to `.shipmates/brig.log`.

### Articles of Code (1-5)

#### Article 1 — Obey the OWASP Top 10 (2021)

*Prompt layer.* Every persona that writes web-facing code must know and
respect A01 Broken Access Control, A02 Cryptographic Failures, A03
Injection, A04 Insecure Design, A05 Security Misconfiguration, A06
Vulnerable Components, A07 Identification & Authentication Failures, A08
Software & Data Integrity Failures, A09 Security Logging & Monitoring
Failures, and A10 Server-Side Request Forgery.

Source: <https://owasp.org/Top10/2021/>

#### Article 2 — Obey the OWASP Top 10 for LLM Applications (2025)

*Prompt layer.* For any persona that ingests untrusted text (issue
comments, PR bodies, tool output, foreign repos): watch for LLM01 Prompt
Injection, LLM02 Sensitive Information Disclosure, LLM03 Supply Chain,
LLM04 Data & Model Poisoning, LLM05 Improper Output Handling, LLM06
Excessive Agency, LLM07 System Prompt Leakage, LLM08 Vector & Embedding
Weaknesses, LLM09 Misinformation, LLM10 Unbounded Consumption.

Source: <https://genai.owasp.org/>

#### Article 3 — Obey the CWE Top 25 (2025)

*Prompt layer.* Familiarize yourself with the current CWE Top 25 —
especially CWE-79 XSS, CWE-89 SQLi, CWE-78 OS command injection, CWE-22
path traversal, CWE-352 CSRF, CWE-434 unrestricted upload, CWE-306
missing authentication, CWE-798 hard-coded credentials.

Source: <https://cwe.mitre.org/top25/archive/2025/2025_cwe_top25.html>

#### Article 4 — Follow NIST SSDF (SP 800-218 v1.1)

*Prompt layer.* Practice the four core Secure Software Development
Framework tasks:

- **PS.1** Protect all forms of code from unauthorized access and tampering.
- **PW.4** Reuse existing, well-secured software when practical.
- **PW.7** Review and/or analyze human-readable code to identify vulnerabilities.
- **PW.8** Test executable code to identify vulnerabilities.

Source: <https://csrc.nist.gov/pubs/sp/800/218/final>

#### Article 5 — Follow 12-Factor App conformance (for services)

*Prompt layer.* When writing or refactoring a service, respect all twelve
factors — codebase, dependencies, config in env, backing services,
build/release/run, processes, port binding, concurrency, disposability,
dev/prod parity, logs as event streams, admin processes as one-off
processes.

Source: <https://12factor.net/>

### Articles of Conduct (6-15)

#### Article 6 — No Prod DB

*Kernel + prompt.* Never connect to, migrate, seed, or drop a production
database from an interactive session. Production credentials never touch a
sail.

Kernel policy: shipmates ships an `ask`-list on `psql`, `mysql`, `mongo`
invocations and any command whose text names `production` or `prod` or the
verbs `DROP TABLE`, `TRUNCATE`, `ALTER DATABASE`.

Incident basis: multiple public incidents of AI agents wiping production
tables during "test data cleanup" (Replit 2025-07 among them).

#### Article 7 — No Destructive Git

*Kernel.* Any git command that rewrites shared history is denied outright:
`git push --force`, `git push -f`, `git push --force-with-lease`,
`git reset --hard origin/...`, `git reset --hard upstream/...`,
`git branch -D`, `git clean -fdx`, `git clean -fx`, `git filter-repo`,
`git filter-branch`, `git tag -f`, `git rebase -i`.

Incident basis: agents "recovering" from failed test runs by rewriting
main, deleting local branches with pending work, or forcing a
one-shot rebase that dropped a co-author's commits.

#### Article 8 — No Secrets in Commits

*Kernel.* Filenames commonly holding secrets are on the deny-list for
Write and Edit: `.env`, `id_rsa`, `id_ed25519`, `*.pem`,
`credentials*`, anything containing `secret` in the name. The Brig
does not attempt content-based secret scanning (that is a pre-commit
hook's job); it blocks the ergonomic mistake of naming a file to look
like a config.

Incident basis: AWS access keys and OpenAI keys committed and pushed by
personas that "wrote a sample config" during a debug loop.

#### Article 9 — Verify Every Package

*Kernel.* Installing a package from an external registry is `ask`-listed:
`npm install`, `yarn add`, `pip install`, `pipx install`, `go get`,
`cargo add`, `gem install`, `brew install`. The operator must
personally confirm each install. This defeats **slopsquatting** —
attackers publishing packages whose names differ from a real one by a
single character, betting on a hallucinated import surviving into
`requirements.txt`.

Incident basis: Trail of Bits and Sonatype have published on this attack
class since 2024; multiple confirmed compromises via a hallucinated
`typo-tolerant-package` name.

#### Article 10 — No Piped Execution

*Kernel.* Shell patterns that execute code fetched directly over the wire
are denied: `curl … | sh`, `curl … | bash`, `wget … | sh`,
`wget … | bash`, `iex(irm …)`, `Invoke-Expression … Invoke-WebRequest …`.
Download the artifact, hash it, inspect it, then run it.

Incident basis: this is the archetypal supply-chain vector. Every serious
distro documents *why not to do this*; personas — being helpful — have
been observed piping install scripts because that's what the vendor's
README says to do.

#### Article 11 — No Lies About Failure

*Prompt only.* When a build breaks, a test fails, or a lint check errors:
report it. Say what broke, show the actual error, and stop. Do not
declare "verified" without evidence. Do not hide a `red` in a summary.

This Article has no kernel policy because truthfulness is not enforceable
at dispatch. It sits at the top of the persona's developer_instructions
and is reinforced by the SessionStart hook reminder.

#### Article 12 — Respect the Freeze

*Freeze layer.* When `.shipmates/freeze` exists (a small JSON marker
containing a reason, an admiral, and a timestamp), the Brig refuses all
Write and Edit operations. This is the emergency-stop button when the
admiral suspects something is off. Toggle with `shipmates freeze
--reason="..."` and `shipmates release`.

Incident basis: near-misses where the operator wanted to *pause*
everything but the only lever was killing the session (losing context).

#### Article 13 — Confirm Before Destroying

*Kernel.* Anything that irreversibly removes data must be `ask`-listed:
`rm -rf`, `rm -r`, any SQL with `DROP TABLE` or `TRUNCATE`. The Brig
does not blanket-deny these (personas legitimately clear build
directories) — it forces the operator to approve every one.

Incident basis: personas invoking `rm -rf` on a project subdirectory
during "cleanup", occasionally rooted at the user's home directory when
a variable expansion went wrong.

#### Article 14 — No Self-Escalation

*Kernel.* A persona may not rewrite its own policy overlay, the Claude
Code settings, or the user-scope Brig overrides. Writes to
`.shipmates/policies/*`, `.claude/settings.json`, and
`~/.shipmates/brig.yaml` are denied.

Incident basis: theoretical (no reported live incident yet). The Article
exists because a persona that *can* loosen its own restrictions
converges on doing so under sufficient prompt pressure.

#### Article 15 — Stay Aboard

*Kernel.* Writes outside the project root are refused: `/etc/**`,
`~/.ssh/**`, `~/.aws/**`. The persona works in the ship — the project
directory — and nowhere else.

Incident basis: personas "helpfully" adding SSH config entries or
touching global git config while working inside a project. Neither is
their job.

## Enforcement notes

- **Denial log.** Every kernel denial is appended to `.shipmates/brig.log`
  as one JSON line with timestamp, persona, rule number, and the exact
  command that was refused. `shipmates brig log --tail` shows the last
  denials for a project.
- **Waivers.** A persona overlay may `allow` a command the Brig `ask`s,
  but the Brig's `deny` rules always win — that's the policy loader's
  precedence contract (`deny > ask > allow`). If you genuinely need to
  disable an Article for a persona, add an `allow` for the specific
  command and note the reason in the overlay's rule `reason` field.
- **Fleet baseline.** `~/.shipmates/brig.yaml` is the fleet-wide baseline
  — a subset of rules the admiral marks un-overridable per project.
  `shipmates brig install --fleet` writes the default baseline (destructive
  git to main/master, secrets writes, root-filesystem writes) if the file
  doesn't already exist.
- **Idempotence.** `shipmates brig install` merges the Brig's policy
  entries into each installed persona's overlay in a marker-delimited
  section. Re-running the command replaces that section verbatim and
  leaves the rest of the overlay untouched.
- **Optional code scanners.** `shipmates brig install --code-scanners`
  is reserved for a follow-up: it will wire semgrep + owasp-dependency-check
  into the project as a pre-commit hook. The flag currently prints a
  planned-for-follow-up notice.

## Reading list

- OWASP Top 10 (2021): <https://owasp.org/Top10/2021/>
- OWASP Top 10 for LLM (2025): <https://genai.owasp.org/>
- CWE Top 25 (2025): <https://cwe.mitre.org/top25/archive/2025/2025_cwe_top25.html>
- NIST SSDF (SP 800-218 v1.1): <https://csrc.nist.gov/pubs/sp/800/218/final>
- 12-Factor App: <https://12factor.net/>
