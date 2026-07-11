package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestEnsureAttachGitignore(t *testing.T) {
	t.Run("creates .gitignore when missing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), attachInboxIgnorePattern) {
			t.Errorf("pattern not written: %q", got)
		}
	})

	t.Run("appends to existing .gitignore without duplicating", func(t *testing.T) {
		t.Chdir(t.TempDir())
		orig := "node_modules/\n*.log\n"
		if err := os.WriteFile(".gitignore", []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		// Second call must not add a second entry.
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "node_modules/") || !strings.Contains(string(got), "*.log") {
			t.Errorf("existing entries dropped: %q", got)
		}
		if n := strings.Count(string(got), attachInboxIgnorePattern); n != 1 {
			t.Errorf("expected exactly one inbox pattern, got %d: %q", n, got)
		}
	})

	t.Run("no-op when pattern already present", func(t *testing.T) {
		t.Chdir(t.TempDir())
		orig := "# user comment\n.shipmates/inbox/\nother\n"
		if err := os.WriteFile(".gitignore", []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != orig {
			t.Errorf("file changed when pattern already present:\nwant %q\ngot  %q", orig, got)
		}
	})
}

func TestComposeAgent(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/routing/github.md": {Data: []byte("## GitHub routing conventions\nROUTING BLOCK\n")},
	})
	base := []byte("---\nname: geordi\n---\n\n# Role\nbody\n")

	t.Run("no routing leaves base unchanged", func(t *testing.T) {
		t.Chdir(t.TempDir())
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Errorf("composeAgent changed base when no routing declared:\n%s", out)
		}
	})

	t.Run("routing appends a marked block", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if !strings.HasPrefix(s, "---\nname: geordi\n") {
			t.Error("base content not preserved at the top")
		}
		if !strings.Contains(s, "ROUTING BLOCK") {
			t.Error("routing block not appended")
		}
		if !strings.Contains(s, "<!-- shipmates:routing:github -->") || !strings.Contains(s, "<!-- /shipmates:routing:github -->") {
			t.Error("routing markers missing")
		}
	})

	t.Run("unknown routing name is a no-op", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Error("unknown routing name should leave base unchanged")
		}
	})
}

func TestAddPersona_VendorsPolicyYAML(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
		"catalog/geordi/policy.yaml": {Data: []byte("allow:\n  - Bash(git status)\ndeny:\n  - Bash(rm -rf /)\n")},
	})
	t.Chdir(t.TempDir())
	// Prep the layout addPersona expects.
	if err := os.MkdirAll(project.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	// Policy should have landed under .shipmates/policies/.
	polPath := filepath.Join(".shipmates", "policies", "geordi.yaml")
	b, err := os.ReadFile(polPath)
	if err != nil {
		t.Fatalf("expected vendored policy at %s: %v", polPath, err)
	}
	if !strings.Contains(string(b), "Bash(rm -rf /)") {
		t.Errorf("vendored policy missing content: %s", string(b))
	}
}

// TestEnsureSessionStartHook_CreatesFreshSettings: with no .claude/settings.json,
// the installer writes one containing just the SessionStart hook block.
func TestEnsureSessionStartHook_CreatesFreshSettings(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := ensureSessionStartHook(); err != nil {
		t.Fatalf("ensureSessionStartHook: %v", err)
	}
	b, err := os.ReadFile(claudeSettingsPath())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("settings not valid JSON: %v\n%s", err, b)
	}
	assertSessionStartHookPresent(t, got)
}

// TestEnsureSessionStartHook_PreservesExistingKeys: an existing settings.json
// with unrelated keys (permissions, env, etc.) survives with our hook block
// added in.
func TestEnsureSessionStartHook_PreservesExistingKeys(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{
  "permissions": {"allow": ["Bash(git status)"]},
  "env": {"MY_KEY": "value"}
}`
	if err := os.WriteFile(claudeSettingsPath(), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionStartHook(); err != nil {
		t.Fatalf("ensureSessionStartHook: %v", err)
	}
	b, err := os.ReadFile(claudeSettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("settings not valid JSON: %v\n%s", err, b)
	}
	// Existing keys preserved.
	if _, ok := got["permissions"]; !ok {
		t.Error("permissions key lost during merge")
	}
	if _, ok := got["env"]; !ok {
		t.Error("env key lost during merge")
	}
	assertSessionStartHookPresent(t, got)
}

// TestEnsureSessionStartHook_Idempotent: running twice does not add a second
// copy of the hook.
func TestEnsureSessionStartHook_Idempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := ensureSessionStartHook(); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, _ := os.ReadFile(claudeSettingsPath())
	if err := ensureSessionStartHook(); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, _ := os.ReadFile(claudeSettingsPath())
	if string(first) != string(second) {
		t.Errorf("second run changed settings.json:\nfirst:  %s\nsecond: %s", first, second)
	}
	// And verify count of shipmates hook entries is exactly one.
	var got map[string]any
	if err := json.Unmarshal(second, &got); err != nil {
		t.Fatal(err)
	}
	if n := countShipmatesHooks(got); n != 1 {
		t.Errorf("expected exactly one shipmates hook entry, got %d", n)
	}
}

// TestEnsureSessionStartHook_PreservesOtherSessionStartHook: a user (or
// another tool) with a pre-existing SessionStart hook keeps that hook —
// ours is appended, not substituted.
func TestEnsureSessionStartHook_PreservesOtherSessionStartHook(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatal(err)
	}
	orig := `{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "echo hello"}]}
    ]
  }
}`
	if err := os.WriteFile(claudeSettingsPath(), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionStartHook(); err != nil {
		t.Fatalf("ensureSessionStartHook: %v", err)
	}
	b, _ := os.ReadFile(claudeSettingsPath())
	if !strings.Contains(string(b), "echo hello") {
		t.Errorf("existing hook lost: %s", b)
	}
	if !strings.Contains(string(b), sessionStartHookCommand) {
		t.Errorf("shipmates hook not added: %s", b)
	}
}

// TestEnsureSessionStartHook_MalformedJSONIsRefused: a malformed settings.json
// is NOT silently overwritten — we return an error so the operator can fix
// their file rather than losing content.
func TestEnsureSessionStartHook_MalformedJSONIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeSettingsPath(), []byte(`{malformed`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureSessionStartHook(); err == nil {
		t.Error("expected error on malformed settings.json, got nil")
	}
	// The bad file should be untouched.
	b, _ := os.ReadFile(claudeSettingsPath())
	if string(b) != `{malformed` {
		t.Errorf("malformed file was rewritten: %q", b)
	}
}

// assertSessionStartHookPresent walks a parsed settings map and fails the
// test if there's no shipmates SessionStart hook command entry.
func assertSessionStartHookPresent(t *testing.T, settings map[string]any) {
	t.Helper()
	if countShipmatesHooks(settings) < 1 {
		t.Errorf("expected shipmates SessionStart hook in settings, got:\n%+v", settings)
	}
}

// countShipmatesHooks counts hook entries in settings.hooks.SessionStart[*].hooks[*]
// whose command matches our sentinel. Used by the idempotency check to make
// sure re-running doesn't stack duplicates.
func countShipmatesHooks(settings map[string]any) int {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return 0
	}
	starts, _ := hooks["SessionStart"].([]any)
	n := 0
	for _, m := range starts {
		mm, _ := m.(map[string]any)
		inner, _ := mm["hooks"].([]any)
		for _, h := range inner {
			hh, _ := h.(map[string]any)
			if hh["type"] == "command" && strings.TrimSpace(toStringLike(hh["command"])) == sessionStartHookCommand {
				n++
			}
		}
	}
	return n
}

func toStringLike(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestAddPersona_NoPolicyYAMLIsFine(t *testing.T) {
	// Personas without a policy.yaml must install cleanly and NOT create an
	// empty policy file — that would be a footgun for operators wondering
	// what's in the empty file.
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
	})
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(project.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".shipmates", "policies", "geordi.yaml")); !os.IsNotExist(err) {
		t.Errorf("no-policy persona should not create a policy file (err=%v)", err)
	}
}
