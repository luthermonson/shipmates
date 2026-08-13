package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestLoadUser_BrigBlock pins the brig: schema in user config — the only
// file the switch is honored from.
func TestLoadUser_BrigBlock(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "runtime: claude\nbrig:\n  enabled: false\n  disabled_articles:\n    - no-piped-execution\n    - twelve-factor\n"
	if err := os.WriteFile(filepath.Join(home, ".shipmates", "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	user, err := LoadUser(home)
	if err != nil {
		t.Fatal(err)
	}
	if user.Brig.Enabled == nil || *user.Brig.Enabled {
		t.Errorf("brig.enabled = %v, want explicit false", user.Brig.Enabled)
	}
	if !slices.Equal(user.Brig.DisabledArticles, []string{"no-piped-execution", "twelve-factor"}) {
		t.Errorf("disabled_articles = %v", user.Brig.DisabledArticles)
	}

	// Absence is distinguishable from explicit false — the default posture
	// (enabled) belongs to the consumer, not the parser.
	user, err = LoadUser(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if user.Brig.Enabled != nil || user.Brig.DisabledArticles != nil {
		t.Errorf("missing config parsed to %+v, want zero Brig", user.Brig)
	}
}

// TestLoadProject_BrigBlockIsIgnored pins the trust boundary at the parser:
// ProjectFile structurally cannot hold a brig block, and LoadProject reports
// the discarded key so the operator hears about it (the runtime selector
// warns with the list).
func TestLoadProject_BrigBlockIsIgnored(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "runtime: claude\nbrig:\n  enabled: false\n"
	if err := os.WriteFile(ProjectPath(proj), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := LoadProject(proj)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Runtime != "claude" {
		t.Errorf("runtime = %q, the trusted key must still load", pf.Runtime)
	}
	if !slices.Contains(pf.Ignored, "brig") {
		t.Errorf("Ignored = %v, want it to report the discarded brig block", pf.Ignored)
	}
}
