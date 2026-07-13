package policy

import (
	"strings"
	"sync"
	"testing"
)

func source(body string) Source {
	return Source{Descriptor: SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml", Present: true}, Bytes: []byte(body)}
}
func completeSources(body string) []Source {
	return []Source{
		source(body),
		{Descriptor: SourceDescriptor{Layer: LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}},
		{Descriptor: SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/p.yaml", Present: true}, Bytes: []byte("version: 1\nallow: []\nask: []\ndeny: []\n")},
	}
}
func document(allow, ask, deny string) string {
	return "version: 1\nallow:\n" + allow + "ask:\n" + ask + "deny:\n" + deny
}
func rule(id, cmd string) string {
	return "  - id: " + id + "\n    kind: process.exec\n    match:\n      command_exact: \"" + cmd + "\"\n    reason: ok\n"
}
func mustParse(t *testing.T, sources ...Source) *Snapshot {
	t.Helper()
	has := map[Layer]bool{}
	for _, src := range sources {
		has[src.Descriptor.Layer] = true
	}
	if !has[LayerProject] {
		sources = append(sources, source("version: 1\nallow: []\nask: []\ndeny: []\n"))
	}
	if !has[LayerProjectLocal] {
		sources = append(sources, Source{Descriptor: SourceDescriptor{Layer: LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}})
	}
	if !has[LayerPersona] {
		sources = append(sources, Source{Descriptor: SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/backend.yaml", Present: true}, Bytes: []byte("version: 1\nallow: []\nask: []\ndeny: []\n")})
	}
	s, d := Parse("backend", "root-digest", sources)
	if s == nil {
		t.Fatalf("diagnostics: %+v", d)
	}
	return s
}

func TestStrictValidAndPrecedence(t *testing.T) {
	s := mustParse(t, source(document(rule("a", "git status"), rule("q", "git status"), rule("d", "git status"))))
	e := Evaluate(s, Request{Kind: ProcessExec, CommandExact: "git status"})
	if e.PolicyEffect != Deny || e.ReasonCode != "matched_deny" || len(e.MatchedRules) != 1 || e.MatchedRules[0].ID != "d" {
		t.Fatalf("explanation: %+v", e)
	}
	if got := Evaluate(s, Request{Kind: ProcessExec, CommandExact: "git  status"}); got.PolicyEffect != Ask || got.ReasonCode != "unmatched" {
		t.Fatalf("exact mismatch: %+v", got)
	}
}

func TestAskDominatesAllowAndMatchedOrdering(t *testing.T) {
	a := source(document(rule("z", "x"), rule("b", "x"), "  []\n"))
	b := Source{Descriptor: SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/backend.yaml", Present: true}, Bytes: []byte(document("  []\n", rule("a", "x"), "  []\n"))}
	s := mustParse(t, a, b)
	e := Evaluate(s, Request{Kind: ProcessExec, CommandExact: "x"})
	if e.PolicyEffect != Ask || len(e.MatchedRules) != 2 || e.MatchedRules[0].ID != "a" || e.MatchedRules[1].ID != "b" {
		t.Fatalf("%+v", e)
	}
}

func TestSemanticIdentityIgnoresFormattingAndRuleOrder(t *testing.T) {
	one := source(document(rule("a", "one")+rule("b", "two"), "  []\n", "  []\n"))
	two := source("# comment\ndeny: []\nask: []\nallow:\n" + rule("b", "two") + rule("a", "one") + "version: 1\n")
	a, b := mustParse(t, one), mustParse(t, two)
	if a.ID != b.ID {
		t.Fatalf("semantic IDs differ: %s %s", a.ID, b.ID)
	}
	changed := source(document(rule("a", "one")+strings.Replace(rule("b", "two"), "reason: ok", "reason: changed", 1), "  []\n", "  []\n"))
	if mustParse(t, changed).ID == a.ID {
		t.Fatal("reason did not affect ID")
	}
}

func TestLayersAreSetsAndCrossLayerMatcherAllowed(t *testing.T) {
	a := source(document(rule("a", "x"), "  []\n", "  []\n"))
	b := Source{Descriptor: SourceDescriptor{Layer: LayerPersona, Path: ".shipmates/policies/backend.yaml", Present: true}, Bytes: []byte(document("  []\n", "  []\n", rule("b", "x")))}
	s1 := mustParse(t, a, b)
	s2 := mustParse(t, b, a)
	if s1.ID != s2.ID {
		t.Fatal("source order affected identity")
	}
	if Evaluate(s1, Request{Kind: ProcessExec, CommandExact: "x"}).PolicyEffect != Deny {
		t.Fatal("deny did not dominate across layers")
	}
}

func TestStrictFailuresSanitized(t *testing.T) {
	tests := map[string]string{
		"duplicate_key":             "version: 1\nversion: 1\nallow: []\nask: []\ndeny: []\n",
		"missing_key":               "version: 1\nallow: []\nask: []\n",
		"unknown_key":               "version: 1\nallow: []\nask: []\ndeny: []\nsecret: CANARY\n",
		"unknown_version":           "version: 2\nallow: []\nask: []\ndeny: []\n",
		"invalid_type":              "version: \"1\"\nallow: []\nask: []\ndeny: []\n",
		"policy_multiple_documents": "version: 1\nallow: []\nask: []\ndeny: []\n---\nfoo: bar\n",
		"policy_invalid_yaml":       "version: [\n",
		"alias":                     "version: 1\nallow: &x []\nask: *x\ndeny: []\n",
		"tag":                       "version: 1\nallow: !thing []\nask: []\ndeny: []\n",
		"unknown_kind":              document(rule("a", "x"), "  []\n", "  []\n")[:],
	}
	tests["unknown_kind"] = strings.Replace(tests["unknown_kind"], "process.exec", "other", 1)
	for want, body := range tests {
		t.Run(want, func(t *testing.T) {
			s, ds := Parse("backend", "root", []Source{source(body)})
			if s != nil || len(ds) == 0 {
				t.Fatalf("snapshot=%v diagnostics=%+v", s, ds)
			}
			found := false
			for _, d := range ds {
				if d.Code == want || (want == "alias" || want == "tag") && d.Code == "policy_invalid_yaml" {
					found = true
				}
				if strings.Contains(d.Message, "CANARY") {
					t.Fatalf("leaked input: %+v", d)
				}
			}
			if !found {
				t.Fatalf("want %s: %+v", want, ds)
			}
		})
	}
}

func TestRuleValidationAndBounds(t *testing.T) {
	cases := []struct{ name, body, code string }{
		{"bad id", document(rule("Bad", "x"), "  []\n", "  []\n"), "invalid_rule_id"},
		{"control command", document(rule("a", "x\\u0000y"), "  []\n", "  []\n"), "invalid_match"},
		{"empty reason", strings.Replace(document(rule("a", "x"), "  []\n", "  []\n"), "reason: ok", "reason: \"\"", 1), "invalid_reason"},
		{"duplicate id", document(rule("a", "x")+rule("a", "y"), "  []\n", "  []\n"), "duplicate_rule_id"},
		{"duplicate matcher", document(rule("a", "x")+rule("b", "x"), "  []\n", "  []\n"), "duplicate_rule"},
		{"unknown rule key", strings.Replace(document(rule("a", "x"), "  []\n", "  []\n"), "    reason:", "    extra: no\n    reason:", 1), "unknown_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ds := Parse("p", "r", []Source{source(tc.body)})
			if s != nil {
				t.Fatal("invalid input accepted")
			}
			ok := false
			for _, d := range ds {
				if d.Code == tc.code {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("want %s: %+v", tc.code, ds)
			}
		})
	}
	tooMany := "version: 1\nallow:\n"
	for i := 0; i < 257; i++ {
		tooMany += rule("r"+string(rune('a'+i%26))+strings.Repeat("a", i/26), "x"+string(rune(i+32)))
	}
	tooMany += "ask: []\ndeny: []\n"
	if s, d := Parse("p", "r", []Source{source(tooMany)}); s != nil || len(d) == 0 {
		t.Fatal("per-effect bound accepted")
	}
}

func TestInvalidUTF8SizeAbsentAndUnsupported(t *testing.T) {
	bad := source("")
	bad.Bytes = []byte{0xff}
	if s, d := Parse("p", "r", []Source{bad}); s != nil || !hasCode(d, "policy_invalid_utf8") {
		t.Fatalf("%v %+v", s, d)
	}
	large := source("")
	large.Bytes = make([]byte, MaxSourceBytes+1)
	if s, d := Parse("p", "r", []Source{large}); s != nil || !hasCode(d, "policy_too_large") {
		t.Fatalf("%v %+v", s, d)
	}
	abs := Source{Descriptor: SourceDescriptor{Layer: LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}}
	s := mustParse(t, abs)
	if e := Evaluate(s, Request{Kind: "future.kind", CommandExact: "x"}); e.ReasonCode != "unsupported_request" || e.PolicyEffect != Ask {
		t.Fatalf("%+v", e)
	}
}

func hasCode(ds []Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestConcurrentEvaluation(t *testing.T) {
	s := mustParse(t, source(document(rule("a", "x"), "  []\n", "  []\n")))
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if Evaluate(s, Request{Kind: ProcessExec, CommandExact: "x"}).PolicyEffect != Allow {
					t.Error("unstable")
				}
			}
		}()
	}
	wg.Wait()
}

func FuzzParseNeverPanicsOrLeaks(f *testing.F) {
	f.Add([]byte("version: 1\nallow: []\nask: []\ndeny: []\n"))
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > MaxSourceBytes+1 {
			return
		}
		s, ds := Parse("p", "r", completeSources(string(b)))
		if s != nil && len(ds) > 0 {
			t.Fatal("partial snapshot")
		}
		if len(ds) > MaxDiagnostics {
			t.Fatal("unbounded diagnostics")
		}
	})
}

func FuzzExactEvaluation(f *testing.F) {
	f.Add("echo x", "echo x")
	f.Fuzz(func(t *testing.T, ruleCommand, request string) {
		if !validCommand(ruleCommand) || !validCommand(request) {
			return
		}
		body := document(rule("a", strings.ReplaceAll(strings.ReplaceAll(ruleCommand, "\\", "\\\\"), "\"", "\\\"")), "  []\n", "  []\n")
		s, d := Parse("p", "r", completeSources(body))
		if s == nil {
			return
		}
		e := Evaluate(s, Request{Kind: ProcessExec, CommandExact: request})
		if e.PolicyEffect == Allow && ruleCommand != request {
			t.Fatalf("non-exact allow; diagnostics=%v", d)
		}
	})
}
