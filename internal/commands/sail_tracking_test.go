package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tracker selection: markdown is the first-class default whenever a Beads
// workspace is not available; explicitly configuring beads without bd (or
// without an initialized workspace) errors instead of silently falling back.
func TestSelectVoyageTracker(t *testing.T) {
	t.Run("auto without beads workspace is markdown with one INFO line", func(t *testing.T) {
		root := t.TempDir()
		var out bytes.Buffer
		tr, err := selectVoyageTracker(root, "", &out)
		if err != nil || tr.Name() != "markdown" {
			t.Fatalf("tracker=%v err=%v", tr, err)
		}
		if !strings.Contains(out.String(), "TRACKER  markdown") || !strings.Contains(out.String(), "shipmates beads init") {
			t.Fatalf("info line = %q", out.String())
		}
	})
	t.Run("explicit markdown", func(t *testing.T) {
		var out bytes.Buffer
		tr, err := selectVoyageTracker(t.TempDir(), "markdown", &out)
		if err != nil || tr.Name() != "markdown" {
			t.Fatalf("tracker=%v err=%v", tr, err)
		}
	})
	t.Run("explicit beads without bd errors naming the install", func(t *testing.T) {
		// An empty PATH guarantees bd is unresolvable even on machines that
		// have it installed.
		t.Setenv("PATH", t.TempDir())
		var out bytes.Buffer
		_, err := selectVoyageTracker(t.TempDir(), "beads", &out)
		if err == nil || !strings.Contains(err.Error(), "bd CLI is not installed") {
			t.Fatalf("explicit beads without bd = %v", err)
		}
	})
	t.Run("explicit beads without workspace errors naming init", func(t *testing.T) {
		root := t.TempDir()
		// A fake bd on PATH proves the error is about the workspace, not the
		// binary. Windows needs an executable extension for LookPath.
		bin := t.TempDir()
		for _, name := range []string{"bd", "bd.bat"} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte("@echo off\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", bin)
		var out bytes.Buffer
		_, err := selectVoyageTracker(root, "beads", &out)
		if err == nil || !strings.Contains(err.Error(), "shipmates beads init") {
			t.Fatalf("explicit beads without workspace = %v", err)
		}
	})
	t.Run("unknown tracker name errors", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := selectVoyageTracker(t.TempDir(), "jira", &out); err == nil || !strings.Contains(err.Error(), "jira") {
			t.Fatalf("unknown tracker = %v", err)
		}
	})
}
