package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// A persona name reaches two places where a hostile value matters: it is
// joined into a filesystem path (AgentPath) and passed to the child process
// as the --agent flag value. Both are validated at the runtime seam, not
// only at the CLI boundary.
var hostilePersonaNames = []string{
	"../escape",
	"..\\escape",
	"nested/name",
	"nested\\name",
	"--dangerously-skip-permissions",
	"-rf",
	"has space",
	"", // empty is not a valid artifact name
}

func TestInstallPersonaRejectsHostileNames(t *testing.T) {
	rt := New(Config{})
	for _, name := range hostilePersonaNames {
		proj := t.TempDir()
		err := rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{Name: name, Description: "d"})
		if err == nil {
			t.Errorf("InstallPersona(%q) succeeded, want rejection", name)
			continue
		}
		// Nothing may be written outside the project's agents directory.
		if _, statErr := os.Stat(filepath.Join(filepath.Dir(proj), "escape.md")); statErr == nil {
			t.Errorf("InstallPersona(%q) wrote outside the project", name)
		}
	}
}

func TestStartSessionRejectsHostilePersonaNames(t *testing.T) {
	rt := New(Config{})
	for _, name := range hostilePersonaNames {
		if name == "" {
			continue // a session with no persona is legitimate
		}
		if _, err := rt.StartSession(context.Background(), runtime.SessionSpec{Persona: name}); err == nil {
			t.Errorf("StartSession(persona=%q) succeeded, want rejection", name)
		}
		if _, err := rt.ResumeSession(context.Background(), "sess-1", runtime.SessionSpec{Persona: name}); err == nil {
			t.Errorf("ResumeSession(persona=%q) succeeded, want rejection", name)
		}
	}
}

func TestStartSessionAllowsNoPersona(t *testing.T) {
	rt := New(Config{})
	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{})
	if err != nil || s == nil {
		t.Fatalf("StartSession with no persona: session=%v err=%v", s, err)
	}
}

func TestUninstallPersonaRejectsTraversal(t *testing.T) {
	rt := New(Config{})
	err := rt.UninstallPersona(context.Background(), t.TempDir(), "../../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "invalid persona") {
		t.Fatalf("UninstallPersona traversal error = %v, want invalid persona", err)
	}
}
