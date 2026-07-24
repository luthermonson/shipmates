// Package brig implements the Ship's Articles — shipmates' security and
// hardening subsystem. It carries the canonical rule inventory (metadata,
// provenance, source), a helper that merges the shipped kernel policy into
// a persona overlay in an idempotent marker-delimited section, a freeze
// marker check for the emergency stop, and an append-only JSONL denial log.
//
// The Brig has no runtime or process-execution dependencies. Enforcement
// lives in the existing internal/policy loader (kernel layer), in the
// runtime installers (prompt layer reminder), and in the persona inventory
// composer (prompt layer Articles block). This package is the source of
// truth for what those enforcement points enforce.
package brig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Category is the coarse rule grouping — Articles of Code (standards-grounded,
// prompt-layer) vs Articles of Conduct (incident-driven, kernel-enforced).
type Category string

const (
	CategoryCode    Category = "articles-of-code"
	CategoryConduct Category = "articles-of-conduct"
)

// Layer identifies the enforcement layer that catches a given rule.
type Layer string

const (
	LayerPrompt Layer = "prompt" // developer_instructions + SessionStart reminder
	LayerKernel Layer = "kernel" // .shipmates/policies/<persona>.yaml deny/ask
	LayerFreeze Layer = "freeze" // .shipmates/freeze marker
)

// Rule is the canonical description of one Article. It is deliberately a
// data-only value — enforcement lives elsewhere; this record is what the
// `shipmates brig list|explain` commands render and what the policy merger
// consults when it stamps the kernel section into a persona overlay.
type Rule struct {
	// Number is the 1-based Article number (1..15).
	Number int
	// Handle is a short stable slug, e.g. "no-prod-db".
	Handle string
	// Title is the operator-facing short name, e.g. "No Prod DB".
	Title string
	// Category is Code (1-5) or Conduct (6-15).
	Category Category
	// Layers lists every layer that enforces this Article. May be more than
	// one — Article 6 is enforced at both prompt and kernel layers.
	Layers []Layer
	// Source is the public URL or short citation. Present for Articles 1-5.
	// Articles 6-15 cite the incident basis in their Rationale instead.
	Source string
	// Rationale is one paragraph explaining what the rule guards against
	// and, for Articles 6-15, the incident basis.
	Rationale string
}

// Rules returns the complete Articles inventory in canonical order (1..15).
// The slice is a fresh copy; callers may mutate it without affecting other
// callers or subsequent invocations.
func Rules() []Rule {
	out := make([]Rule, len(canonicalRules))
	copy(out, canonicalRules)
	return out
}

// Get returns the Article with the given number (1..15) or an error if
// the number is out of range.
func Get(number int) (Rule, error) {
	if number < 1 || number > len(canonicalRules) {
		return Rule{}, fmt.Errorf("brig: no such Article %d (valid: 1..%d)", number, len(canonicalRules))
	}
	return canonicalRules[number-1], nil
}

var canonicalRules = []Rule{
	{
		Number:    1,
		Handle:    "owasp-top-10",
		Title:     "Obey the OWASP Top 10 (2021)",
		Category:  CategoryCode,
		Layers:    []Layer{LayerPrompt},
		Source:    "https://owasp.org/Top10/2021/",
		Rationale: "Every persona that writes web-facing code must know and respect A01-A10 — Broken Access Control, Cryptographic Failures, Injection, Insecure Design, Security Misconfiguration, Vulnerable Components, Identification & Authentication Failures, Software & Data Integrity Failures, Security Logging & Monitoring Failures, and Server-Side Request Forgery.",
	},
	{
		Number:    2,
		Handle:    "owasp-llm-top-10",
		Title:     "Obey the OWASP Top 10 for LLM Applications (2025)",
		Category:  CategoryCode,
		Layers:    []Layer{LayerPrompt},
		Source:    "https://genai.owasp.org/",
		Rationale: "Any persona that ingests untrusted text (issue comments, PR bodies, tool output, foreign repos) must watch for LLM01-LLM10 — Prompt Injection, Sensitive Information Disclosure, Supply Chain, Data & Model Poisoning, Improper Output Handling, Excessive Agency, System Prompt Leakage, Vector & Embedding Weaknesses, Misinformation, Unbounded Consumption.",
	},
	{
		Number:    3,
		Handle:    "cwe-top-25",
		Title:     "Obey the CWE Top 25 (2025)",
		Category:  CategoryCode,
		Layers:    []Layer{LayerPrompt},
		Source:    "https://cwe.mitre.org/top25/archive/2025/2025_cwe_top25.html",
		Rationale: "Especially CWE-79 XSS, CWE-89 SQLi, CWE-78 OS command injection, CWE-22 path traversal, CWE-352 CSRF, CWE-434 unrestricted upload, CWE-306 missing authentication, CWE-798 hard-coded credentials.",
	},
	{
		Number:    4,
		Handle:    "nist-ssdf",
		Title:     "Follow NIST SSDF practices (SP 800-218 v1.1)",
		Category:  CategoryCode,
		Layers:    []Layer{LayerPrompt},
		Source:    "https://csrc.nist.gov/pubs/sp/800/218/final",
		Rationale: "Practice PS.1 (protect code from unauthorized access and tampering), PW.4 (reuse well-secured software when practical), PW.7 (review or analyze human-readable code for vulnerabilities), and PW.8 (test executable code for vulnerabilities).",
	},
	{
		Number:    5,
		Handle:    "twelve-factor",
		Title:     "Follow 12-Factor App conformance (for services)",
		Category:  CategoryCode,
		Layers:    []Layer{LayerPrompt},
		Source:    "https://12factor.net/",
		Rationale: "When writing or refactoring a service, respect all twelve factors: codebase, dependencies, config in env, backing services, build/release/run, processes, port binding, concurrency, disposability, dev/prod parity, logs as event streams, admin processes as one-off processes.",
	},
	{
		Number:    6,
		Handle:    "no-prod-db",
		Title:     "No Prod DB",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerPrompt, LayerKernel},
		Source:    "incident: Replit 2025-07 and multiple public reports of AI agents wiping production tables during 'test data cleanup'",
		Rationale: "Never connect to, migrate, seed, or drop a production database from an interactive session. Production credentials never touch a sail. The Brig ships an ask-list on psql/mysql/mongo invocations and on any command whose text names 'production' or 'prod' or the verbs DROP TABLE / TRUNCATE / ALTER DATABASE.",
	},
	{
		Number:    7,
		Handle:    "no-destructive-git",
		Title:     "No Destructive Git",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "incident: agents 'recovering' from failed tests by rewriting main, deleting local branches with pending work, force-rebasing away co-author commits",
		Rationale: "Any git command that rewrites shared history is denied outright: git push --force / -f / --force-with-lease, git reset --hard origin/upstream, git branch -D, git clean -fdx / -fx, git filter-repo, git filter-branch, git tag -f, git rebase -i.",
	},
	{
		Number:    8,
		Handle:    "no-secrets-in-commits",
		Title:     "No Secrets in Commits",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "incident: AWS keys and OpenAI keys pushed by personas writing 'sample configs' during debug loops",
		Rationale: "Filenames commonly holding secrets are on the deny-list for Write and Edit: .env, id_rsa, id_ed25519, *.pem, credentials*, anything containing 'secret' in the name. The Brig does not attempt content-based secret scanning (that's a pre-commit hook's job); it blocks the ergonomic mistake of naming a file to look like a config.",
	},
	{
		Number:    9,
		Handle:    "verify-every-package",
		Title:     "Verify Every Package",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "industry: Trail of Bits and Sonatype 'slopsquatting' research (2024-2025); multiple confirmed compromises via hallucinated package names",
		Rationale: "Installing a package from an external registry is ask-listed: npm install, yarn add, pip install, pipx install, go get, cargo add, gem install, brew install. The operator must personally confirm each install. This defeats slopsquatting — attackers publishing packages whose names differ from a real one by a single character.",
	},
	{
		Number:    10,
		Handle:    "no-piped-execution",
		Title:     "No Piped Execution",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "industry: the archetypal supply-chain vector; every serious distro documents why not to do this",
		Rationale: "Shell patterns that execute code fetched directly over the wire are denied: curl | sh, curl | bash, wget | sh, wget | bash, iex(irm ...), Invoke-Expression + Invoke-WebRequest. Download the artifact, hash it, inspect it, then run it.",
	},
	{
		Number:    11,
		Handle:    "no-lies-about-failure",
		Title:     "No Lies About Failure",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerPrompt},
		Source:    "incident: personas declaring builds 'verified' after test failures because the summary paragraph read smoother that way",
		Rationale: "When a build breaks, a test fails, or a lint check errors, report it. Say what broke, show the actual error, and stop. Do not declare 'verified' without evidence. Do not hide a red in a summary. This Article has no kernel policy because truthfulness is not enforceable at dispatch.",
	},
	{
		Number:    12,
		Handle:    "respect-the-freeze",
		Title:     "Respect the Freeze",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerFreeze},
		Source:    "incident: near-misses where the admiral wanted to pause everything but the only lever was killing the session (losing context)",
		Rationale: "When .shipmates/freeze exists — a small JSON marker with reason, admiral, and timestamp — the Brig refuses all Write and Edit operations. This is the emergency-stop button when the admiral suspects something is off. Toggle with `shipmates freeze --reason=\"...\"` and `shipmates release`.",
	},
	{
		Number:    13,
		Handle:    "confirm-before-destroying",
		Title:     "Confirm Before Destroying",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "incident: personas running rm -rf on a project subdirectory during 'cleanup', occasionally rooted at $HOME when a variable expansion went wrong",
		Rationale: "Anything that irreversibly removes data must be ask-listed: rm -rf, rm -r, any SQL with DROP TABLE or TRUNCATE. The Brig does not blanket-deny these (personas legitimately clear build directories) — it forces the operator to approve every one.",
	},
	{
		Number:    14,
		Handle:    "no-self-escalation",
		Title:     "No Self-Escalation",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "theoretical: no reported live incident, but a persona that CAN loosen its own restrictions converges on doing so under sufficient prompt pressure",
		Rationale: "A persona may not rewrite its own policy overlay, the Claude Code settings, or the user-scope Brig overrides. Writes to .shipmates/policies/*, .claude/settings.json, and ~/.shipmates/brig.yaml are denied.",
	},
	{
		Number:    15,
		Handle:    "stay-aboard",
		Title:     "Stay Aboard",
		Category:  CategoryConduct,
		Layers:    []Layer{LayerKernel},
		Source:    "incident: personas 'helpfully' adding SSH config entries or touching global git config while working inside a project",
		Rationale: "Writes outside the project root are refused: /etc/**, ~/.ssh/**, ~/.aws/**. The persona works in the ship — the project directory — and nowhere else.",
	},
}

// startMarker and endMarker delimit the Brig's kernel-policy section inside
// a persona overlay. The composer keeps everything outside these markers
// untouched and rewrites only what lives between them.
const (
	startMarker = "# <!-- shipmates:brig:start -->"
	endMarker   = "# <!-- shipmates:brig:end -->"
)

// PromptStartMarker and PromptEndMarker delimit the Articles reminder block
// inside a persona's developer_instructions. Exported so the persona
// inventory composer can splice/replace the block idempotently.
const (
	PromptStartMarker = "<!-- shipmates:articles:start -->"
	PromptEndMarker   = "<!-- shipmates:articles:end -->"
)

// FreezeMarkerPath returns the on-disk location of the freeze marker inside
// a project's control directory.
func FreezeMarkerPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".shipmates", "freeze")
}

// DenialLogPath returns the on-disk location of the append-only JSONL denial log.
func DenialLogPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".shipmates", "brig.log")
}

// FreezeMarker is the on-disk shape of the freeze marker file.
type FreezeMarker struct {
	Reason    string    `json:"reason"`
	Admiral   string    `json:"admiral"`
	Timestamp time.Time `json:"timestamp"`
}

// CheckFreeze returns (true, marker) when the freeze marker exists and is a
// valid regular file. Returns (false, nil) when the file is absent or
// unreadable. Callers must treat any error at the read/parse stage as
// "freeze in effect, refuse writes" — the fail-closed default keeps the
// emergency stop conservative.
func CheckFreeze(projectRoot string) (bool, *FreezeMarker) {
	path := FreezeMarkerPath(projectRoot)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, nil
	}
	if !info.Mode().IsRegular() {
		return true, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return true, nil
	}
	var m FreezeMarker
	if err := json.Unmarshal(body, &m); err != nil {
		return true, nil
	}
	return true, &m
}

// SetFreeze writes the freeze marker file with the given reason and admiral,
// stamping the current time. Creates the .shipmates directory if missing.
// Overwrites an existing marker (re-freezing is a no-op semantic, but the
// new reason and timestamp replace the old ones).
func SetFreeze(projectRoot, reason, admiral string) error {
	if err := os.MkdirAll(filepath.Join(projectRoot, ".shipmates"), 0o755); err != nil {
		return fmt.Errorf("brig: create .shipmates: %w", err)
	}
	m := FreezeMarker{Reason: reason, Admiral: admiral, Timestamp: time.Now().UTC()}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("brig: marshal freeze marker: %w", err)
	}
	body = append(body, '\n')
	return os.WriteFile(FreezeMarkerPath(projectRoot), body, 0o600)
}

// ClearFreeze removes the freeze marker if present. Missing marker is not
// an error — release is idempotent.
func ClearFreeze(projectRoot string) error {
	err := os.Remove(FreezeMarkerPath(projectRoot))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LogDenial appends one JSONL entry to the project's brig.log, describing a
// kernel-layer refusal. Creates the file if missing. Best-effort: this must
// never fail the enforcement path — an error is returned so the caller can
// surface it, but the caller should proceed to refuse the command either way.
func LogDenial(projectRoot, persona string, rule int, command string) error {
	if err := os.MkdirAll(filepath.Join(projectRoot, ".shipmates"), 0o755); err != nil {
		return fmt.Errorf("brig: create .shipmates: %w", err)
	}
	entry := struct {
		Timestamp time.Time `json:"ts"`
		Persona   string    `json:"persona"`
		Rule      int       `json:"rule"`
		Command   string    `json:"command"`
	}{
		Timestamp: time.Now().UTC(),
		Persona:   persona,
		Rule:      rule,
		Command:   command,
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("brig: marshal denial: %w", err)
	}
	body = append(body, '\n')
	f, err := os.OpenFile(DenialLogPath(projectRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("brig: open denial log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("brig: append denial: %w", err)
	}
	return nil
}

// ReadDenials reads the project's brig.log and returns the parsed entries in
// file order. Returns an empty slice (no error) when the log is missing.
// Lines that don't parse are skipped silently — the log is append-only and
// old entries from a prior schema version should not stop the newer reader.
func ReadDenials(projectRoot string) ([]Denial, error) {
	body, err := os.ReadFile(DenialLogPath(projectRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Denial
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Denial
		if err := json.Unmarshal(line, &d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// Denial is one parsed entry from brig.log.
type Denial struct {
	Timestamp time.Time `json:"ts"`
	Persona   string    `json:"persona"`
	Rule      int       `json:"rule"`
	Command   string    `json:"command"`
}

// MergeInto is idempotent — it splices `template` (the Brig kernel policy
// template body) into the persona overlay at `personaPolicyPath`, wrapping it
// in shipmates-owned marker comments. Re-running MergeInto with the same
// template produces a byte-identical file; re-running with a changed template
// replaces the marker block verbatim and leaves everything outside it alone.
//
// If the overlay file doesn't exist yet, MergeInto creates one containing an
// empty strict schema plus the Brig block. If the overlay exists but the
// block is absent, the block is appended.
//
// The overlay's `allow`/`ask`/`deny` sections are not merged rule-by-rule —
// that would require re-serializing YAML and would collide with a user's
// hand-edits. Instead the Brig block sits as a top-of-file *comment block*
// (the template's rules are commented sample) and the operator can copy
// desired rules into the overlay by hand. This keeps MergeInto boring: no
// YAML round-trip, no rule renumbering, no user-edit clobbering.
//
// NOTE: because the shipmates policy loader only reads the overlay body
// (allow/ask/deny arrays), rules inside the commented Brig block are *not*
// active by default. This is intentional — MergeInto is a docs-and-defaults
// installer; the operator opts each rule into their persona by copying the
// template entries into their allow/ask/deny arrays, or by loading the
// template directly via the fleet baseline path.
func MergeInto(personaPolicyPath string, template []byte) error {
	block := renderBlock(template)

	existing, err := os.ReadFile(personaPolicyPath)
	if errors.Is(err, fs.ErrNotExist) {
		body := []byte("version: 1\nallow: []\nask: []\ndeny: []\n\n") // strict-schema-empty overlay
		body = append(body, block...)
		if err := os.MkdirAll(filepath.Dir(personaPolicyPath), 0o755); err != nil {
			return fmt.Errorf("brig: create policy dir: %w", err)
		}
		return os.WriteFile(personaPolicyPath, body, 0o600)
	}
	if err != nil {
		return fmt.Errorf("brig: read %s: %w", personaPolicyPath, err)
	}
	updated := spliceBlock(existing, block)
	// Skip the write when the file is already correct — MergeInto is
	// documented idempotent and callers may want to detect no-op runs by
	// mtime.
	if bytes.Equal(updated, existing) {
		return nil
	}
	return os.WriteFile(personaPolicyPath, updated, 0o600)
}

// renderBlock wraps the raw template bytes in the marker comments. The
// template is emitted as commented YAML so it does not participate in the
// active policy — see MergeInto's docstring for the rationale.
func renderBlock(template []byte) []byte {
	var b bytes.Buffer
	b.WriteString(startMarker + "\n")
	b.WriteString("# The Ship's Articles (see docs/brig.md and catalog/ARTICLES.md).\n")
	b.WriteString("# Rules in this section are DOCUMENTATION — copy the ones you want to\n")
	b.WriteString("# enforce for this persona into the allow/ask/deny arrays above. The\n")
	b.WriteString("# block is regenerated by `shipmates brig install` and is safe to leave\n")
	b.WriteString("# untouched.\n")
	b.WriteString("#\n")
	for _, line := range strings.Split(strings.TrimRight(string(template), "\n"), "\n") {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString(endMarker + "\n")
	return b.Bytes()
}

// spliceBlock replaces (or appends) the Brig-marked section in `overlay`.
func spliceBlock(overlay, block []byte) []byte {
	text := string(overlay)
	start := strings.Index(text, startMarker)
	end := strings.Index(text, endMarker)
	if start == -1 || end == -1 || end < start {
		// No block yet — append with a blank-line separator.
		out := bytes.TrimRight(overlay, "\n")
		out = append(out, '\n', '\n')
		out = append(out, block...)
		return out
	}
	// Consume trailing newline of the closing marker so re-runs are stable.
	tail := end + len(endMarker)
	if tail < len(text) && text[tail] == '\n' {
		tail++
	}
	// Trim any trailing whitespace before the start marker so we don't grow
	// blank lines on every re-run.
	head := start
	for head > 0 && (text[head-1] == ' ' || text[head-1] == '\t') {
		head--
	}
	var out bytes.Buffer
	out.WriteString(strings.TrimRight(text[:head], "\n"))
	out.WriteString("\n\n")
	out.Write(block)
	rest := strings.TrimLeft(text[tail:], "\n")
	if rest != "" {
		out.WriteString(rest)
		if !strings.HasSuffix(rest, "\n") {
			out.WriteString("\n")
		}
	}
	return out.Bytes()
}

// RulesForCategory returns the subset of Rules() belonging to the given
// category, preserving canonical order. Convenience for `brig list --code`
// and `brig list --conduct`.
func RulesForCategory(cat Category) []Rule {
	var out []Rule
	for _, r := range canonicalRules {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

// SortedHandles returns the rule handles in alphabetical order, used by the
// `brig` command's shell completions.
func SortedHandles() []string {
	out := make([]string, 0, len(canonicalRules))
	for _, r := range canonicalRules {
		out = append(out, r.Handle)
	}
	sort.Strings(out)
	return out
}
