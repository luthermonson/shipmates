package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestOpenCodexOnlyPersonaReachesDashboard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open uses a PTY code path that is unix-only; Windows returns a different unsupported-platform error")
	}
	t.Chdir(t.TempDir())
	writeOpenCodexPersona(t, "backend", `developer_instructions = "Review backend changes."`+"\n")

	err := Open().Run(context.Background(), []string{"open", "backend"})
	if !errors.Is(err, dashboard.ErrNotTTY) {
		t.Fatalf("open error = %v, want dashboard TTY refusal", err)
	}
}

func TestOpenRejectsMissingCodexArtifact(t *testing.T) {
	t.Chdir(t.TempDir())

	err := Open().Run(context.Background(), []string{"open", "backend"})
	if err == nil || !strings.Contains(err.Error(), `persona "backend" is not installed`) {
		t.Fatalf("open error = %v, want missing canonical artifact refusal", err)
	}
}

func TestOpenRejectsInvalidCodexArtifact(t *testing.T) {
	t.Chdir(t.TempDir())
	writeOpenCodexPersona(t, "backend", "name = \"backend\"\n")

	err := Open().Run(context.Background(), []string{"open", "backend"})
	if err == nil || !strings.Contains(err.Error(), "invalid Codex persona") || !strings.Contains(err.Error(), "missing developer_instructions") {
		t.Fatalf("open error = %v, want invalid canonical artifact refusal", err)
	}
}

func writeOpenCodexPersona(t *testing.T, persona, content string) {
	t.Helper()
	path := project.CodexAgentPath(persona)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
