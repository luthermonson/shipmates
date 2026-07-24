package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

// writeHookProject lays down the minimum project shape the hook needs:
// shipmates.yaml at the root (FindRoot's marker) plus per-persona memory.
func writeHookProject(t *testing.T, memory map[string]map[string]string) {
	t.Helper()
	if err := os.WriteFile(project.ConfigName, []byte("crew: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for persona, files := range memory {
		dir := project.MemoryDir(persona)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestHookLoadMemory_PrintsPersonaMemoryFromEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	writeHookProject(t, map[string]map[string]string{
		"security": {"notes.md": "remember: audit the auth flow\n"},
		"backend":  {"notes.md": "backend-only knowledge\n"},
	})
	t.Setenv("SHIPMATES_PERSONA", "security")

	var out bytes.Buffer
	printHookMemory(&out)

	got := out.String()
	if !strings.Contains(got, "remember: audit the auth flow") {
		t.Errorf("output missing security memory: %q", got)
	}
	if !strings.Contains(got, "security/notes.md") {
		t.Errorf("output missing per-file header: %q", got)
	}
	if strings.Contains(got, "backend-only knowledge") {
		t.Errorf("output leaked another persona's memory: %q", got)
	}
}

func TestHookLoadMemory_PrintsAllPersonasWithoutEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	writeHookProject(t, map[string]map[string]string{
		"security": {"notes.md": "security memory\n"},
		"backend":  {"notes.md": "backend memory\n"},
	})
	t.Setenv("SHIPMATES_PERSONA", "")

	var out bytes.Buffer
	printHookMemory(&out)

	got := out.String()
	if !strings.Contains(got, "security memory") || !strings.Contains(got, "backend memory") {
		t.Errorf("output missing personas' memory: %q", got)
	}
}

func TestHookLoadMemory_EmptyAndMissingMemoryPrintNothing(t *testing.T) {
	t.Run("project without memory dir", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeHookProject(t, nil)
		t.Setenv("SHIPMATES_PERSONA", "security")
		var out bytes.Buffer
		printHookMemory(&out)
		if out.Len() != 0 {
			t.Errorf("expected no output, got %q", out.String())
		}
	})

	t.Run("outside any shipmates project", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("SHIPMATES_PERSONA", "security")
		var out bytes.Buffer
		printHookMemory(&out)
		if out.Len() != 0 {
			t.Errorf("expected no output, got %q", out.String())
		}
	})

	t.Run("invalid persona name is ignored", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeHookProject(t, map[string]map[string]string{
			"security": {"notes.md": "security memory\n"},
		})
		t.Setenv("SHIPMATES_PERSONA", "../escape")
		var out bytes.Buffer
		printHookMemory(&out)
		if out.Len() != 0 {
			t.Errorf("expected no output for invalid persona, got %q", out.String())
		}
	})
}

func TestHookLoadMemory_BoundsOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	huge := strings.Repeat("x", hookMemoryBudget*4)
	writeHookProject(t, map[string]map[string]string{
		"security": {"huge.md": huge},
	})
	t.Setenv("SHIPMATES_PERSONA", "security")

	var out bytes.Buffer
	printHookMemory(&out)
	if out.Len() > hookMemoryBudget+256 { // header + trailing newlines allowance
		t.Errorf("output exceeds budget: %d bytes", out.Len())
	}
}

// TestHookCommand_ExitsCleanOutsideProject drives the actual CLI command to
// verify the SessionStart contract: exit 0 (nil error) no matter what.
func TestHookCommand_ExitsCleanOutsideProject(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SHIPMATES_PERSONA", "")
	if err := runM10Command(t, Hook(), "hook", "load-memory"); err != nil {
		t.Fatalf("hook load-memory must never fail: %v", err)
	}
}
