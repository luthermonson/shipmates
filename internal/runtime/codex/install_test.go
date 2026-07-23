package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// codexInstallTester exposes just the install methods without needing an
// actual codex app-server transport. The receiver methods don't touch the
// adapter, so a zero-value Runtime is fine for install tests.
func codexInstallTester() *Runtime { return &Runtime{} }

func TestCodexInstallPersona_WritesCodexAgentDir(t *testing.T) {
	rt := codexInstallTester()
	proj := t.TempDir()
	err := rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{
		Name:         "skipper",
		Description:  "Voyage planner.",
		Model:        "o4",
		Capabilities: []string{"read", "plan"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(proj, ".codex", "agents", "skipper.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"name: skipper", "description: Voyage planner", "model: o4", "plan", "# skipper"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in output:\n%s", want, body)
		}
	}
}

func TestCodexInstallPersona_NoName(t *testing.T) {
	rt := codexInstallTester()
	err := rt.InstallPersona(context.Background(), t.TempDir(), runtime.PersonaSpec{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCodexUninstallPersona_Idempotent(t *testing.T) {
	rt := codexInstallTester()
	proj := t.TempDir()
	_ = rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{Name: "quartermaster"})
	if err := rt.UninstallPersona(context.Background(), proj, "quartermaster"); err != nil {
		t.Fatal(err)
	}
	if err := rt.UninstallPersona(context.Background(), proj, "quartermaster"); err != nil {
		t.Errorf("second uninstall should be idempotent, got %v", err)
	}
}

func TestCodexInstallMemoryHook_NoOp(t *testing.T) {
	// Phase 4 note: codex InstallMemoryHook is intentionally a no-op until
	// the session-start mechanism is factored out of codexapp. Test locks
	// that expectation so a future implementation deliberately breaks
	// this test as a "you're changing behavior" signal.
	rt := codexInstallTester()
	proj := t.TempDir()
	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Errorf("no-op should return nil; got %v", err)
	}
	// Should not have created any files.
	entries, _ := os.ReadDir(proj)
	if len(entries) != 0 {
		t.Errorf("no-op should not create files; found %d", len(entries))
	}
}
