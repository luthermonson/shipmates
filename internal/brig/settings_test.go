package brig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime/config"
)

// configBrig builds a config.Brig literal for tests.
func configBrig(enabled *bool, disabled []string) config.Brig {
	return config.Brig{Enabled: enabled, DisabledArticles: disabled}
}

func TestFromConfigDefaults(t *testing.T) {
	s := FromConfig(config.Brig{})
	if !s.Enabled {
		t.Fatal("absent brig block must default to enabled — installed means bound")
	}
	if got := s.DisabledHandles(); len(got) != 0 {
		t.Fatalf("default posture waives %v", got)
	}
	if s.Disabled("no-destructive-git") {
		t.Error("default posture reports an Article waived")
	}
	if !s.ArticleEnabled(7) {
		t.Error("ArticleEnabled(7) false under default posture")
	}
}

func TestFromConfigDisabled(t *testing.T) {
	off := false
	s := FromConfig(configBrig(&off, nil))
	if s.Enabled {
		t.Fatal("enabled: false ignored")
	}
	// A disabled brig waives everything.
	if !s.Disabled("no-destructive-git") || !s.Disabled("respect-the-freeze") {
		t.Error("disabled brig must waive every Article")
	}
	on := true
	if !FromConfig(configBrig(&on, nil)).Enabled {
		t.Error("explicit enabled: true ignored")
	}
}

func TestFromConfigDisabledArticles(t *testing.T) {
	s := FromConfig(configBrig(nil, []string{"no-piped-execution", " Twelve-Factor "}))
	if !s.Disabled("no-piped-execution") {
		t.Error("listed handle not waived")
	}
	if !s.Disabled("twelve-factor") {
		t.Error("handle matching should trim and lowercase")
	}
	if s.Disabled("no-destructive-git") {
		t.Error("unlisted handle waived")
	}
	if got := s.DisabledHandles(); len(got) != 2 || got[0] != "twelve-factor" || got[1] != "no-piped-execution" {
		t.Errorf("DisabledHandles = %v, want canonical order [twelve-factor no-piped-execution]", got)
	}
}

// TestFromConfigUnknownHandleIgnored: a typo must not silently waive a
// different Article — and must not take the rest of the config down.
func TestFromConfigUnknownHandleIgnored(t *testing.T) {
	s := FromConfig(configBrig(nil, []string{"no-such-article", "no-prod-db"}))
	if !s.Disabled("no-prod-db") {
		t.Error("valid handle next to a typo was dropped")
	}
	if got := s.DisabledHandles(); len(got) != 1 {
		t.Errorf("DisabledHandles = %v, want just no-prod-db", got)
	}
}

func TestLoadReadsUserConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "brig:\n  enabled: true\n  disabled_articles: [verify-every-package]\n"
	if err := os.WriteFile(filepath.Join(home, ".shipmates", "config.yaml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Load(home)
	if !s.Enabled {
		t.Fatal("Load lost enabled")
	}
	if !s.Disabled("verify-every-package") {
		t.Error("Load lost disabled_articles")
	}
	// Missing config: default posture.
	s = Load(t.TempDir())
	if !s.Enabled || len(s.DisabledHandles()) != 0 {
		t.Errorf("missing config => %+v, want default enabled", s)
	}
}

// TestProjectConfigCannotCarryBrig pins the trust boundary from the brig's
// side: a `brig:` block in a project checkout's .shipmates/config.yaml is
// reported as an ignored key by the runtime config loader — the schema has
// no field for it — so nothing a cloned repo says can reach brig.Load.
func TestProjectConfigCannotCarryBrig(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "runtime: claude\nbrig:\n  enabled: false\n"
	if err := os.WriteFile(config.ProjectPath(proj), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := config.LoadProject(proj)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range pf.Ignored {
		if k == "brig" {
			found = true
		}
	}
	if !found {
		t.Fatalf("project brig block not reported as ignored; Ignored = %v", pf.Ignored)
	}
	// And the brig posture for a home with no config stays the default even
	// though the project said enabled: false.
	if s := Load(t.TempDir()); !s.Enabled {
		t.Fatal("a project checkout disabled the brig — trust boundary broken")
	}
}
