package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestInstallPersona_CarriesTheArticlesReminder: codex has no session-start
// hook seam, so the persona artifact IS the prompt layer — the reminder must
// ride it, and must follow the operator's switch.
func TestInstallPersona_CarriesTheArticlesReminder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	rt := &Runtime{}
	proj := t.TempDir()
	spec := runtime.PersonaSpec{Name: "backend", SystemPrompt: "# Role\n\nBody.\n"}
	if err := rt.InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(proj, ".codex", "agents", "backend.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), brig.PromptStartMarker) || !strings.Contains(string(body), "Ship's Articles") {
		t.Fatalf("codex artifact lacks the Articles reminder:\n%s", body)
	}
	if !strings.Contains(string(body), "# Role") {
		t.Fatalf("persona body lost:\n%s", body)
	}

	// Disabled brig: no block.
	if err := os.MkdirAll(filepath.Join(home, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".shipmates", "config.yaml"), []byte("brig:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(proj, ".codex", "agents", "backend.md"))
	if strings.Contains(string(body), brig.PromptStartMarker) {
		t.Fatalf("disabled brig still injected the reminder:\n%s", body)
	}
}
