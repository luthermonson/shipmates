package berth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

// v04Config is a shipmates.yaml as v0.4.0 wrote it, berth and cwd crew
// overrides included. project.LoadConfigAt sets yaml KnownFields(true), so an
// unknown key is a hard parse error rather than a silently ignored line —
// which is exactly how the berth surface regressed: every project carrying
// this file stopped loading at all.
const v04Config = `sessionPrefix: demo
skipperPersona: skipper
modelLadder:
  - gpt-5.6-luna
  - gpt-5.6-sol
sharedMemory: false
crew:
  skipper:
    model: gpt-5.6-sol
    effort: high
    berth: auto
  quartermaster:
    berth: require
  tester:
    cwd: sandbox/tester
  backend:
    berth: off
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, project.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestV04ConfigStillParses is the regression test for the config surface: this
// file must load, not fail with `field berth not found in type
// project.CrewOverride`.
func TestV04ConfigStillParses(t *testing.T) {
	root := writeConfig(t, v04Config)
	conf, err := project.LoadConfigAt(root)
	if err != nil {
		t.Fatalf("LoadConfigAt rejected a v0.4.0 config: %v", err)
	}
	if got := conf.Crew["skipper"].Berth; got != "auto" {
		t.Errorf("crew.skipper.berth = %q, want auto", got)
	}
	if got := conf.Crew["tester"].CWD; got != "sandbox/tester" {
		t.Errorf("crew.tester.cwd = %q, want sandbox/tester", got)
	}
}

// TestUnknownCrewKeyStillFails guards the other direction: restoring berth/cwd
// must not have loosened KnownFields, which is what makes a typo an error
// instead of a silently ignored setting.
func TestUnknownCrewKeyStillFails(t *testing.T) {
	root := writeConfig(t, "crew:\n  skipper:\n    berthh: auto\n")
	if _, err := project.LoadConfigAt(root); err == nil {
		t.Fatal("LoadConfigAt accepted an unknown crew key; KnownFields is no longer strict")
	} else if !strings.Contains(err.Error(), "berthh") {
		t.Errorf("parse error does not name the offending key: %v", err)
	}
}

// TestResolvePersonaConfigCarriesBerthAndCWD closes the loop: the crew
// override has to reach PersonaConfig, because that is the struct every spawn
// site hands to ResolveSpawnCWD.
func TestResolvePersonaConfigCarriesBerthAndCWD(t *testing.T) {
	root := writeConfig(t, v04Config)
	for _, tt := range []struct{ persona, berth, cwd string }{
		{"skipper", "auto", ""},
		{"quartermaster", "require", ""},
		{"tester", "", "sandbox/tester"},
		{"backend", "off", ""},
		{"nobody", "", ""}, // no override at all — the fleet default
	} {
		cfg, err := project.ResolvePersonaConfigAt(root, tt.persona)
		if err != nil {
			t.Fatalf("ResolvePersonaConfigAt(%s): %v", tt.persona, err)
		}
		if cfg.Berth != tt.berth || cfg.CWD != tt.cwd {
			t.Errorf("%s resolved berth=%q cwd=%q, want berth=%q cwd=%q", tt.persona, cfg.Berth, cfg.CWD, tt.berth, tt.cwd)
		}
	}
}

// TestBerthAndCWDStayOutOfFingerprint is the guardrail from the design doc: if
// berth entered PersonaConfig.Fingerprint, then giving a persona a berth would
// auto-fresh the very session the berth is meant to keep. The working
// directory is folded in separately, at the spawn site, by SessionFingerprint.
func TestBerthAndCWDStayOutOfFingerprint(t *testing.T) {
	plain := project.PersonaConfig{Model: "gpt-5.6-sol", Effort: "high"}
	berthed := plain
	berthed.Berth = "auto"
	berthed.CWD = "somewhere/else"
	if plain.Fingerprint() != berthed.Fingerprint() {
		t.Error("berth/cwd entered PersonaConfig.Fingerprint; gaining a berth would auto-fresh the session it means to preserve")
	}
}

// TestResolveSpawnCWDFromRealConfig exercises the whole path an operator sees:
// a shipmates.yaml on disk in a real git repo, resolved into a provisioned
// worktree for one persona and nothing at all for another.
func TestResolveSpawnCWDFromRealConfig(t *testing.T) {
	root := initTmpGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, project.ConfigName), []byte(v04Config), 0o644); err != nil {
		t.Fatal(err)
	}

	skipperCfg, err := project.ResolvePersonaConfigAt(root, "skipper")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSpawnCWDAt(root, "skipper", skipperCfg)
	if err != nil {
		t.Fatalf("ResolveSpawnCWDAt(skipper): %v", err)
	}
	if want := filepath.Join(root, Dir("skipper")); got != want {
		t.Errorf("skipper cwd = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, ".git")); err != nil {
		t.Errorf("skipper berth was not provisioned: %v", err)
	}

	backendCfg, err := project.ResolvePersonaConfigAt(root, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveSpawnCWDAt(root, "backend", backendCfg); err != nil || got != "" {
		t.Errorf("backend (berth: off) cwd = %q, %v; want empty", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, Dir("backend"))); !os.IsNotExist(err) {
		t.Errorf("berth: off still provisioned a worktree: %v", err)
	}
}
