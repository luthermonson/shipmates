package personaname

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	good := []string{"captain", "data", "data-2", "first_officer", "q", "a1", "x_y-z9"}
	for _, name := range good {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false, want true", name)
		}
	}

	bad := []struct{ name, why string }{
		{"", "empty"},
		{"Data", "uppercase — persona paths are case-sensitive on Linux, not on Windows"},
		{"2fast", "must start with a letter"},
		{"-rf", "leading dash reads as another CLI flag"},
		{"_private", "must start with a letter"},
		{"has space", "a space splits an HTTP request target and an argv element"},
		{"crlf\r\ninjected", "CR/LF ends an HTTP request line"},
		{"tab\there", "control character"},
		{"a/b", "path separator"},
		{"a\\b", "path separator on Windows"},
		{"..", "parent-directory hop"},
		{"../escape", "parent-directory hop"},
		{"a.b", "dot is not in the alphabet — it enables traversal in paths"},
		{"héllo", "non-ASCII"},
		{"a%2fb", "percent-encoding is not decoded consistently by every consumer"},
		{"a?b", "query separator"},
		{"a#b", "fragment separator"},
		{"trailing\n", "trailing LF"},
	}
	for _, tc := range bad {
		if Valid(tc.name) {
			t.Errorf("Valid(%q) = true, want false (%s)", tc.name, tc.why)
		}
	}
}

func TestValidate_ErrorNamesTheInput(t *testing.T) {
	if err := Validate("captain"); err != nil {
		t.Fatalf("Validate(captain) = %v, want nil", err)
	}
	err := Validate("bad/name")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "bad/name") {
		t.Errorf("error should quote the offending name, got %q", err)
	}
}
