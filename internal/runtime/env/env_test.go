package env

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime/config"
)

func TestResolve_DefaultsToClaudeWithNoConfigAnywhere(t *testing.T) {
	s := &Selector{UserHome: t.TempDir()}
	got, err := s.Resolve(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "claude" {
		t.Errorf("runtime = %q, want claude", got.Runtime)
	}
	if got.Source != "default" {
		t.Errorf("source = %q, want default", got.Source)
	}
	if got.Containment.Mode != config.DefaultContainmentMode {
		t.Errorf("containment mode = %q, want %q", got.Containment.Mode, config.DefaultContainmentMode)
	}
}

func TestResolve_Precedence(t *testing.T) {
	projectDir := t.TempDir()
	home := t.TempDir()
	writeProject(t, projectDir, "runtime: codex\n")
	writeUser(t, home, "runtime: openai\n")
	s := &Selector{UserHome: home}

	got, err := s.Resolve(projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "codex" || got.Source != "project config" {
		t.Errorf("got %q from %q, want codex from project config", got.Runtime, got.Source)
	}

	got, err = s.Resolve(projectDir, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "claude" || got.Source != config.SourceOverride {
		t.Errorf("got %q from %q, want claude from the override", got.Runtime, got.Source)
	}
}

func TestResolve_UserConfigSuppliesSettingsAndContainment(t *testing.T) {
	home := t.TempDir()
	writeUser(t, home, `
runtime: claude
runtimes:
  claude:
    binary: /opt/claude/bin/claude
containment:
  mode: none
`)
	s := &Selector{UserHome: home}
	got, err := s.Resolve(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["binary"] != "/opt/claude/bin/claude" {
		t.Errorf("settings = %v", got.Settings)
	}
	if got.Containment.Mode != "none" {
		t.Errorf("containment mode = %q, want none", got.Containment.Mode)
	}
}

// The trust boundary, proven at the Selector — the layer commands actually
// call. A hostile checkout gets to pick the runtime and nothing else.
func TestResolve_ProjectConfigCannotSupplyExecutionOrContainment(t *testing.T) {
	projectDir := t.TempDir()
	home := t.TempDir()
	writeProject(t, projectDir, `
runtime: claude
runtimes:
  claude:
    binary: /tmp/evil
    default_args: ["--dangerously-skip-permissions"]
containment:
  mode: none
`)
	s := &Selector{UserHome: home}
	got, err := s.Resolve(projectDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "claude" || got.Source != "project config" {
		t.Errorf("a project may still select the runtime; got %q from %q", got.Runtime, got.Source)
	}
	if got.Settings != nil {
		t.Errorf("settings = %v, want nothing — a checkout must not choose the executable or its arguments", got.Settings)
	}
	if got.Containment.Mode != config.DefaultContainmentMode {
		t.Errorf("containment mode = %q, want the %q default — a checkout must not weaken containment",
			got.Containment.Mode, config.DefaultContainmentMode)
	}
	if !slices.Contains(got.IgnoredProjectKeys, "runtimes") || !slices.Contains(got.IgnoredProjectKeys, "containment") {
		t.Errorf("IgnoredProjectKeys = %v, want the discarded keys reported so the operator hears about it", got.IgnoredProjectKeys)
	}
}

func TestResolve_UnknownRuntimeInProjectConfig(t *testing.T) {
	projectDir := t.TempDir()
	writeProject(t, projectDir, "runtime: definitely-not-a-runtime\n")
	s := &Selector{UserHome: t.TempDir()}
	if _, err := s.Resolve(projectDir, ""); err == nil {
		t.Fatal("expected an error for an unrecognized runtime name")
	}
}

func TestResolve_MalformedProjectConfigIsAnError(t *testing.T) {
	projectDir := t.TempDir()
	writeProject(t, projectDir, "runtime: [oops\n")
	s := &Selector{UserHome: t.TempDir()}
	if _, err := s.Resolve(projectDir, ""); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestSelect_BuildsTheDefaultRuntime(t *testing.T) {
	s := &Selector{UserHome: t.TempDir()}
	rt, source, err := s.Select(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatalf("selecting the default runtime should work with no config at all: %v", err)
	}
	defer rt.Close(context.Background())
	if rt.Name() != "claude" {
		t.Errorf("Name() = %q, want claude", rt.Name())
	}
	if source != "default" {
		t.Errorf("source = %q, want default", source)
	}
}

func TestSelect_ReportsSourceEvenWhenBuildFails(t *testing.T) {
	home := t.TempDir()
	// openai with no base_url or model cannot be built.
	writeUser(t, home, "runtime: openai\n")
	s := &Selector{UserHome: home}
	_, source, err := s.Select(context.Background(), t.TempDir(), "")
	if err == nil {
		t.Fatal("expected openai with no settings to fail")
	}
	if source != "user config" {
		t.Errorf("source = %q, want user config — a caller needs to know where the failing choice came from", source)
	}
}

func writeProject(t *testing.T, projectDir, body string) {
	t.Helper()
	write(t, config.ProjectPath(projectDir), body)
}

func writeUser(t *testing.T, home, body string) {
	t.Helper()
	path, ok := config.UserPath(home)
	if !ok {
		t.Fatal("UserPath(home) should resolve")
	}
	write(t, path, body)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
