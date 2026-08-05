package openai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestInstallPersona_CarriesTheArticlesReminder: the openai runtime builds
// its system prompt from the installed persona file, so splicing the
// reminder into that file is what puts the Articles in front of the model.
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
	path := personaPath(proj, "backend")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), brig.PromptStartMarker) || !strings.Contains(string(body), "Ship's Articles") {
		t.Fatalf("openai artifact lacks the Articles reminder:\n%s", body)
	}

	// And the reminder reaches the assembled system prompt — the thing the
	// model actually sees.
	prompt, err := systemPrompt(proj, "backend", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Ship's Articles") {
		t.Fatalf("system prompt lacks the Articles reminder:\n%s", prompt)
	}

	// Disabled brig: fresh install carries no block.
	if err := os.MkdirAll(filepath.Join(home, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".shipmates", "config.yaml"), []byte("brig:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if strings.Contains(string(body), brig.PromptStartMarker) {
		t.Fatalf("disabled brig still injected the reminder:\n%s", body)
	}
}
