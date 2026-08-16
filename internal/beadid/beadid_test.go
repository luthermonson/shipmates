package beadid

import (
	"strings"
	"testing"
)

// TestValidAcceptsRealIDs: everything bd actually mints must keep working.
// This is the half of the guard that a tightening change can silently break.
func TestValidAcceptsRealIDs(t *testing.T) {
	good := []string{
		"proj-c03",
		"proj-a3f8.1",
		"proj-a3f8.1.2",
		"shipmates-1",
		"a_b",
		"A1",
		"0",
		"has.dots.but.no.hops",
		strings.Repeat("a", maxLen),
	}
	for _, id := range good {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
}

// TestValidRejectsHostileIDs pins the reasons, not just the verdicts. The ".."
// cases are the ones the ship-side copy of this guard used to accept.
func TestValidRejectsHostileIDs(t *testing.T) {
	bad := []struct{ id, why string }{
		{"", "empty"},
		{"..", "bare parent-directory hop"},
		{"...", "hop with a trailing dot"},
		{"a..b", "hop in the middle"},
		{"proj-c03..", "trailing hop"},
		{"..proj-c03", "leading hop"},
		{"-rf", "leading dash reads as a flag"},
		{"--json", "leading dash reads as a flag"},
		{"-", "leading dash"},
		{"proj c03", "space"},
		{"proj/c03", "path separator"},
		{"proj\\c03", "windows path separator"},
		{"proj\nc03", "request-line framing"},
		{"proj\rc03", "request-line framing"},
		{"proj\x00c03", "NUL"},
		{"a%2fb", "percent is not in the alphabet"},
		{"$(whoami)", "substitution characters"},
		{"pröj-c03", "non-ascii"},
		{strings.Repeat("a", maxLen+1), "over the length bound"},
	}
	for _, tc := range bad {
		if Valid(tc.id) {
			t.Errorf("Valid(%q) = true, want false (%s)", tc.id, tc.why)
		}
	}
}

func TestValidateMessage(t *testing.T) {
	if err := Validate("proj-c03"); err != nil {
		t.Fatalf("Validate(proj-c03) = %v, want nil", err)
	}
	err := Validate("..")
	if err == nil {
		t.Fatal("Validate(..) = nil, want an error")
	}
	if !strings.Contains(err.Error(), `".."`) {
		t.Errorf("error should name the ..-hop rule, got %q", err)
	}
}
