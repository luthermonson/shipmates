package env

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// writeProjectConfig writes .shipmates/config.yaml into projectDir with the
// given YAML body.
func writeProjectConfig(t *testing.T, projectDir, body string) {
	t.Helper()
	dir := filepath.Join(projectDir, ".shipmates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSelect_DefaultsToCodexWithNoConfig: the built-in default runtime is
// codex (the codex-native command surface stays the default behavior), and
// codex through the Selector surfaces as a typed ErrNotConfigured — the
// signal `ask` uses to take the codex-native dispatcher.
func TestSelect_DefaultsToCodexWithNoConfig(t *testing.T) {
	projectDir := t.TempDir()
	sel := New()
	_, src, err := sel.Select(context.Background(), projectDir, "")
	if src != "default" {
		t.Errorf("Source()=%q want default", src)
	}
	var notCfg *runtime.ErrNotConfigured
	if !errors.As(err, &notCfg) || notCfg.Runtime != "codex" {
		t.Fatalf("err = %v, want *ErrNotConfigured for codex", err)
	}
}

func TestSelect_ProjectConfigOverridesDefault(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectConfig(t, projectDir, `runtime: claude
runtimes:
  claude:
    binary: /custom/claude
`)
	sel := New()
	rt, src, err := sel.Select(context.Background(), projectDir, "")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
	if src != "project config" {
		t.Errorf("Source()=%q want project config", src)
	}
}

func TestSelect_CLIOverridesProject(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectConfig(t, projectDir, "runtime: claude\n")
	sel := New()
	// Codex will surface as ErrNotConfigured (needs NewCodexWith) — that's
	// fine, we're testing precedence + source labeling.
	_, src, err := sel.Select(context.Background(), projectDir, "codex")
	if src != "--runtime flag" {
		t.Errorf("Source()=%q want --runtime flag", src)
	}
	// Error must be typed ErrNotConfigured so commands can print an
	// operator-facing message.
	if err == nil {
		t.Fatal("expected ErrNotConfigured, got nil")
	}
	if _, ok := err.(*runtime.ErrNotConfigured); !ok {
		t.Errorf("err type = %T, want *runtime.ErrNotConfigured", err)
	}
}

func TestSelect_InvalidYAMLInProjectConfig(t *testing.T) {
	projectDir := t.TempDir()
	writeProjectConfig(t, projectDir, "runtime: [not a string\n") // malformed
	sel := New()
	_, _, err := sel.Select(context.Background(), projectDir, "")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}
