package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

func TestInstallPersona_WritesFileWithFrontmatter(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	spec := runtime.PersonaSpec{
		Name:         "architect",
		Description:  "Cross-cutting design review.",
		Model:        "sonnet",
		Capabilities: []string{"read", "edit", "bash"},
		SystemPrompt: "# Custom prompt\n\nBody text.\n",
	}
	if err := rt.InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := filepath.Join(proj, ".claude", "agents", "architect.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.HasPrefix(body, "---\n") {
		t.Errorf("missing frontmatter start")
	}
	for _, want := range []string{"name: architect", "description: Cross-cutting design review", "model: sonnet", "read", "# Custom prompt"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; got:\n%s", want, body)
		}
	}
}

func TestInstallPersona_EmptyPromptGeneratesDefault(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	err := rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{
		Name:        "backend",
		Description: "Backend review.",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(proj, ".claude", "agents", "backend.md"))
	body := string(data)
	if !strings.Contains(body, "# backend") {
		t.Errorf("expected default heading; got:\n%s", body)
	}
	if !strings.Contains(body, "Backend review.") {
		t.Errorf("expected description in body")
	}
}

func TestInstallPersona_NoName(t *testing.T) {
	rt := New(Config{})
	err := rt.InstallPersona(context.Background(), t.TempDir(), runtime.PersonaSpec{})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestUninstallPersona_Idempotent(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	// Install then uninstall twice — second uninstall must not error.
	_ = rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{Name: "tester"})
	if err := rt.UninstallPersona(context.Background(), proj, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := rt.UninstallPersona(context.Background(), proj, "tester"); err != nil {
		t.Errorf("second uninstall should be idempotent, got %v", err)
	}
	if err := rt.UninstallPersona(context.Background(), proj, "never-installed"); err != nil {
		t.Errorf("uninstalling never-existed should not error, got %v", err)
	}
}

// readSessionStart returns the parsed settings file and its SessionStart
// hook groups.
func readSessionStart(t *testing.T, proj string) (map[string]any, []any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("no hooks block; got %v", settings)
	}
	groups, _ := hooks["SessionStart"].([]any)
	return settings, groups
}

// countMemoryHooks counts our marker command across every SessionStart
// matcher group, in the nested shape Claude Code actually executes.
func countMemoryHooks(groups []any) int {
	n := 0
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); strings.Contains(cmd, "load-memory") {
				n++
			}
		}
	}
	return n
}

// TestInstallMemoryHook_WritesTheShapeClaudeExecutes pins the matcher-group
// form. The flatter {"SessionStart":[{"type":"command",…}]} spelling parses
// but is silently ignored by claude 2.1.153 — verified with
// --include-hook-events: only the nested form produced hook_started /
// hook_response frames.
func TestInstallMemoryHook_WritesTheShapeClaudeExecutes(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	_, groups := readSessionStart(t, proj)
	if len(groups) != 1 {
		t.Fatalf("SessionStart len = %d, want 1", len(groups))
	}
	group := groups[0].(map[string]any)
	if _, flat := group["command"]; flat {
		t.Fatal("hook written in the flat shape claude ignores")
	}
	inner, _ := group["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("group hooks len = %d, want 1: %v", len(inner), group)
	}
	entry := inner[0].(map[string]any)
	if entry["type"] != "command" || entry["command"] != MemoryHookCommand {
		t.Errorf("hook entry = %v", entry)
	}
	// No matcher: memory must load on resumed sessions too, not only fresh ones.
	if _, ok := group["matcher"]; ok {
		t.Errorf("group pins a matcher, which would skip non-startup sessions: %v", group)
	}
}

func TestInstallMemoryHook_IdempotentAndPreservesExisting(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	// Pre-write settings.json with an unrelated user hook and unrelated keys.
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "echo hi" } ] }
    ],
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "echo tool" } ] }
    ]
  },
  "theme": "dark"
}`)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), pre, 0o644)

	// Install twice.
	for range 2 {
		if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
			t.Fatal(err)
		}
	}
	settings, groups := readSessionStart(t, proj)
	if settings["theme"] != "dark" {
		t.Errorf("unrelated key lost; got %v", settings)
	}
	if _, ok := settings["hooks"].(map[string]any)["PreToolUse"]; !ok {
		t.Errorf("unrelated hook event lost; got %v", settings)
	}
	if len(groups) != 2 {
		t.Errorf("SessionStart len = %d, want 2 (the operator's group + ours, not doubled)", len(groups))
	}
	if n := countMemoryHooks(groups); n != 1 {
		t.Errorf("found %d load-memory hooks, want exactly 1", n)
	}
	// The operator's own hook must survive verbatim.
	first := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if first["command"] != "echo hi" {
		t.Errorf("operator hook was rewritten: %v", first)
	}
}

// TestInstallMemoryHook_MigratesTheLegacyFlatEntry proves a previously
// written flat entry — which claude never executed — is replaced by a
// working group rather than left behind as dead configuration.
func TestInstallMemoryHook_MigratesTheLegacyFlatEntry(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	pre := []byte(`{"hooks":{"SessionStart":[{"type":"command","command":"` + MemoryHookCommand + `"}]}}`)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), pre, 0o644)

	if err := InstallMemoryHookAt(proj); err != nil {
		t.Fatal(err)
	}
	_, groups := readSessionStart(t, proj)
	if len(groups) != 1 {
		t.Fatalf("SessionStart len = %d, want 1 (migrated in place)", len(groups))
	}
	if _, flat := groups[0].(map[string]any)["command"]; flat {
		t.Fatal("legacy flat entry survived migration")
	}
	if n := countMemoryHooks(groups); n != 1 {
		t.Errorf("found %d load-memory hooks after migration, want 1", n)
	}
}

// TestInstallMemoryHook_RejectsUnparseableSettings proves an operator's
// broken settings file is reported, never silently overwritten.
func TestInstallMemoryHook_RejectsUnparseableSettings(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(path, []byte(`{"theme": "dark"`), 0o644)

	if err := InstallMemoryHookAt(proj); err == nil {
		t.Fatal("expected an error for unparseable settings")
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"theme": "dark"` {
		t.Errorf("broken settings file was modified: %s", data)
	}
}

// TestInstallMemoryHook_TreatsEmptyFileAsAbsent covers the file an editor
// or a failed write can leave behind.
func TestInstallMemoryHook_TreatsEmptyFileAsAbsent(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte("  \n"), 0o644)

	if err := InstallMemoryHookAt(proj); err != nil {
		t.Fatal(err)
	}
	if _, groups := readSessionStart(t, proj); countMemoryHooks(groups) != 1 {
		t.Errorf("hook not installed over an empty file: %v", groups)
	}
}
