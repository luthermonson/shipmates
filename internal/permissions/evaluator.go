package permissions

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Effect is the tri-state outcome of an evaluation. It maps 1:1 to Claude
// Code's hookSpecificOutput.permissionDecision values, so the server can
// hand it back to the CLI verbatim.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectAsk   Effect = "ask"
	EffectDeny  Effect = "deny"
)

// Decision is what Evaluate hands back to the caller: the effect and a
// human-readable reason ("allowed: matched Bash(git *)"). The reason surfaces
// in shipmates' event stream so the operator can see WHY a call was
// auto-decided.
type Decision struct {
	Effect Effect
	Reason string
}

// Evaluator is the entry point. Callers construct one per project (or per
// persona-session) and reuse it; the settings load happens once and rules
// are cached until Invalidate is called.
//
// Layers (highest precedence deny wins across all of them):
//
//   1. Fleet-wide deny (set via SetFleetPolicy; sourced from Fleet Command).
//   2. Per-persona catalog overlay (catalog/<persona>/policy.yaml, vendored
//      to .shipmates/policies/<persona>.yaml on install).
//   3. Project settings (.claude/settings[.local].json) — Claude Code's own
//      rules; loaded once on first use.
//   4. User settings (~/.claude/settings.json).
//   5. Time-box grants — auto-allow that the operator opted into for N
//      minutes via `shipmates allow <id> --for 30m`. Consulted before ask.
//
// Deny in ANY layer beats everything else. Ask beats allow. Time-boxes fire
// at the "no rule matched, would ask" stage — they upgrade the ask to allow
// without ever waking the human.
//
// The zero value is not ready — use NewEvaluator.
type Evaluator struct {
	projectRoot string

	mu    sync.RWMutex
	rules MergedRules
	errs  []error
	// loaded gates the one-time load. We don't watch the files for
	// changes; captains are short-lived enough that a fresh evaluator
	// gets built on the next boot.
	loaded bool

	// fleet holds the fleet-wide deny list pushed by SetFleetPolicy. Nil
	// pointer would work but the slot keeps the lock semantics obvious.
	fleet *fleetSlot

	// brig holds the Brig's compiled kernel rules (internal/brig), applied
	// to every persona ahead of persona overlays and project settings.
	// Installed once by SetBrigPolicy at server construction.
	brig *brigSlot

	// personaRules memoizes the per-persona overlay merged onto the base
	// rules. Reset by Invalidate alongside the base settings.
	personaRules *personaCache

	// timeboxes holds active `shipmates allow <id> --for <duration>` grants,
	// keyed by persona. Consulted before falling through to ask. Storage is
	// in-memory only; a ship restart wipes all time-boxes.
	timeboxes *timeboxStore
}

// NewEvaluator builds an Evaluator bound to projectRoot. Settings are
// lazily loaded on the first Evaluate call so tests can construct evaluators
// without a filesystem dance.
func NewEvaluator(projectRoot string) *Evaluator {
	return &Evaluator{
		projectRoot:  projectRoot,
		fleet:        newFleetSlot(),
		brig:         newBrigSlot(),
		personaRules: newPersonaCache(),
		timeboxes:    newTimeboxStore(),
	}
}

// NewEvaluatorWithRules is a test seam — supply pre-parsed rules directly.
func NewEvaluatorWithRules(rules MergedRules) *Evaluator {
	return &Evaluator{
		rules:        rules,
		loaded:       true,
		fleet:        newFleetSlot(),
		brig:         newBrigSlot(),
		personaRules: newPersonaCache(),
		timeboxes:    newTimeboxStore(),
	}
}

// Invalidate clears the cached rules so the next Evaluate reloads settings.
// Not currently wired to a fs watcher — exists so operators (or a future
// SIGHUP handler) can force a refresh without restarting the ship.
func (e *Evaluator) Invalidate() {
	e.mu.Lock()
	e.loaded = false
	e.rules = MergedRules{}
	e.errs = nil
	e.mu.Unlock()
	if e.personaRules != nil {
		e.personaRules.reset()
	}
}

// SetFleetPolicy installs (or replaces) the fleet-wide deny list. Called by
// the ship's fleet-policy fetch loop on connect and on every poll. Passing
// nil clears the cache — but only do that when the operator explicitly wants
// to (a fleet fetch failure should keep the last-known policy, not drop it).
func (e *Evaluator) SetFleetPolicy(pol *FleetPolicy) {
	if e.fleet == nil {
		return
	}
	e.fleet.set(pol)
}

// SetBrigPolicy installs (or replaces) the Brig's compiled kernel rules —
// the Ship's Articles subset the operator has in force. The rules apply to
// EVERY persona this evaluator serves, ahead of persona overlays and
// project settings for the same effect: a Brig deny cannot be shadowed by
// a persona allow (deny beats everything) and a Brig ask cannot be skipped
// by one either (ask beats allow). Passing empty slices clears the policy —
// that is what `brig.enabled: false` resolves to.
func (e *Evaluator) SetBrigPolicy(ask, deny []Rule) {
	if e.brig == nil {
		return
	}
	e.brig.set(ask, deny)
}

// FleetDeny consults ONLY the fleet-wide deny list for a tool call. It is
// the seam the server's bypass-mode path uses: Article 14 (No
// Self-Escalation) requires the fleet layer to bind even personas running
// with dangerouslySkipPermissions, while bypass still skips every ship-side
// layer (Brig, persona overlay, project settings).
func (e *Evaluator) FleetDeny(tool string, input map[string]any) (Decision, bool) {
	if e.fleet == nil {
		return Decision{}, false
	}
	return matchFleetDeny(tool, input, e.fleet.snapshot())
}

// RegisterTimeBox records a "allow this exact command for the next duration"
// grant, tied to (persona, tool, command). Called from server handleResolve
// when the operator approved with a --for flag. Returns the created TimeBox
// so the caller can emit an event with the expiry time.
func (e *Evaluator) RegisterTimeBox(persona, tool, command string, duration time.Duration) TimeBox {
	if e.timeboxes == nil {
		e.timeboxes = newTimeboxStore()
	}
	return e.timeboxes.add(persona, tool, command, duration)
}

// SweepTimeBoxes drops expired grants across all personas. Optional hygiene
// call for a background ticker — evaluator lookups already self-expire.
func (e *Evaluator) SweepTimeBoxes() {
	if e.timeboxes != nil {
		e.timeboxes.sweep()
	}
}

// ensureLoaded loads the merged settings on first use. Load errors are
// stashed but not returned — a broken settings file falls back to "no
// rules", which reverts to the read-only-builtin allowlist plus ask for
// everything else. Same failure mode as vanilla Claude Code.
func (e *Evaluator) ensureLoaded() {
	e.mu.RLock()
	if e.loaded {
		e.mu.RUnlock()
		return
	}
	e.mu.RUnlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loaded {
		return
	}
	rules, errs := LoadSettings(e.projectRoot)
	e.rules = rules
	e.errs = errs
	e.loaded = true
}

// Evaluate is the top-level decision function. It applies Claude Code's
// documented precedence:
//
//  1. Deny in ANY layer beats everything.
//  2. Read-only Bash builtins are auto-allowed UNLESS an ask/deny rule
//     from settings matches them explicitly.
//  3. Compound Bash commands (a && b || c) are decomposed and every
//     subcommand must independently pass.
//  4. Ask rules force a prompt.
//  5. Allow rules skip the prompt.
//  6. Anything not matched anywhere falls through to Ask — the safe
//     default when no rule speaks to a tool call.
//
// tool is the Claude Code tool name ("Bash", "Read", "Edit", "WebFetch",
// …). input is the tool_input map from the hook payload.
//
// This overload does NOT layer the per-persona catalog policy or check the
// time-box store; use EvaluateFor(persona, …) for those. Kept for legacy
// callers that don't know which persona is asking (e.g. broadcast paths).
func (e *Evaluator) Evaluate(tool string, input map[string]any) Decision {
	return e.EvaluateFor("", tool, input)
}

// EvaluateFor is Evaluate scoped to a specific persona. The persona name
// selects the catalog policy overlay and the time-box keyring. An empty
// persona is equivalent to Evaluate — no overlay, no time-box.
func (e *Evaluator) EvaluateFor(persona, tool string, input map[string]any) Decision {
	e.ensureLoaded()
	rules := e.rulesFor(persona)

	// The Brig's kernel rules overlay every persona. Prepended so a Brig
	// rule's reason string (which names the Article) wins over a same-effect
	// project rule matching the same command.
	if e.brig != nil {
		rules = e.brig.overlay(rules)
	}

	// Fleet-wide deny is checked BEFORE anything else so nothing on the ship
	// can shadow it. A deny match here short-circuits the whole evaluation.
	if e.fleet != nil {
		if d, ok := matchFleetDeny(tool, input, e.fleet.snapshot()); ok {
			return d
		}
	}

	var d Decision
	switch tool {
	case "Bash", "PowerShell":
		d = evaluateBash(tool, input, rules)
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		d = evaluatePath(tool, input, rules)
	case "WebFetch", "WebSearch":
		d = evaluateWeb(tool, input, rules)
	default:
		// Non-shell, non-file, non-web tools: check for a bare tool-name
		// rule (e.g. `Bash` allows all Bash — same shape for others). If
		// nothing matches, default-allow: shipmates' original behavior
		// was to auto-allow anything that wasn't Bash/PowerShell, and
		// silently changing that would surprise users.
		if match, ok := matchToolNameRules(tool, "", rules); ok {
			d = match
		} else {
			d = Decision{Effect: EffectAllow, Reason: "default-allow: non-gated tool " + tool}
		}
	}

	// Time-box grants intercept an "ask" verdict and upgrade it to allow if
	// the operator recently approved this exact command with --for <dur>.
	// They never override a deny — deny beats everything, always.
	if d.Effect == EffectAsk && persona != "" && e.timeboxes != nil {
		if tb, ok := e.timeboxes.lookup(persona, tool, commandFor(tool, input)); ok {
			return Decision{
				Effect: EffectAllow,
				Reason: fmt.Sprintf("time-boxed until %s", tb.ExpiresAt.Format(time.RFC3339)),
			}
		}
	}
	return d
}

// rulesFor returns the effective rule set for a persona: the base project
// settings merged with the persona's catalog overlay (if any). Cached per
// persona so repeated tool calls don't re-parse the YAML.
func (e *Evaluator) rulesFor(persona string) MergedRules {
	e.mu.RLock()
	base := e.rules
	e.mu.RUnlock()
	if persona == "" || e.personaRules == nil {
		return base
	}
	if cached, ok := e.personaRules.get(persona); ok {
		return cached
	}
	pol, err := LoadPersonaPolicy(e.projectRoot, persona)
	if err != nil {
		// Malformed YAML: log and fall back to the base rules. Failing
		// closed here would silently break every tool call for the
		// persona, which is a worse UX than surfacing the parse error
		// and continuing with the (still-strict) project rules.
		slog.Warn("persona policy load failed", "persona", persona, "err", err)
		e.personaRules.set(persona, base)
		return base
	}
	merged := mergePersonaPolicy(base, pol)
	e.personaRules.set(persona, merged)
	return merged
}

// commandFor pulls the summary string a time-box key is built from, for a
// given tool call. Mirrors summarizeToolInput in the server package but is
// self-contained so the evaluator doesn't reach into server internals.
func commandFor(tool string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch tool {
	case "Bash", "PowerShell":
		if s, ok := input["command"].(string); ok {
			return s
		}
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		if s, ok := input["file_path"].(string); ok {
			return s
		}
		if s, ok := input["path"].(string); ok {
			return s
		}
	case "WebFetch":
		if s, ok := input["url"].(string); ok {
			return s
		}
	}
	return ""
}

// matchFleetDeny walks the fleet-wide deny rules against a tool call and
// returns a Decision if any match. Uses the same per-tool matcher shape as
// the ship-side path (bash/path/domain) so a rule like Bash(kubectl *) works
// identically whether it came from the fleet or the project settings.
func matchFleetDeny(tool string, input map[string]any, rules []Rule) (Decision, bool) {
	if len(rules) == 0 {
		return Decision{}, false
	}
	fake := MergedRules{Deny: rules}
	switch tool {
	case "Bash", "PowerShell":
		d := evaluateBash(tool, input, fake)
		if d.Effect == EffectDeny {
			return Decision{Effect: EffectDeny, Reason: "fleet-deny: " + trimLabel(d.Reason)}, true
		}
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		path, _ := input["file_path"].(string)
		if path == "" {
			if p, ok := input["path"].(string); ok {
				path = p
			}
		}
		if d, ok := firstPathMatch(tool, path, rules, EffectDeny, "denied"); ok {
			return Decision{Effect: EffectDeny, Reason: "fleet-deny: " + trimLabel(d.Reason)}, true
		}
	case "WebFetch", "WebSearch":
		host, _ := input["url"].(string)
		if d, ok := firstDomainMatch(tool, host, rules, EffectDeny, "denied"); ok {
			return Decision{Effect: EffectDeny, Reason: "fleet-deny: " + trimLabel(d.Reason)}, true
		}
	default:
		for _, r := range rules {
			if toolMatches(r.Tool, tool) && r.Pattern == "" {
				return Decision{Effect: EffectDeny, Reason: "fleet-deny: matched " + r.Raw}, true
			}
		}
	}
	return Decision{}, false
}

// trimLabel strips the "denied: " prefix the per-tool matchers add so we can
// re-prefix as "fleet-deny: " for reason-string clarity.
func trimLabel(reason string) string {
	for _, p := range []string{"denied: ", "denied "} {
		if strings.HasPrefix(reason, p) {
			return reason[len(p):]
		}
	}
	return reason
}

// evaluateBash handles Bash and PowerShell tool calls. It splits compound
// commands, strips wrappers on each subcommand, evaluates independently,
// and combines — the compound is denied if ANY subcommand is denied, asks
// if any asks, otherwise allow.
func evaluateBash(tool string, input map[string]any, rules MergedRules) Decision {
	cmd, _ := input["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		// A Bash call with no command is nonsense — ask the human.
		return Decision{Effect: EffectAsk, Reason: "empty command"}
	}
	subs := SplitCompound(cmd)
	if len(subs) == 0 {
		subs = []string{cmd}
	}
	var (
		worst    = EffectAllow
		reasons  []string
	)
	for _, sub := range subs {
		d := evaluateBashSingle(tool, sub, rules)
		reasons = append(reasons, d.Reason)
		if d.Effect == EffectDeny {
			// Deny short-circuits — no need to evaluate remaining subs.
			return Decision{Effect: EffectDeny, Reason: d.Reason}
		}
		if d.Effect == EffectAsk && worst == EffectAllow {
			worst = EffectAsk
		}
	}
	return Decision{
		Effect: worst,
		Reason: joinReasons(reasons),
	}
}

// evaluateBashSingle handles ONE non-compound subcommand.
func evaluateBashSingle(tool, sub string, rules MergedRules) Decision {
	prepped := prepareBashCommand(sub)

	// Deny always wins. Check against both the raw and prepped forms so a
	// deny rule like `Bash(rm *)` still catches `timeout 5 rm foo`.
	if d, ok := firstBashMatch(tool, prepped, sub, rules.Deny, EffectDeny, "denied"); ok {
		return d
	}
	// Ask beats allow (documented Claude Code precedence).
	if d, ok := firstBashMatch(tool, prepped, sub, rules.Ask, EffectAsk, "ask"); ok {
		return d
	}
	// Read-only builtins are allowed WITHOUT consulting the settings allow
	// list, but AFTER ask/deny so a settings-level ask/deny can still gate
	// them if the user really wants that.
	if ok, why := isReadOnlyBuiltin(prepped); ok {
		return Decision{Effect: EffectAllow, Reason: "allowed: " + why}
	}
	if d, ok := firstBashMatch(tool, prepped, sub, rules.Allow, EffectAllow, "allowed"); ok {
		return d
	}
	// Fall through: no rule spoke to this command. Ask.
	return Decision{Effect: EffectAsk, Reason: "no matching rule: " + sub}
}

// firstBashMatch walks a rule list for the given tool and returns the first
// matching rule's decision. Rules for a different tool are skipped. A bare
// `Bash` rule (empty pattern) matches anything.
func firstBashMatch(tool, prepped, raw string, rules []Rule, effect Effect, label string) (Decision, bool) {
	for _, r := range rules {
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if r.Pattern == "" {
			return Decision{Effect: effect, Reason: fmt.Sprintf("%s: matched %s", label, r.Raw)}, true
		}
		if MatchBash(r.Pattern, prepped) || MatchBash(r.Pattern, raw) {
			return Decision{Effect: effect, Reason: fmt.Sprintf("%s: matched %s", label, r.Raw)}, true
		}
	}
	return Decision{}, false
}

// evaluatePath handles Read/Edit/Write and friends. Path matching uses the
// gitignore-style matcher. No wrapper/compound handling — file tools take
// a single path per invocation.
func evaluatePath(tool string, input map[string]any, rules MergedRules) Decision {
	path, _ := input["file_path"].(string)
	if path == "" {
		// Try alternate keys some tools use.
		if p, ok := input["path"].(string); ok {
			path = p
		}
	}
	if d, ok := firstPathMatch(tool, path, rules.Deny, EffectDeny, "denied"); ok {
		return d
	}
	if d, ok := firstPathMatch(tool, path, rules.Ask, EffectAsk, "ask"); ok {
		return d
	}
	if d, ok := firstPathMatch(tool, path, rules.Allow, EffectAllow, "allowed"); ok {
		return d
	}
	// File-tool default is allow: shipmates' original behavior was to
	// auto-allow anything that wasn't Bash/PowerShell. Keeping that as
	// the fallback so we're strictly less noisy, not more.
	return Decision{Effect: EffectAllow, Reason: "default-allow: no matching rule for " + tool}
}

func firstPathMatch(tool, path string, rules []Rule, effect Effect, label string) (Decision, bool) {
	for _, r := range rules {
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if r.Pattern == "" || MatchPath(r.Pattern, path) {
			return Decision{Effect: effect, Reason: fmt.Sprintf("%s: matched %s", label, r.Raw)}, true
		}
	}
	return Decision{}, false
}

// evaluateWeb handles WebFetch/WebSearch. WebFetch has a `url`; WebSearch
// has a `query` which we don't attempt to match (no rule syntax for it).
func evaluateWeb(tool string, input map[string]any, rules MergedRules) Decision {
	host, _ := input["url"].(string)
	if d, ok := firstDomainMatch(tool, host, rules.Deny, EffectDeny, "denied"); ok {
		return d
	}
	if d, ok := firstDomainMatch(tool, host, rules.Ask, EffectAsk, "ask"); ok {
		return d
	}
	if d, ok := firstDomainMatch(tool, host, rules.Allow, EffectAllow, "allowed"); ok {
		return d
	}
	return Decision{Effect: EffectAllow, Reason: "default-allow: no matching rule for " + tool}
}

func firstDomainMatch(tool, url string, rules []Rule, effect Effect, label string) (Decision, bool) {
	for _, r := range rules {
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if r.Pattern == "" || MatchDomain(r.Pattern, url) {
			return Decision{Effect: effect, Reason: fmt.Sprintf("%s: matched %s", label, r.Raw)}, true
		}
	}
	return Decision{}, false
}

// matchToolNameRules handles the fallback path for tools we don't have a
// specialized matcher for. It only fires on bare `ToolName` rules; anything
// with a pattern is skipped (we can't match against an unknown input shape).
func matchToolNameRules(tool, _ string, rules MergedRules) (Decision, bool) {
	for _, r := range rules.Deny {
		if toolMatches(r.Tool, tool) && r.Pattern == "" {
			return Decision{Effect: EffectDeny, Reason: "denied: matched " + r.Raw}, true
		}
	}
	for _, r := range rules.Ask {
		if toolMatches(r.Tool, tool) && r.Pattern == "" {
			return Decision{Effect: EffectAsk, Reason: "ask: matched " + r.Raw}, true
		}
	}
	for _, r := range rules.Allow {
		if toolMatches(r.Tool, tool) && r.Pattern == "" {
			return Decision{Effect: EffectAllow, Reason: "allowed: matched " + r.Raw}, true
		}
	}
	return Decision{}, false
}

// toolMatches handles the wildcard rule tool name `*` (matches any tool)
// and case-sensitive exact matches otherwise.
func toolMatches(ruleTool, actual string) bool {
	if ruleTool == "*" || ruleTool == "" {
		return true
	}
	return ruleTool == actual
}

// joinReasons combines the reason strings for compound subcommands. If they
// all agree on one reason we return that; otherwise we join with " ; " so
// the operator sees the full trail.
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	if len(reasons) == 1 {
		return reasons[0]
	}
	all := reasons[0]
	same := true
	for _, r := range reasons[1:] {
		if r != all {
			same = false
			break
		}
	}
	if same {
		return all
	}
	return strings.Join(reasons, " ; ")
}
