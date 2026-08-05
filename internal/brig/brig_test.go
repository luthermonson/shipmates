package brig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulesInventory(t *testing.T) {
	rules := Rules()
	if len(rules) != 15 {
		t.Fatalf("Rules() len = %d, want 15", len(rules))
	}
	seen := map[string]bool{}
	for i, r := range rules {
		if r.Number != i+1 {
			t.Errorf("rule %d carries Number %d", i+1, r.Number)
		}
		if r.Handle == "" || r.Title == "" || r.Rationale == "" {
			t.Errorf("Article %d has empty metadata: %+v", r.Number, r)
		}
		if seen[r.Handle] {
			t.Errorf("duplicate handle %q", r.Handle)
		}
		seen[r.Handle] = true
		if len(r.Layers) == 0 {
			t.Errorf("Article %d declares no enforcement layer", r.Number)
		}
		wantCat := CategoryConduct
		if r.Number <= 5 {
			wantCat = CategoryCode
		}
		if r.Category != wantCat {
			t.Errorf("Article %d category = %s, want %s", r.Number, r.Category, wantCat)
		}
	}
	// The handles named by the operator guide and the config schema.
	for _, h := range []string{
		"owasp-top-10", "owasp-llm-top-10", "cwe-top-25", "nist-ssdf", "twelve-factor",
		"no-prod-db", "no-destructive-git", "no-secrets-in-commits", "verify-every-package",
		"no-piped-execution", "no-lies-about-failure", "respect-the-freeze",
		"confirm-before-destroying", "no-self-escalation", "stay-aboard",
	} {
		if _, ok := ByHandle(h); !ok {
			t.Errorf("ByHandle(%q) missing", h)
		}
	}
	// Rules() hands back a copy.
	rules[0].Handle = "mutated"
	if fresh := Rules(); fresh[0].Handle == "mutated" {
		t.Error("Rules() exposes the canonical slice to mutation")
	}
}

func TestGetBounds(t *testing.T) {
	if _, err := Get(0); err == nil {
		t.Error("Get(0) should error")
	}
	if _, err := Get(16); err == nil {
		t.Error("Get(16) should error")
	}
	r, err := Get(14)
	if err != nil || r.Handle != "no-self-escalation" {
		t.Errorf("Get(14) = %+v, %v", r, err)
	}
}

func TestRulesForCategory(t *testing.T) {
	if got := len(RulesForCategory(CategoryCode)); got != 5 {
		t.Errorf("code rules = %d, want 5", got)
	}
	if got := len(RulesForCategory(CategoryConduct)); got != 10 {
		t.Errorf("conduct rules = %d, want 10", got)
	}
}

func TestFreezeRoundTrip(t *testing.T) {
	root := t.TempDir()
	if frozen, _ := CheckFreeze(root); frozen {
		t.Fatal("fresh project reports frozen")
	}
	if err := SetFreeze(root, "suspicious diff", "luther"); err != nil {
		t.Fatal(err)
	}
	frozen, marker := CheckFreeze(root)
	if !frozen {
		t.Fatal("freeze marker written but CheckFreeze says not frozen")
	}
	if marker == nil || marker.Reason != "suspicious diff" || marker.Admiral != "luther" {
		t.Fatalf("marker = %+v", marker)
	}
	if marker.Timestamp.IsZero() {
		t.Error("marker timestamp not stamped")
	}
	if err := ClearFreeze(root); err != nil {
		t.Fatal(err)
	}
	if frozen, _ := CheckFreeze(root); frozen {
		t.Error("freeze survives ClearFreeze")
	}
	// Release is idempotent.
	if err := ClearFreeze(root); err != nil {
		t.Errorf("second ClearFreeze errored: %v", err)
	}
}

// TestFreezeFailsClosed: a marker that exists but cannot be parsed still
// counts as frozen — the emergency stop must not be defeatable by
// corrupting its own marker.
func TestFreezeFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FreezeMarkerPath(root), []byte("not json {"), 0o644); err != nil {
		t.Fatal(err)
	}
	frozen, marker := CheckFreeze(root)
	if !frozen {
		t.Fatal("garbage marker must still freeze (fail closed)")
	}
	if marker != nil {
		t.Errorf("garbage marker parsed to %+v", marker)
	}
	// A directory squatting on the marker path also freezes.
	root2 := t.TempDir()
	if err := os.MkdirAll(FreezeMarkerPath(root2), 0o755); err != nil {
		t.Fatal(err)
	}
	if frozen, _ := CheckFreeze(root2); !frozen {
		t.Error("non-regular marker must freeze (fail closed)")
	}
}

func TestDenialLogRoundTrip(t *testing.T) {
	root := t.TempDir()
	entries, err := ReadDenials(root)
	if err != nil || entries != nil {
		t.Fatalf("missing log => (%v, %v), want (nil, nil)", entries, err)
	}
	if err := LogDenial(root, "backend", 7, "git push --force"); err != nil {
		t.Fatal(err)
	}
	if err := LogDenial(root, "tester", 12, "Write main.go"); err != nil {
		t.Fatal(err)
	}
	entries, err = ReadDenials(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Persona != "backend" || entries[0].Rule != 7 || entries[0].Command != "git push --force" {
		t.Errorf("first entry = %+v", entries[0])
	}
	if entries[1].Rule != 12 {
		t.Errorf("second entry = %+v", entries[1])
	}
	// Garbage lines are skipped, not fatal — the log is append-only history.
	f, err := os.OpenFile(DenialLogPath(root), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	entries, err = ReadDenials(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("after garbage line: entries = %d (%v), want 2", len(entries), err)
	}
}

func TestDenialArticle(t *testing.T) {
	cases := []struct {
		reason string
		want   int
		ok     bool
	}{
		{"denied: matched brig: Article 7 (no-destructive-git) Bash(git push*--force*)", 7, true},
		{"ask: matched brig: Article 13 (confirm-before-destroying) Bash(rm -rf*)", 13, true},
		{"denied: matched Bash(rm *)", 0, false},
		{"fleet-deny: matched Bash(kubectl *)", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := DenialArticle(tc.reason)
		if got != tc.want || ok != tc.ok {
			t.Errorf("DenialArticle(%q) = (%d, %v), want (%d, %v)", tc.reason, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPromptBlock(t *testing.T) {
	block := PromptBlock(DefaultSettings())
	if !strings.HasPrefix(block, PromptStartMarker) || !strings.HasSuffix(block, PromptEndMarker) {
		t.Fatalf("block not marker-delimited:\n%s", block)
	}
	for _, want := range []string{"Ship's Articles", "ARTICLES.md", "No Destructive Git", "OWASP Top 10", ".shipmates/freeze"} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
	// A waived Article drops out of the block.
	s := FromConfig(configBrig(nil, []string{"twelve-factor", "respect-the-freeze"}))
	block = PromptBlock(s)
	if strings.Contains(block, "12-Factor") {
		t.Errorf("waived Article still listed:\n%s", block)
	}
	if strings.Contains(block, ".shipmates/freeze") {
		t.Errorf("freeze text remains with Article 12 waived:\n%s", block)
	}
	if !strings.Contains(block, "No Destructive Git") {
		t.Errorf("un-waived Article missing:\n%s", block)
	}
	// A disabled brig renders nothing.
	off := false
	if got := PromptBlock(FromConfig(configBrig(&off, nil))); got != "" {
		t.Errorf("disabled brig rendered a block:\n%s", got)
	}
}

func TestSplicePrompt(t *testing.T) {
	block := PromptBlock(DefaultSettings())
	body := "# Role\n\nDo the work.\n"

	once := SplicePrompt(body, block)
	if !strings.Contains(once, "# Role") || !strings.Contains(once, PromptStartMarker) {
		t.Fatalf("splice lost content:\n%s", once)
	}
	twice := SplicePrompt(once, block)
	if twice != once {
		t.Errorf("splice is not idempotent:\n--- once\n%q\n--- twice\n%q", once, twice)
	}
	if strings.Count(twice, PromptStartMarker) != 1 {
		t.Errorf("block stacked: %d markers", strings.Count(twice, PromptStartMarker))
	}

	// A changed block replaces the old one in place.
	s := FromConfig(configBrig(nil, []string{"twelve-factor"}))
	newBlock := PromptBlock(s)
	replaced := SplicePrompt(once, newBlock)
	if strings.Contains(replaced, "12-Factor") {
		t.Errorf("old block content survived replacement:\n%s", replaced)
	}
	if strings.Count(replaced, PromptStartMarker) != 1 {
		t.Errorf("replacement stacked blocks:\n%s", replaced)
	}

	// An empty block (brig disabled) removes an existing block.
	removed := SplicePrompt(once, "")
	if strings.Contains(removed, PromptStartMarker) {
		t.Errorf("empty block did not remove the section:\n%s", removed)
	}
	if !strings.Contains(removed, "# Role") {
		t.Errorf("removal ate the persona body:\n%s", removed)
	}
	// And removing from a body with no block is a no-op.
	if got := SplicePrompt(body, ""); got != body {
		t.Errorf("no-op removal changed the body: %q", got)
	}
}
