package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rules(allow, ask, deny []string) MergedRules {
	var m MergedRules
	for _, r := range allow {
		m.Allow = append(m.Allow, ParseRule(r))
	}
	for _, r := range ask {
		m.Ask = append(m.Ask, ParseRule(r))
	}
	for _, r := range deny {
		m.Deny = append(m.Deny, ParseRule(r))
	}
	return m
}

// ---------------------------------------------------------------- evaluateBash

func TestEvaluate_BashEmptyCommandAsks(t *testing.T) {
	e := NewEvaluatorWithRules(rules([]string{"Bash"}, nil, nil))
	for _, in := range []map[string]any{
		{"command": ""},
		{"command": "   "},
		{},
		{"command": 42}, // wrong type — not a string
		nil,
	} {
		d := e.Evaluate("Bash", in)
		// A bare `Bash` allow rule must NOT rubber-stamp a malformed call.
		if d.Effect != EffectAsk {
			t.Fatalf("Evaluate(Bash, %v) = %s (%s), want ask", in, d.Effect, d.Reason)
		}
	}
}

func TestEvaluate_PowerShellUsesBashPath(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, nil, []string{"PowerShell(Remove-Item *)"}))
	d := e.Evaluate("PowerShell", map[string]any{"command": "Remove-Item -Recurse C:/"})
	if d.Effect != EffectDeny {
		t.Fatalf("PowerShell deny = %s (%s), want deny", d.Effect, d.Reason)
	}
}

// ---------------------------------------------------------------- evaluatePath

func TestEvaluate_PathAlternateKey(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, nil, []string{"Read(**/.env)"}))
	// Some tools report the target under "path" instead of "file_path".
	d := e.Evaluate("Read", map[string]any{"path": "config/.env"})
	if d.Effect != EffectDeny {
		t.Fatalf("Read{path:.env} = %s (%s), want deny", d.Effect, d.Reason)
	}
}

func TestEvaluate_PathAskBeatsAllow(t *testing.T) {
	e := NewEvaluatorWithRules(rules(
		[]string{"Edit(src/**)"},
		[]string{"Edit(src/secrets/**)"},
		nil,
	))
	d := e.Evaluate("Edit", map[string]any{"file_path": "src/secrets/keys.go"})
	if d.Effect != EffectAsk {
		t.Fatalf("= %s (%s), want ask (ask beats allow)", d.Effect, d.Reason)
	}
	if d := e.Evaluate("Edit", map[string]any{"file_path": "src/main.go"}); d.Effect != EffectAllow {
		t.Fatalf("= %s (%s), want allow", d.Effect, d.Reason)
	}
}

func TestEvaluate_PathBareToolRuleMatchesAnyPath(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, nil, []string{"Write"}))
	d := e.Evaluate("Write", map[string]any{"file_path": "anything/at/all.txt"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny from the bare Write rule", d.Effect, d.Reason)
	}
}

func TestEvaluate_PathWildcardToolRule(t *testing.T) {
	// `*(...)` — a rule whose tool is the wildcard applies to every path tool.
	e := NewEvaluatorWithRules(rules(nil, nil, []string{"*(**/.env)"}))
	for _, tool := range []string{"Read", "Edit", "Write", "MultiEdit", "NotebookEdit"} {
		d := e.Evaluate(tool, map[string]any{"file_path": "deep/nested/.env"})
		if d.Effect != EffectDeny {
			t.Fatalf("%s = %s (%s), want deny", tool, d.Effect, d.Reason)
		}
	}
}

// ----------------------------------------------------------------- evaluateWeb

func TestEvaluate_WebAskAndDefaults(t *testing.T) {
	e := NewEvaluatorWithRules(rules(
		[]string{"WebFetch(domain:*.github.com)"},
		[]string{"WebFetch(domain:*.internal.corp)"},
		[]string{"WebFetch(domain:evil.example)"},
	))
	tests := []struct {
		url  string
		want Effect
	}{
		{"https://api.github.com/repos", EffectAllow},
		{"https://wiki.internal.corp/page", EffectAsk},
		{"https://evil.example/pwn", EffectDeny},
		{"https://unlisted.example/x", EffectAllow}, // web default is allow
	}
	for _, tt := range tests {
		d := e.Evaluate("WebFetch", map[string]any{"url": tt.url})
		if d.Effect != tt.want {
			t.Errorf("WebFetch(%s) = %s (%s), want %s", tt.url, d.Effect, d.Reason, tt.want)
		}
	}
}

func TestEvaluate_WebSearchHasNoURL(t *testing.T) {
	// WebSearch carries a query, not a url. A bare `WebSearch` deny still
	// applies (empty pattern), which is the only rule shape that can gate it.
	e := NewEvaluatorWithRules(rules(nil, nil, []string{"WebSearch"}))
	d := e.Evaluate("WebSearch", map[string]any{"query": "how to rm -rf"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny", d.Effect, d.Reason)
	}
}

// -------------------------------------------------------- matchToolNameRules

func TestEvaluate_UnknownToolDenyBeatsAskBeatsAllow(t *testing.T) {
	tests := []struct {
		name  string
		rules MergedRules
		want  Effect
	}{
		{"deny wins", rules([]string{"Task"}, []string{"Task"}, []string{"Task"}), EffectDeny},
		{"ask beats allow", rules([]string{"Task"}, []string{"Task"}, nil), EffectAsk},
		{"allow", rules([]string{"Task"}, nil, nil), EffectAllow},
		{"nothing matches -> default allow", rules([]string{"Bash"}, nil, nil), EffectAllow},
		// A patterned rule can't be evaluated against an unknown input shape,
		// so it must be skipped rather than guessed at.
		{"patterned rule is ignored", rules(nil, nil, []string{"Task(dangerous *)"}), EffectAllow},
		{"wildcard tool deny applies", rules(nil, nil, []string{"*"}), EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewEvaluatorWithRules(tt.rules)
			d := e.Evaluate("Task", map[string]any{"prompt": "x"})
			if d.Effect != tt.want {
				t.Fatalf("= %s (%s), want %s", d.Effect, d.Reason, tt.want)
			}
		})
	}
}

// ------------------------------------------------------------- matchFleetDeny

func TestFleetDeny_PathTool(t *testing.T) {
	e := NewEvaluatorWithRules(rules([]string{"Read"}, nil, nil)) // ship says allow all reads
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Read(**/.ssh/**)"}})

	d := e.EvaluateFor("captain", "Read", map[string]any{"file_path": "home/user/.ssh/id_ed25519"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny — a ship allow must not shadow a fleet deny", d.Effect, d.Reason)
	}
	if !strings.HasPrefix(d.Reason, "fleet-deny: ") {
		t.Fatalf("reason = %q, want a fleet-deny: prefix", d.Reason)
	}
	// The "denied: " label from the inner matcher must be stripped, not stacked.
	if strings.Contains(d.Reason, "denied: ") {
		t.Fatalf("reason = %q, want the inner denied: label trimmed", d.Reason)
	}

	if d := e.EvaluateFor("captain", "Read", map[string]any{"file_path": "src/main.go"}); d.Effect != EffectAllow {
		t.Fatalf("unrelated read = %s (%s), want allow", d.Effect, d.Reason)
	}
}

func TestFleetDeny_PathToolAlternateKey(t *testing.T) {
	e := NewEvaluatorWithRules(MergedRules{})
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Write(**/.ssh/**)"}})
	d := e.EvaluateFor("captain", "Write", map[string]any{"path": "home/user/.ssh/authorized_keys"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny via the alternate path key", d.Effect, d.Reason)
	}
}

func TestFleetDeny_WebTool(t *testing.T) {
	e := NewEvaluatorWithRules(rules([]string{"WebFetch"}, nil, nil))
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"WebFetch(domain:*.exfil.example)"}})

	d := e.EvaluateFor("captain", "WebFetch", map[string]any{"url": "https://drop.exfil.example/upload"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny", d.Effect, d.Reason)
	}
	if !strings.HasPrefix(d.Reason, "fleet-deny: ") {
		t.Fatalf("reason = %q, want a fleet-deny: prefix", d.Reason)
	}
	if d := e.EvaluateFor("captain", "WebFetch", map[string]any{"url": "https://github.com/x"}); d.Effect != EffectAllow {
		t.Fatalf("unrelated fetch = %s (%s), want allow", d.Effect, d.Reason)
	}
}

func TestFleetDeny_UnknownToolBareRule(t *testing.T) {
	e := NewEvaluatorWithRules(MergedRules{})
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Task"}})

	d := e.EvaluateFor("captain", "Task", map[string]any{"prompt": "spawn"})
	if d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny (default-allow must not beat a fleet deny)", d.Effect, d.Reason)
	}
	if !strings.Contains(d.Reason, "fleet-deny") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestFleetDeny_UnknownToolPatternedRuleDoesNotMatch(t *testing.T) {
	e := NewEvaluatorWithRules(MergedRules{})
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Task(something *)"}})
	if d := e.EvaluateFor("captain", "Task", map[string]any{"prompt": "x"}); d.Effect != EffectAllow {
		t.Fatalf("= %s (%s), want allow — a patterned rule has no input shape to match on", d.Effect, d.Reason)
	}
}

func TestFleetDeny_AppliesWithoutAPersona(t *testing.T) {
	// Legacy Evaluate (no persona) must still be covered by the fleet layer.
	e := NewEvaluatorWithRules(rules([]string{"Bash(rm *)"}, nil, nil))
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Bash(rm -rf /*)"}})
	if d := e.Evaluate("Bash", map[string]any{"command": "rm -rf /"}); d.Effect != EffectDeny {
		t.Fatalf("= %s (%s), want deny", d.Effect, d.Reason)
	}
}

func TestSetFleetPolicyOnEvaluatorWithoutSlotIsSafe(t *testing.T) {
	var e Evaluator // zero value: no fleet slot
	e.SetFleetPolicy(&FleetPolicy{Deny: []string{"Bash"}})
	// Must not panic, and must not pretend the policy took effect.
	if e.fleet != nil {
		t.Fatal("SetFleetPolicy fabricated a fleet slot on a zero Evaluator")
	}
}

func TestTrimLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"denied: matched Bash(rm *)", "matched Bash(rm *)"},
		{"denied matched Bash(rm *)", "matched Bash(rm *)"},
		{"allowed: matched Bash(ls *)", "allowed: matched Bash(ls *)"},
		{"", ""},
	}
	for _, c := range cases {
		if got := trimLabel(c.in); got != c.want {
			t.Errorf("trimLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- commandFor

func TestCommandFor(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "pnpm test"}, "pnpm test"},
		{"PowerShell", map[string]any{"command": "Get-ChildItem"}, "Get-ChildItem"},
		{"Bash", map[string]any{"command": 7}, ""}, // wrong type
		{"Read", map[string]any{"file_path": "a/b.txt"}, "a/b.txt"},
		{"Write", map[string]any{"path": "a/b.txt"}, "a/b.txt"},   // alternate key
		{"Edit", map[string]any{"other": "a/b.txt"}, ""},          // neither key
		{"WebFetch", map[string]any{"url": "https://x/y"}, "https://x/y"},
		{"WebSearch", map[string]any{"query": "q"}, ""}, // no key for search
		{"Task", map[string]any{"prompt": "p"}, ""},
		{"Bash", nil, ""},
	}
	for _, c := range cases {
		if got := commandFor(c.tool, c.input); got != c.want {
			t.Errorf("commandFor(%s, %v) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

// ------------------------------------------------------------------ time-box

func TestTimeBox_PathToolGrant(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, []string{"Read(secrets/**)"}, nil))
	if d := e.EvaluateFor("captain", "Read", map[string]any{"file_path": "secrets/a.txt"}); d.Effect != EffectAsk {
		t.Fatalf("baseline = %s, want ask", d.Effect)
	}
	e.RegisterTimeBox("captain", "Read", "secrets/a.txt", time.Minute)
	d := e.EvaluateFor("captain", "Read", map[string]any{"file_path": "secrets/a.txt"})
	if d.Effect != EffectAllow {
		t.Fatalf("= %s (%s), want allow from the time-box", d.Effect, d.Reason)
	}
	// A sibling file was never approved.
	if d := e.EvaluateFor("captain", "Read", map[string]any{"file_path": "secrets/b.txt"}); d.Effect != EffectAsk {
		t.Fatalf("sibling = %s (%s), want ask", d.Effect, d.Reason)
	}
}

func TestTimeBox_NotConsultedWithoutPersona(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, []string{"Bash(pnpm *)"}, nil))
	e.RegisterTimeBox("", "Bash", "pnpm test", time.Minute)
	// The persona-less overload must never pick up a grant; the keyring is
	// per-persona by design.
	if d := e.Evaluate("Bash", map[string]any{"command": "pnpm test"}); d.Effect != EffectAsk {
		t.Fatalf("= %s (%s), want ask", d.Effect, d.Reason)
	}
}

func TestRegisterTimeBoxCreatesStoreOnDemand(t *testing.T) {
	e := &Evaluator{loaded: true} // zero-ish evaluator, no store
	tb := e.RegisterTimeBox("captain", "Bash", "pnpm test", time.Minute)
	if tb.Pattern != "pnpm test" || tb.Tool != "Bash" {
		t.Fatalf("TimeBox = %+v", tb)
	}
	if e.timeboxes == nil {
		t.Fatal("RegisterTimeBox did not create the store")
	}
	if _, ok := e.timeboxes.lookup("captain", "Bash", "pnpm  test"); !ok {
		t.Fatal("grant not retrievable after registration")
	}
}

func TestSweepTimeBoxes(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, []string{"Bash(pnpm *)"}, nil))
	e.RegisterTimeBox("captain", "Bash", "pnpm test", -time.Minute) // already expired
	e.RegisterTimeBox("captain", "Bash", "pnpm build", time.Hour)   // still live
	e.RegisterTimeBox("scout", "Bash", "pnpm lint", -time.Minute)   // expired, only entry

	e.SweepTimeBoxes()

	if _, ok := e.timeboxes.entries["captain"]["Bash\x00pnpm test"]; ok {
		t.Error("expired captain grant survived the sweep")
	}
	if _, ok := e.timeboxes.entries["captain"]["Bash\x00pnpm build"]; !ok {
		t.Error("live captain grant was swept")
	}
	// A persona whose last grant expired should not linger as an empty map.
	if _, ok := e.timeboxes.entries["scout"]; ok {
		t.Error("scout kept an empty entry map after its only grant expired")
	}
}

func TestSweepTimeBoxesOnEvaluatorWithoutStoreIsSafe(t *testing.T) {
	var e Evaluator
	e.SweepTimeBoxes() // must not panic
}

// ---------------------------------------------------------------- Invalidate

func TestInvalidateForcesReload(t *testing.T) {
	root := t.TempDir()
	isolateHome(t)
	writeJSON(t, filepath.Join(root, ".claude", "settings.json"), Settings{
		Permissions: &PermissionsSection{Allow: []string{"Bash(git *)"}},
	})

	e := NewEvaluator(root)
	if d := e.Evaluate("Bash", map[string]any{"command": "git status"}); d.Effect != EffectAllow {
		t.Fatalf("first load = %s (%s), want allow", d.Effect, d.Reason)
	}

	// Change the rule on disk. Without Invalidate the cached rules stand.
	writeJSON(t, filepath.Join(root, ".claude", "settings.json"), Settings{
		Permissions: &PermissionsSection{Deny: []string{"Bash(git *)"}},
	})
	if d := e.Evaluate("Bash", map[string]any{"command": "git status"}); d.Effect != EffectAllow {
		t.Fatalf("cached = %s (%s), want the cached allow (settings are not watched)", d.Effect, d.Reason)
	}

	e.Invalidate()
	if d := e.Evaluate("Bash", map[string]any{"command": "git status"}); d.Effect != EffectDeny {
		t.Fatalf("after Invalidate = %s (%s), want deny from the new file", d.Effect, d.Reason)
	}
}

func TestInvalidateDropsPersonaCache(t *testing.T) {
	root := t.TempDir()
	isolateHome(t)
	polPath := filepath.Join(root, ".shipmates", "policies", "captain.yaml")
	writeFile(t, polPath, "allow:\n  - Bash(pnpm *)\n")

	e := NewEvaluator(root)
	if d := e.EvaluateFor("captain", "Bash", map[string]any{"command": "pnpm test"}); d.Effect != EffectAllow {
		t.Fatalf("first = %s (%s), want allow", d.Effect, d.Reason)
	}

	writeFile(t, polPath, "deny:\n  - Bash(pnpm *)\n")
	if d := e.EvaluateFor("captain", "Bash", map[string]any{"command": "pnpm test"}); d.Effect != EffectAllow {
		t.Fatalf("cached = %s, want the memoized allow", d.Effect)
	}

	e.Invalidate()
	if d := e.EvaluateFor("captain", "Bash", map[string]any{"command": "pnpm test"}); d.Effect != EffectDeny {
		t.Fatalf("after Invalidate = %s (%s), want deny — the persona cache was not reset", d.Effect, d.Reason)
	}
}

func TestInvalidateOnEvaluatorWithoutPersonaCacheIsSafe(t *testing.T) {
	var e Evaluator
	e.Invalidate() // must not panic
	if e.loaded {
		t.Fatal("Invalidate left loaded = true")
	}
}

// ---------------------------------------------------------- persona policies

func TestLoadPersonaPolicyCatalogFallback(t *testing.T) {
	root := t.TempDir()
	// No .shipmates/policies entry — the catalog source is the second candidate.
	writeFile(t, filepath.Join(root, "catalog", "captain", "policy.yaml"), "deny:\n  - Bash(rm *)\n")

	pol, err := LoadPersonaPolicy(root, "captain")
	if err != nil {
		t.Fatal(err)
	}
	if pol == nil || len(pol.Deny) != 1 || pol.Deny[0] != "Bash(rm *)" {
		t.Fatalf("pol = %+v, want the catalog policy", pol)
	}
}

func TestLoadPersonaPolicyVendoredWinsOverCatalog(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".shipmates", "policies", "captain.yaml"), "allow:\n  - Bash(vendored *)\n")
	writeFile(t, filepath.Join(root, "catalog", "captain", "policy.yaml"), "allow:\n  - Bash(catalog *)\n")

	pol, err := LoadPersonaPolicy(root, "captain")
	if err != nil {
		t.Fatal(err)
	}
	if len(pol.Allow) != 1 || pol.Allow[0] != "Bash(vendored *)" {
		t.Fatalf("pol = %+v, want the operator-edited vendored copy to win", pol)
	}
}

func TestLoadPersonaPolicyEmptyArgs(t *testing.T) {
	for _, c := range []struct{ root, persona string }{{"", "captain"}, {"/x", ""}, {"", ""}} {
		pol, err := LoadPersonaPolicy(c.root, c.persona)
		if err != nil || pol != nil {
			t.Fatalf("LoadPersonaPolicy(%q,%q) = %+v, %v; want nil, nil", c.root, c.persona, pol, err)
		}
	}
}

func TestLoadPersonaPolicyMissingIsNotAnError(t *testing.T) {
	pol, err := LoadPersonaPolicy(t.TempDir(), "nobody")
	if err != nil {
		t.Fatalf("missing overlay returned an error: %v", err)
	}
	if pol != nil {
		t.Fatalf("pol = %+v, want nil", pol)
	}
}

func TestMergePersonaPolicyDoesNotMutateBase(t *testing.T) {
	base := rules([]string{"Bash(git *)"}, []string{"Bash(npm *)"}, []string{"Bash(rm *)"})
	merged := mergePersonaPolicy(base, &PersonaPolicy{
		Allow: []string{"Bash(pnpm *)", "Bash(git *)"}, // second is a dupe of base
		Ask:   []string{"Bash(curl *)"},
		Deny:  []string{"Bash(dd *)", "Bash(rm *)"}, // second is a dupe of base
	})
	if len(base.Allow) != 1 || len(base.Ask) != 1 || len(base.Deny) != 1 {
		t.Fatalf("base was mutated: %+v", base)
	}
	if len(merged.Allow) != 2 || merged.Allow[0].Raw != "Bash(pnpm *)" {
		t.Fatalf("merged.Allow = %+v, want the persona rule first and no dupe", merged.Allow)
	}
	if len(merged.Ask) != 2 || merged.Ask[0].Raw != "Bash(curl *)" {
		t.Fatalf("merged.Ask = %+v", merged.Ask)
	}
	if len(merged.Deny) != 2 {
		t.Fatalf("merged.Deny = %+v, want the dupe collapsed", merged.Deny)
	}
}

func TestMergePersonaPolicyNilReturnsBase(t *testing.T) {
	base := rules([]string{"Bash(git *)"}, nil, nil)
	if got := mergePersonaPolicy(base, nil); len(got.Allow) != 1 || got.Allow[0].Raw != "Bash(git *)" {
		t.Fatalf("got %+v, want the base unchanged", got)
	}
}

// ------------------------------------------------------------------ settings

func TestLoadSettingsReportsMalformedFileButKeepsWorking(t *testing.T) {
	root := t.TempDir()
	isolateHome(t)
	writeJSON(t, filepath.Join(root, ".claude", "settings.json"), Settings{
		Permissions: &PermissionsSection{Deny: []string{"Bash(rm *)"}},
	})
	writeFile(t, filepath.Join(root, ".claude", "settings.local.json"), "{ not json")

	merged, errs := LoadSettings(root)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly the settings.local.json parse error", errs)
	}
	// The good file must still take effect — a broken local override cannot be
	// allowed to wedge the gate open.
	if len(merged.Deny) != 1 || merged.Deny[0].Raw != "Bash(rm *)" {
		t.Fatalf("merged.Deny = %+v, want the valid file's rule", merged.Deny)
	}
}

func TestLoadSettingsMissingFilesAreNotErrors(t *testing.T) {
	isolateHome(t)
	merged, errs := LoadSettings(t.TempDir())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(merged.Allow)+len(merged.Ask)+len(merged.Deny) != 0 {
		t.Fatalf("merged = %+v, want empty", merged)
	}
}

func TestSettingsPathsWithEmptyRoot(t *testing.T) {
	if got := projectSettingsPath(""); got != "" {
		t.Errorf("projectSettingsPath(\"\") = %q, want empty", got)
	}
	if got := projectLocalSettingsPath(""); got != "" {
		t.Errorf("projectLocalSettingsPath(\"\") = %q, want empty", got)
	}
	root := filepath.FromSlash("/proj")
	if got := projectSettingsPath(root); got != filepath.Join(root, ".claude", "settings.json") {
		t.Errorf("projectSettingsPath = %q", got)
	}
	if got := projectLocalSettingsPath(root); got != filepath.Join(root, ".claude", "settings.local.json") {
		t.Errorf("projectLocalSettingsPath = %q", got)
	}
}

func TestLoadSettingsWithEmptyRootStillReadsUserSettings(t *testing.T) {
	home := isolateHome(t)
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), Settings{
		Permissions: &PermissionsSection{Deny: []string{"Bash(shutdown *)"}},
	})
	merged, errs := LoadSettings("")
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(merged.Deny) != 1 || merged.Deny[0].Raw != "Bash(shutdown *)" {
		t.Fatalf("merged.Deny = %+v, want the user-level rule", merged.Deny)
	}
}

func TestMergeSettingsPrecedence(t *testing.T) {
	user := Settings{Permissions: &PermissionsSection{
		Allow: []string{"Bash(user *)", "Bash(shared *)"},
		Deny:  []string{"Bash(user-deny *)"},
	}}
	project := Settings{Permissions: &PermissionsSection{
		Allow: []string{"Bash(project *)", "Bash(shared *)"},
		Ask:   []string{"Bash(ask-me *)"},
		Deny:  []string{"Bash(project-deny *)"},
	}}
	local := Settings{Permissions: &PermissionsSection{
		Allow: []string{"Bash(local *)"},
	}}
	empty := Settings{} // nil Permissions must be skipped, not panic

	got := mergeSettings([]Settings{user, project, local, empty})

	// Highest precedence first, deduped by raw text.
	wantAllow := []string{"Bash(local *)", "Bash(project *)", "Bash(shared *)", "Bash(user *)"}
	if len(got.Allow) != len(wantAllow) {
		t.Fatalf("Allow = %+v, want %v", got.Allow, wantAllow)
	}
	for i, w := range wantAllow {
		if got.Allow[i].Raw != w {
			t.Fatalf("Allow = %+v, want %v", got.Allow, wantAllow)
		}
	}
	// Deny unions in source order.
	wantDeny := []string{"Bash(user-deny *)", "Bash(project-deny *)"}
	for i, w := range wantDeny {
		if got.Deny[i].Raw != w {
			t.Fatalf("Deny = %+v, want %v", got.Deny, wantDeny)
		}
	}
	if len(got.Ask) != 1 || got.Ask[0].Raw != "Bash(ask-me *)" {
		t.Fatalf("Ask = %+v", got.Ask)
	}
}

func TestReadSettingsFileWithoutPermissionsKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, p, `{"model":"opus","env":{"A":"b"}}`)
	s, err := readSettings(p)
	if err != nil {
		t.Fatalf("readSettings: %v", err)
	}
	if s.Permissions != nil {
		t.Fatalf("Permissions = %+v, want nil for a file with no permissions key", s.Permissions)
	}
}

func TestUserSettingsPathWithoutAHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	// No home is survivable — the user layer is simply skipped rather than
	// producing a path like "/.claude/settings.json".
	if got := userSettingsPath(); got != "" {
		t.Fatalf("userSettingsPath() = %q, want empty when no home is resolvable", got)
	}
	if _, errs := LoadSettings(t.TempDir()); len(errs) != 0 {
		t.Fatalf("LoadSettings errs = %v, want none", errs)
	}
}

// ------------------------------------------------------- rule tool scoping

func TestRulesDoNotLeakAcrossTools(t *testing.T) {
	e := NewEvaluatorWithRules(rules(nil, nil, []string{
		"Read(secrets/**)",
		"WebFetch(domain:evil.example)",
	}))
	// A Read deny must not gate an Edit of the same path...
	if d := e.Evaluate("Edit", map[string]any{"file_path": "secrets/a.txt"}); d.Effect != EffectAllow {
		t.Fatalf("Edit = %s (%s), want allow — a Read rule must not gate Edit", d.Effect, d.Reason)
	}
	// ...and a WebFetch deny must not gate WebSearch.
	if d := e.Evaluate("WebSearch", map[string]any{"url": "https://evil.example"}); d.Effect != EffectAllow {
		t.Fatalf("WebSearch = %s (%s), want allow", d.Effect, d.Reason)
	}
	// The rules still bite their own tools.
	if d := e.Evaluate("Read", map[string]any{"file_path": "secrets/a.txt"}); d.Effect != EffectDeny {
		t.Fatalf("Read = %s (%s), want deny", d.Effect, d.Reason)
	}
}

func TestJoinedReasonsForMixedCompound(t *testing.T) {
	e := NewEvaluatorWithRules(rules([]string{"Bash(frobnicate *)"}, nil, nil))
	d := e.Evaluate("Bash", map[string]any{"command": "frobnicate --now && zork --hard"})
	if d.Effect != EffectAsk {
		t.Fatalf("= %s (%s), want ask", d.Effect, d.Reason)
	}
	// Differing sub-reasons are joined so the operator sees the whole trail,
	// not just the first one.
	if !strings.Contains(d.Reason, " ; ") {
		t.Fatalf("reason = %q, want the differing sub-reasons joined", d.Reason)
	}
	if !strings.Contains(d.Reason, "frobnicate *") || !strings.Contains(d.Reason, "zork") {
		t.Fatalf("reason = %q, want both subcommands represented", d.Reason)
	}
}

func TestIdenticalCompoundReasonsCollapse(t *testing.T) {
	e := NewEvaluatorWithRules(rules([]string{"Bash(frobnicate *)"}, nil, nil))
	d := e.Evaluate("Bash", map[string]any{"command": "frobnicate one && frobnicate two"})
	if d.Effect != EffectAllow {
		t.Fatalf("= %s (%s), want allow", d.Effect, d.Reason)
	}
	// Both subcommands matched the same rule, so the operator sees one reason,
	// not the same sentence twice.
	if strings.Contains(d.Reason, " ; ") {
		t.Fatalf("reason = %q, want the identical reasons collapsed to one", d.Reason)
	}
	if d.Reason != "allowed: matched Bash(frobnicate *)" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

func TestMatchDomainNormalisation(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"domain:github.com", "HTTPS://GitHub.com/org/repo", true}, // case + scheme + path
		{"domain:github.com", "github.com?a=b", true},
		{"domain:github.com", "github.com#frag", true},
		{"*.github.com", "github.com", true}, // bare parent matches
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com.evil.example", false},
		{"domain:github.com", "notgithub.com", false},
		{"", "anything", true}, // empty pattern matches all
	}
	for _, c := range cases {
		if got := MatchDomain(c.pattern, c.host); got != c.want {
			t.Errorf("MatchDomain(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

// ------------------------------------------------------------------- helpers

// isolateHome points os.UserHomeDir at a scratch dir so LoadSettings can't
// pick up the developer's real ~/.claude/settings.json. Both env vars are set
// because UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b))
}
