package project

import (
	"os"
	"reflect"
	"testing"
)

// launchArgs is the shape every caller (ask/drain/open/fanout/live server)
// depends on, so the assertions here are exact rather than "contains".
func TestSessionLaunchCreatesWhenNoSession(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, args, id, name, fp, creating := SessionLaunch("captain", false)
	if !creating {
		t.Fatal("creating = false with no tracked session")
	}
	if id == "" {
		t.Fatal("id is empty")
	}
	if name != "captain" {
		t.Fatalf("name = %q, want captain (no configured prefix)", name)
	}
	want := []string{"--session-id", id, "--name", name, "--agent", "captain"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	if fp != cfg.Fingerprint() {
		t.Fatalf("fp = %q, want the resolved config's fingerprint %q", fp, cfg.Fingerprint())
	}
}

func TestSessionLaunchResumesTrackedSession(t *testing.T) {
	t.Chdir(t.TempDir())

	fp := PersonaConfig{}.Fingerprint()
	if err := WriteSessionMeta("captain", "captain", "uuid-abc", fp, ""); err != nil {
		t.Fatal(err)
	}

	_, args, id, _, gotFP, creating := SessionLaunch("captain", false)
	if creating {
		t.Fatal("creating = true for an unchanged tracked session")
	}
	if id != "uuid-abc" {
		t.Fatalf("id = %q, want the stored uuid", id)
	}
	// Resume by UUID, never by name: two sessions can share a name and
	// `--resume <name>` then errors as ambiguous.
	want := []string{"--resume", "uuid-abc", "--agent", "captain"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	if gotFP != fp {
		t.Fatalf("fp = %q, want %q", gotFP, fp)
	}
}

func TestSessionLaunchFreshOverridesResume(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := PersonaConfig{}.Fingerprint()
	if err := WriteSessionMeta("captain", "captain", "uuid-abc", fp, ""); err != nil {
		t.Fatal(err)
	}

	_, args, id, _, _, creating := SessionLaunch("captain", true)
	if !creating {
		t.Fatal("creating = false when fresh was requested")
	}
	if id == "uuid-abc" {
		t.Fatal("fresh launch reused the tracked session id")
	}
	if args[0] != "--session-id" {
		t.Fatalf("args = %v, want a --session-id create", args)
	}
}

func TestSessionLaunchAutoFreshOnConfigDrift(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "captain", "model: claude-opus-4-7\neffort: high\n")

	// The stored hash is from a different model/effort — the session baked the
	// old ones, so it can't be resumed with the new config.
	if err := WriteSessionMeta("captain", "captain", "uuid-old", "stale-hash", ""); err != nil {
		t.Fatal(err)
	}

	cfg, args, id, _, fp, creating := SessionLaunch("captain", false)
	if !creating {
		t.Fatal("creating = false despite a changed config fingerprint")
	}
	if id == "uuid-old" {
		t.Fatal("drifted config reused the old session id")
	}
	if args[0] != "--session-id" {
		t.Fatalf("args = %v, want a --session-id create", args)
	}
	if fp != cfg.Fingerprint() || fp == "stale-hash" {
		t.Fatalf("fp = %q, want the newly resolved fingerprint", fp)
	}
}

func TestSessionLaunchLegacyMetaResumesWithoutAutoFresh(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "captain", "model: claude-opus-4-7\n")

	// An empty stored hash means "written before fingerprinting existed". That
	// must NOT count as drift — we don't abandon a pre-upgrade session.
	meta := SessionMeta{Name: "captain", ID: "uuid-legacy", ConfigHash: ""}
	if err := WriteSessionMeta("captain", meta.Name, meta.ID, meta.ConfigHash, ""); err != nil {
		t.Fatal(err)
	}

	_, args, id, _, _, creating := SessionLaunch("captain", false)
	if creating {
		t.Fatal("an empty stored fingerprint was treated as drift")
	}
	if id != "uuid-legacy" {
		t.Fatalf("id = %q, want uuid-legacy", id)
	}
	if args[0] != "--resume" {
		t.Fatalf("args = %v, want a --resume", args)
	}
}

func TestSessionLaunchCreatesWhenTrackedIDMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	fp := PersonaConfig{}.Fingerprint()
	// A name-only marker (no UUID) cannot be resumed unambiguously.
	if err := WriteSessionMeta("captain", "captain", "", fp, ""); err != nil {
		t.Fatal(err)
	}

	_, args, id, _, _, creating := SessionLaunch("captain", false)
	if !creating {
		t.Fatal("creating = false with no tracked session id to resume")
	}
	if id == "" {
		t.Fatal("no id minted")
	}
	if args[0] != "--session-id" {
		t.Fatalf("args = %v, want a --session-id create", args)
	}
}

func TestSessionLaunchUsesConfiguredPrefix(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(ConfigName, []byte("sessionPrefix: project-one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, args, _, name, _, _ := SessionLaunch("picard", false)
	if name != "project-one-picard" {
		t.Fatalf("name = %q, want project-one-picard", name)
	}
	// The prefixed name is what lands in --name.
	if args[3] != "project-one-picard" {
		t.Fatalf("args = %v, want --name project-one-picard", args)
	}
}

func TestSessionLaunchCarriesResolvedConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "captain", "model: claude-opus-4-7\neffort: high\nberth: auto\n")
	cfg, _, _, _, _, _ := SessionLaunch("captain", false)
	if cfg.Model != "claude-opus-4-7" || cfg.Effort != "high" || cfg.Berth != "auto" {
		t.Fatalf("cfg = %+v, want the persona's resolved frontmatter", cfg)
	}
}

func TestResumeCWDNoSession(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := ResumeCWD("ghost"); got != "" {
		t.Fatalf("ResumeCWD(ghost) = %q, want empty", got)
	}
}
