package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These two cases were pinned in internal/commands when the hook merge lived
// there (TestMergeSessionStartHook). The merge moved here in the runtime
// refactor; the cases move with it so hand-edited settings keep the coverage
// they had.

// A hand-written `"hooks": "nope"` must not panic the installer and must not
// cost the user their other settings: we replace only the piece we own.
func TestInstallMemoryHook_SurvivesHooksKeyOfTheWrongType(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	writeSettings(t, proj, map[string]any{"hooks": "nope", "keepMe": true})

	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	settings := readSettings(t, proj)
	if settings["keepMe"] != true {
		t.Error("unrelated key lost while repairing a malformed hooks key")
	}
	if n := countMemoryHooks(sessionStartGroups(settings)); n != 1 {
		t.Errorf("memory hooks = %d, want 1 (settings: %v)", n, settings)
	}
}

// Junk inside SessionStart — strings, numbers, half-shaped maps — must be
// carried through untouched, with our group added alongside.
func TestInstallMemoryHook_SurvivesJunkSessionStartEntries(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	junk := []any{"a string, not an object", 42, map[string]any{"hooks": "also wrong"}}
	writeSettings(t, proj, map[string]any{
		"hooks": map[string]any{"SessionStart": junk},
	})

	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	settings := readSettings(t, proj)
	groups := sessionStartGroups(settings)
	if n := countMemoryHooks(groups); n != 1 {
		t.Errorf("memory hooks = %d, want 1 alongside junk", n)
	}
	if n := countBrigHooks(groups); n != 1 {
		t.Errorf("brig hooks = %d, want 1 alongside junk", n)
	}
	// 3 junk entries preserved + our two groups (load-memory + brig-reminder).
	if len(groups) != 5 {
		t.Errorf("SessionStart len = %d, want 5 (junk preserved): %v", len(groups), groups)
	}
}

func writeSettings(t *testing.T, proj string, settings map[string]any) {
	t.Helper()
	dir := filepath.Join(proj, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSettings(t *testing.T, proj string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	return settings
}

// sessionStartGroups digs out hooks.SessionStart without failing the test on
// shape: these tests feed the installer hostile shapes on purpose.
func sessionStartGroups(settings map[string]any) []any {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	return groups
}
