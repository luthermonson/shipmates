package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// L7. keylog.go is a keylogger: while it is on, every keystroke an operator
// types into a mate's terminal — including secrets pasted into a prompt —
// lands in a file in cleartext. The mitigation is that it is opt-in, so
// "opt-in" is a property worth pinning rather than a comment worth trusting.
// See docs/security.md, "Keystroke logging".

// The environment variable is the ONLY switch. Unset, empty, or whitespace
// must all mean no file, no capture.
func TestKeyLogIsOptIn(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // so a stray relative-path open would be visible here

	cases := []struct {
		name string
		set  bool
		val  string
	}{
		{name: "unset", set: false},
		{name: "empty", set: true, val: ""},
		{name: "whitespace only", set: true, val: "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(keyLogEnv, tc.val)
			} else {
				// t.Setenv registers the restore; set then unset so an
				// ambient SHIPMATES_BRIDGE_KEYLOG cannot leak into the test.
				t.Setenv(keyLogEnv, "")
				if err := os.Unsetenv(keyLogEnv); err != nil {
					t.Fatal(err)
				}
			}
			l := openKeyLog()
			if l != nil {
				l.Close()
				t.Fatal("a keylog was opened without the operator asking for one")
			}
			// A nil log still has to absorb the whole key path.
			l.record(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hunter2")}, "TYPE", []byte("hunter2"), true)
			l.Close()
			if l.count() != 0 {
				t.Fatal("a disabled keylog recorded a keystroke")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("a disabled keylog created %v", entries)
			}
		})
	}
}

// And when the operator does ask for it, it records — in cleartext, secrets
// included. This test exists to document that, not to celebrate it: it is the
// reason docs/security.md tells operators to delete the file afterwards.
func TestKeyLogCapturesSecretsWhenExplicitlyEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.log")
	t.Setenv(keyLogEnv, path)

	l := openKeyLog()
	if l == nil {
		t.Fatal("the keylog did not open with the env var set to a path")
	}
	l.record(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk-live-abc123"), Paste: true},
		"TYPE", []byte("sk-live-abc123"), true)
	l.Close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sk-live-abc123") {
		t.Fatalf("the enabled keylog did not record the keystrokes:\n%s", body)
	}
}

// A path that cannot be opened must not take the bridge down with it —
// diagnostics are never load-bearing.
func TestKeyLogIgnoresAnUnopenablePath(t *testing.T) {
	// A directory is never a valid append target.
	dir := t.TempDir()
	t.Setenv(keyLogEnv, dir)
	if l := openKeyLog(); l != nil {
		l.Close()
		t.Fatal("openKeyLog returned a log for a path it cannot write")
	}
}

// The switch is an environment variable and nothing else: no shipmates.yaml
// key, no CLI flag. A checked-in config must not be able to turn a keylogger
// on for somebody else's crew.
func TestKeyLogHasNoConfigSwitch(t *testing.T) {
	if keyLogEnv != "SHIPMATES_BRIDGE_KEYLOG" {
		t.Fatalf("keyLogEnv = %q; docs/security.md names SHIPMATES_BRIDGE_KEYLOG", keyLogEnv)
	}
}
