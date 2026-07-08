package permissions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Feature A: per-persona catalog policy overlay -------------------------

func TestPersonaPolicy_ExtendsProjectAllow(t *testing.T) {
	tmp := t.TempDir()
	writePolicy(t, tmp, "captain", `
allow:
  - Bash(shipmates-cap-test *)
`)
	e := NewEvaluator(tmp)
	// The project has NO allow for `shipmates-cap-test`, and it's not on the
	// read-only builtins list — without the overlay it would fall through
	// to ask. Using a synthetic command name so a user-level settings.json
	// on the test host can't accidentally allow it.
	d := e.EvaluateFor("captain", "Bash", bashInput("shipmates-cap-test alpha"))
	if d.Effect != EffectAllow {
		t.Fatalf("expected allow via persona overlay, got %v (%s)", d.Effect, d.Reason)
	}
	// A DIFFERENT persona must NOT inherit captain's overlay.
	d = e.EvaluateFor("tester", "Bash", bashInput("shipmates-cap-test alpha"))
	if d.Effect != EffectAsk {
		t.Errorf("tester should not inherit captain overlay, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestPersonaPolicy_DenyBeatsPersonaAllow(t *testing.T) {
	tmp := t.TempDir()
	// Project denies rm outright…
	mustWrite(t, filepath.Join(tmp, ".claude", "settings.json"), `{
  "permissions": { "deny": ["Bash(rm *)"] }
}`)
	// …persona overlay tries to broadly allow Bash.
	writePolicy(t, tmp, "captain", `
allow:
  - Bash
`)
	e := NewEvaluator(tmp)
	d := e.EvaluateFor("captain", "Bash", bashInput("rm foo"))
	if d.Effect != EffectDeny {
		t.Fatalf("deny in project must beat persona allow, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestPersonaPolicy_PersonaDenyUnions(t *testing.T) {
	tmp := t.TempDir()
	// Project broadly allows Bash…
	mustWrite(t, filepath.Join(tmp, ".claude", "settings.json"), `{
  "permissions": { "allow": ["Bash"] }
}`)
	// …persona denies a specific dangerous command.
	writePolicy(t, tmp, "backend", `
deny:
  - Bash(kubectl delete *)
`)
	e := NewEvaluator(tmp)
	d := e.EvaluateFor("backend", "Bash", bashInput("kubectl delete ns prod"))
	if d.Effect != EffectDeny {
		t.Fatalf("persona deny must apply, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestPersonaPolicy_MalformedYamlFallsBack(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, ".shipmates", "policies", "captain.yaml"),
		"this is: not:\n  - valid yaml [")
	e := NewEvaluator(tmp)
	// Should NOT panic; fall through to ask on a non-builtin. Use a
	// synthetic command name so a user-level settings.json on the test host
	// can't accidentally allow it and mask the fall-through.
	d := e.EvaluateFor("captain", "Bash", bashInput("shipmates-yaml-test alpha"))
	if d.Effect != EffectAsk {
		t.Errorf("bad yaml should degrade to base rules (ask), got %v (%s)", d.Effect, d.Reason)
	}
}

// --- Feature B: fleet-wide deny --------------------------------------------

func TestFleetPolicy_DenyBeatsEverything(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(
		[]string{"Bash"}, // ship allows everything
		nil, nil,
	))
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Bash(kubectl delete ns *)"}})

	d := e.EvaluateFor("backend", "Bash", bashInput("kubectl delete ns production"))
	if d.Effect != EffectDeny {
		t.Fatalf("fleet-deny must beat ship allow, got %v (%s)", d.Effect, d.Reason)
	}
	if !strings.Contains(d.Reason, "fleet-deny") {
		t.Errorf("reason should identify fleet-deny origin, got %q", d.Reason)
	}
}

func TestFleetPolicy_CleanFallThroughWhenNoMatch(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash(git *)"}, nil, nil))
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Bash(rm -rf /)"}})
	// Fleet policy doesn't match this — normal evaluation proceeds.
	d := e.EvaluateFor("captain", "Bash", bashInput("git status"))
	if d.Effect != EffectAllow {
		t.Errorf("no fleet match should not affect normal allow, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestFleetPolicy_NilClearsCache(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom([]string{"Bash"}, nil, nil))
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Bash(gh secret *)"}})
	// Confirm the fleet deny fires.
	d := e.EvaluateFor("captain", "Bash", bashInput("gh secret set foo"))
	if d.Effect != EffectDeny {
		t.Fatalf("expected deny before clear, got %v", d.Effect)
	}
	// Clear via nil.
	e.SetFleetPolicy(nil)
	d = e.EvaluateFor("captain", "Bash", bashInput("gh secret set foo"))
	if d.Effect != EffectAllow {
		t.Fatalf("expected allow after clear, got %v (%s)", d.Effect, d.Reason)
	}
}

// --- Feature C: time-boxed approvals ---------------------------------------

func TestTimeBox_ActiveUpgradesAskToAllow(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	// Without a time-box, this asks.
	if d := e.EvaluateFor("tester", "Bash", bashInput("pnpm test unit")); d.Effect != EffectAsk {
		t.Fatalf("precondition: expected ask, got %v", d.Effect)
	}
	e.RegisterTimeBox("tester", "Bash", "pnpm test unit", 30*time.Minute)
	d := e.EvaluateFor("tester", "Bash", bashInput("pnpm test unit"))
	if d.Effect != EffectAllow {
		t.Fatalf("expected allow via time-box, got %v (%s)", d.Effect, d.Reason)
	}
	if !strings.HasPrefix(d.Reason, "time-boxed until ") {
		t.Errorf("reason should identify time-box, got %q", d.Reason)
	}
}

func TestTimeBox_OnlyMatchesExactCommand(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	e.RegisterTimeBox("tester", "Bash", "pnpm test unit", 30*time.Minute)
	// Different command must still ask.
	d := e.EvaluateFor("tester", "Bash", bashInput("pnpm test integration"))
	if d.Effect != EffectAsk {
		t.Errorf("time-box should be exact-match, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestTimeBox_ScopedPerPersona(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	e.RegisterTimeBox("backend", "Bash", "pnpm test unit", 30*time.Minute)
	// A different persona must not benefit from backend's grant.
	d := e.EvaluateFor("tester", "Bash", bashInput("pnpm test unit"))
	if d.Effect != EffectAsk {
		t.Errorf("time-box must be per-persona, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestTimeBox_ExpiredFallsThrough(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	e.RegisterTimeBox("tester", "Bash", "pnpm test unit", 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	d := e.EvaluateFor("tester", "Bash", bashInput("pnpm test unit"))
	if d.Effect != EffectAsk {
		t.Errorf("expired time-box should fall through to ask, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestTimeBox_NeverOverridesDeny(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, []string{"Bash(rm *)"}))
	// Even a very generous time-box for `rm foo` cannot bypass the deny.
	e.RegisterTimeBox("tester", "Bash", "rm foo", time.Hour)
	d := e.EvaluateFor("tester", "Bash", bashInput("rm foo"))
	if d.Effect != EffectDeny {
		t.Fatalf("deny must beat time-box, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestTimeBox_WhitespaceNormalized(t *testing.T) {
	e := NewEvaluatorWithRules(rulesFrom(nil, nil, nil))
	e.RegisterTimeBox("tester", "Bash", "pnpm test unit", time.Minute)
	// Same command with different whitespace should still hit.
	d := e.EvaluateFor("tester", "Bash", bashInput("pnpm  test\tunit"))
	if d.Effect != EffectAllow {
		t.Errorf("time-box match should be whitespace-normalized, got %v (%s)", d.Effect, d.Reason)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePolicy(t *testing.T, projectRoot, persona, yaml string) {
	t.Helper()
	mustWrite(t, filepath.Join(projectRoot, ".shipmates", "policies", persona+".yaml"), yaml)
}
