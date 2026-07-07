package permissions

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// PersonaPolicy is the on-disk shape of catalog/<persona>/policy.yaml (vendored
// to .shipmates/policies/<persona>.yaml on install). Same YAML shape as the
// `permissions:` block in Claude Code's settings.json — allow/ask extend the
// project pools, deny unions with all other denies.
//
// Only allow/ask/deny are consumed; unknown top-level keys are ignored so we
// can grow the format without breaking older ships.
type PersonaPolicy struct {
	Allow []string `yaml:"allow"`
	Ask   []string `yaml:"ask"`
	Deny  []string `yaml:"deny"`
}

// LoadPersonaPolicy reads the persona overlay from projectRoot. It looks in two
// places, in order:
//
//  1. `<projectRoot>/.shipmates/policies/<persona>.yaml` — vendored by
//     `shipmates add <persona>` and edited in place by operators.
//  2. `<projectRoot>/catalog/<persona>/policy.yaml` — the catalog source, used
//     when the project IS the catalog (development / test harness).
//
// Returns (nil, nil) when neither file exists — a persona without an overlay
// contributes no rules. A malformed YAML file returns an error so callers can
// surface it; the evaluator treats that the same as "no overlay" and logs.
func LoadPersonaPolicy(projectRoot, persona string) (*PersonaPolicy, error) {
	if projectRoot == "" || persona == "" {
		return nil, nil
	}
	candidates := []string{
		filepath.Join(projectRoot, ".shipmates", "policies", persona+".yaml"),
		filepath.Join(projectRoot, "catalog", persona, "policy.yaml"),
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var pol PersonaPolicy
		if err := yaml.Unmarshal(b, &pol); err != nil {
			return nil, err
		}
		return &pol, nil
	}
	return nil, nil
}

// mergePersonaPolicy overlays a persona policy onto a MergedRules, returning a
// fresh MergedRules. The base is not mutated (so evaluators that share a base
// rule set across personas stay safe). Semantics:
//
//   - persona allow rules are appended (higher precedence than project allow
//     within the "allow" pool — but deny/ask elsewhere still wins).
//   - persona ask rules are appended.
//   - persona deny rules are UNIONed with the base deny; deny in any layer
//     is unconditional.
func mergePersonaPolicy(base MergedRules, pol *PersonaPolicy) MergedRules {
	if pol == nil {
		return base
	}
	out := MergedRules{
		Allow: append([]Rule(nil), base.Allow...),
		Ask:   append([]Rule(nil), base.Ask...),
		Deny:  append([]Rule(nil), base.Deny...),
	}
	seen := func(list []Rule, raw string) bool {
		for _, r := range list {
			if r.Raw == raw {
				return true
			}
		}
		return false
	}
	// Persona-specific rules go FIRST in each list. Match order is stable and
	// first-match-wins within a given effect list, so persona rules take
	// precedence over broader project rules for the same effect.
	prefixAllow := make([]Rule, 0, len(pol.Allow))
	for _, raw := range pol.Allow {
		if seen(out.Allow, raw) {
			continue
		}
		prefixAllow = append(prefixAllow, ParseRule(raw))
	}
	prefixAsk := make([]Rule, 0, len(pol.Ask))
	for _, raw := range pol.Ask {
		if seen(out.Ask, raw) {
			continue
		}
		prefixAsk = append(prefixAsk, ParseRule(raw))
	}
	out.Allow = append(prefixAllow, out.Allow...)
	out.Ask = append(prefixAsk, out.Ask...)
	// Deny: append (order doesn't matter — deny short-circuits anyway).
	for _, raw := range pol.Deny {
		if seen(out.Deny, raw) {
			continue
		}
		out.Deny = append(out.Deny, ParseRule(raw))
	}
	return out
}

// personaCache is a tiny memoizer for per-persona rule sets. Loading the same
// policy on every tool call is wasteful; the file rarely changes during a
// session, and Invalidate() drops the cache alongside settings.
type personaCache struct {
	mu    sync.RWMutex
	rules map[string]MergedRules
}

func newPersonaCache() *personaCache {
	return &personaCache{rules: map[string]MergedRules{}}
}

func (c *personaCache) get(persona string) (MergedRules, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.rules[persona]
	return r, ok
}

func (c *personaCache) set(persona string, r MergedRules) {
	c.mu.Lock()
	c.rules[persona] = r
	c.mu.Unlock()
}

func (c *personaCache) reset() {
	c.mu.Lock()
	c.rules = map[string]MergedRules{}
	c.mu.Unlock()
}
