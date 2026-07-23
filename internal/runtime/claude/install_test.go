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

func TestInstallMemoryHook_CreatesSettings(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("no hooks block; got %v", settings)
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	if len(sessionStart) != 1 {
		t.Fatalf("SessionStart len = %d, want 1", len(sessionStart))
	}
	entry := sessionStart[0].(map[string]any)
	if !strings.Contains(entry["command"].(string), "load-memory") {
		t.Errorf("command missing marker; got %v", entry)
	}
}

func TestInstallMemoryHook_IdempotentAndPreservesExisting(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	// Pre-write settings.json with an unrelated user hook.
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      { "type": "command", "command": "echo hi" }
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
	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" {
		t.Errorf("preserved theme lost; got %v", settings)
	}
	hooks := settings["hooks"].(map[string]any)
	starts := hooks["SessionStart"].([]any)
	if len(starts) != 2 {
		t.Errorf("SessionStart len = %d, want 2 (existing + ours, not doubled by 2nd install)", len(starts))
	}
	// Marker must appear exactly once.
	markers := 0
	for _, e := range starts {
		m := e.(map[string]any)
		if cmd, _ := m["command"].(string); strings.Contains(cmd, "load-memory") {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("found %d load-memory hooks, want exactly 1", markers)
	}
}
