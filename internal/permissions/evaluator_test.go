package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// rulesFrom is a small helper: build MergedRules from three arrays of raw
// rule strings, so tests read like the settings.json snippets they mirror.
func rulesFrom(allow, ask, deny []string) MergedRules {
	var r MergedRules
	for _, a := range allow {
		r.Allow = append(r.Allow, ParseRule(a))
	}
	for _, a := range ask {
		r.Ask = append(r.Ask, ParseRule(a))
	}
	for _, d := range deny {
		r.Deny = append(r.Deny, ParseRule(d))
	}
	return r
}

func bashInput(cmd string) map[string]any {
	return map[string]any{"command": cmd}
}

func pathInput(p string) map[string]any {
	return map[string]any{"file_path": p}
}

func webInput(u string) map[string]any {
	return map[string]any{"url": u}
}

func TestEvaluate_BashAllowMatchesGitStar(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash(git *)"}, nil, nil))
	d := e.Evaluate("Bash", bashInput("git status"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashDenyBeatsAllow(t *testing.T) {
	// A broad Bash allow with a targeted deny — deny must win.
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"Bash"},          // allow all Bash
		nil,
		[]string{"Bash(rm *)"},    // but never rm
	))
	d := e.Evaluate("Bash", bashInput("rm foo"))
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (rm), got %v (%s)", d.Effect, d.Reason)
	}
	// Non-rm still allowed.
	d = e.Evaluate("Bash", bashInput("echo hello"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (echo), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashAskBeatsAllow(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"Bash"},
		[]string{"Bash(git push*)"},
		nil,
	))
	d := e.Evaluate("Bash", bashInput("git push origin main"))
	if d.Effect != EffectAsk {
		t.Errorf("expected ask, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashReadOnlyBuiltinAutoAllow(t *testing.T) {
	// No rules — a plain `ls` should still allow because it's a read-only
	// builtin.
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	d := e.Evaluate("Bash", bashInput("ls -la src/"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (builtin), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashReadOnlyBuiltinRespectsExplicitDeny(t *testing.T) {
	// The user explicitly denied `ls *` — the builtin allowlist must not
	// override that.
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, []string{"Bash(ls *)"}))
	d := e.Evaluate("Bash", bashInput("ls -la"))
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (explicit), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashNoRuleFallsThroughToAsk(t *testing.T) {
	// A non-builtin, unmatched command should ask.
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	d := e.Evaluate("Bash", bashInput("npm install"))
	if d.Effect != EffectAsk {
		t.Errorf("expected ask (fallthrough), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashWrapperStripped(t *testing.T) {
	// `timeout 30 npm test` should match `Bash(npm test)`.
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash(npm test)"}, nil, nil))
	d := e.Evaluate("Bash", bashInput("timeout 30 npm test"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (wrapper stripped), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashCompoundRequiresAllToPass(t *testing.T) {
	// A compound where one subcommand is allowed (git status) and the
	// other is denied (rm) — whole thing must deny.
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"Bash"},
		nil,
		[]string{"Bash(rm *)"},
	))
	d := e.Evaluate("Bash", bashInput("git status && rm -rf tmp"))
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (compound), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashCompoundAllAllowed(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash(git *)", "Bash(npm *)"}, nil, nil))
	d := e.Evaluate("Bash", bashInput("git status && npm test"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (all match), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_BashCompoundOneAsksOthersAllow(t *testing.T) {
	// One subcommand triggers ask, the rest allow — worst case wins.
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"Bash(git *)", "Bash(npm *)"},
		[]string{"Bash(npm publish*)"},
		nil,
	))
	d := e.Evaluate("Bash", bashInput("git status && npm publish"))
	if d.Effect != EffectAsk {
		t.Errorf("expected ask, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_PathMatch(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, []string{"Read(**/.env)"}))
	d := e.Evaluate("Read", pathInput("apps/web/.env"))
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (.env), got %v (%s)", d.Effect, d.Reason)
	}
	d = e.Evaluate("Read", pathInput("apps/web/README.md"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (README), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_DomainMatch(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"WebFetch(domain:*.github.com)"},
		nil,
		[]string{"WebFetch(domain:evil.example.com)"},
	))
	d := e.Evaluate("WebFetch", webInput("https://api.github.com/repos"))
	if d.Effect != EffectAllow {
		t.Errorf("expected allow (github), got %v (%s)", d.Effect, d.Reason)
	}
	d = e.Evaluate("WebFetch", webInput("https://evil.example.com/x"))
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (evil), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_UnknownToolDefaultsAllow(t *testing.T) {
	// Non-shell, non-file, non-web tools default to allow (shipmates'
	// original behavior).
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	d := e.Evaluate("SomeCustomTool", map[string]any{"foo": "bar"})
	if d.Effect != EffectAllow {
		t.Errorf("expected default-allow, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestEvaluate_UnknownToolBareRule(t *testing.T) {
	// A bare `SomeCustomTool` deny should still catch it.
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, []string{"SomeCustomTool"}))
	d := e.Evaluate("SomeCustomTool", map[string]any{"foo": "bar"})
	if d.Effect != EffectDeny {
		t.Errorf("expected deny (bare rule), got %v (%s)", d.Effect, d.Reason)
	}
}

func TestLoadSettings_MergesThreeSources(t *testing.T) {
	// Build a fake project with a project settings.json and settings.local.json.
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON := func(p string, s Settings) {
		b, _ := json.Marshal(s)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(tmp, ".claude", "settings.json"), Settings{
		Permissions: &PermissionsSection{
			Allow: []string{"Bash(git *)"},
			Deny:  []string{"Bash(rm -rf *)"},
		},
	})
	writeJSON(filepath.Join(tmp, ".claude", "settings.local.json"), Settings{
		Permissions: &PermissionsSection{
			Allow: []string{"Bash(pnpm *)"},
			Ask:   []string{"Bash(git push*)"},
		},
	})
	// LoadSettings walks user ~ too; we don't control it, but we can at
	// least verify our two files landed.
	rules, errs := LoadSettings(tmp)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// Both allows present.
	found := map[string]bool{}
	for _, r := range rules.Allow {
		found[r.Raw] = true
	}
	if !found["Bash(git *)"] || !found["Bash(pnpm *)"] {
		t.Errorf("expected both allow rules, got %v", rules.Allow)
	}
	// Ask from local.
	foundAsk := false
	for _, r := range rules.Ask {
		if r.Raw == "Bash(git push*)" {
			foundAsk = true
		}
	}
	if !foundAsk {
		t.Errorf("expected ask rule, got %v", rules.Ask)
	}
	// Deny from project.
	foundDeny := false
	for _, r := range rules.Deny {
		if r.Raw == "Bash(rm -rf *)" {
			foundDeny = true
		}
	}
	if !foundDeny {
		t.Errorf("expected deny rule, got %v", rules.Deny)
	}
}

func TestEvaluate_ExplicitPersonaCarveout(t *testing.T) {
	// The reason string should identify which rule matched — the UI
	// depends on this for its "auto-allowed because …" display.
	// Use `git checkout` (not on the read-only builtin list) so we exercise
	// the settings-rule match rather than the builtin auto-allow.
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash(git *)"}, nil, nil))
	d := e.Evaluate("Bash", bashInput("git checkout main"))
	if d.Effect != EffectAllow {
		t.Fatalf("expected allow, got %v", d.Effect)
	}
	if d.Reason == "" || !contains(d.Reason, "Bash(git *)") {
		t.Errorf("reason should name the matching rule, got %q", d.Reason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && stringIndex(s, sub) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
