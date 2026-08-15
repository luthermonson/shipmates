package project

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

func TestSHA(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
	}{
		{"empty", []byte("")},
		{"hello", []byte("hello")},
		{"binary", []byte{0x00, 0x01, 0xff}},
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SHA(tt.in)
			if len(got) != 64 {
				t.Fatalf("SHA(%q) length = %d, want 64", tt.in, len(got))
			}
			if again := SHA(tt.in); again != got {
				t.Fatalf("SHA not deterministic: %q vs %q", got, again)
			}
			if !hexRe.MatchString(got) {
				t.Fatalf("SHA(%q) = %q, not lowercase hex", tt.in, got)
			}
		})
	}
	if SHA([]byte("a")) == SHA([]byte("b")) {
		t.Fatal("distinct inputs produced same SHA")
	}
}

func TestManifestRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.MkdirAll(Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	want := &Manifest{
		Version: "1",
		Files: map[string]string{
			".claude/agents/captain.md": SHA([]byte("body")),
			"shipmates.yaml":          SHA([]byte("cfg")),
		},
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if got.Version != want.Version {
		t.Errorf("Version = %q, want %q", got.Version, want.Version)
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("Files len = %d, want %d", len(got.Files), len(want.Files))
	}
	for k, v := range want.Files {
		if got.Files[k] != v {
			t.Errorf("Files[%q] = %q, want %q", k, got.Files[k], v)
		}
	}
}

func TestLoadManifestMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	m, err := LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest on empty dir: %v", err)
	}
	if m.Files == nil {
		t.Fatal("Files map is nil, want empty non-nil map")
	}
	if len(m.Files) != 0 {
		t.Fatalf("Files len = %d, want 0", len(m.Files))
	}
}

func TestSessionName(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string // empty => no shipmates.yaml written
		persona    string
		want       string
	}{
		{"with prefix", "sessionPrefix: myrepo\n", "captain", "myrepo-captain"},
		{"empty prefix", "sessionPrefix: \"\"\n", "captain", "captain"},
		{"no config file", "", "tester", "tester"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.configYAML != "" {
				if err := os.WriteFile(ConfigName, []byte(tt.configYAML), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := SessionName(tt.persona); got != tt.want {
				t.Fatalf("SessionName(%q) = %q, want %q", tt.persona, got, tt.want)
			}
		})
	}
}

func TestResolvePersonaConfigMissing(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	cfg, err := ResolvePersonaConfig("ghost")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig missing persona: %v", err)
	}
	if !reflect.DeepEqual(cfg, PersonaConfig{}) {
		t.Fatalf("missing persona = %+v, want zero PersonaConfig", cfg)
	}
}

// writeAgent installs a fake persona file at AgentPath(persona) relative to cwd,
// wrapping the given frontmatter body in --- delimiters.
func writeAgent(t *testing.T, persona, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "---\n\nbody text\n"
	if err := os.WriteFile(AgentPath(persona), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePersonaConfigFrontmatterOnly(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	writeAgent(t, "captain",
		"permissions:\n  mode: acceptEdits\nremoteControl: true\nmodel: claude-opus-4-7\neffort: high\n")

	cfg, err := ResolvePersonaConfig("captain")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if cfg.Mode != "acceptEdits" {
		t.Errorf("Mode = %q, want acceptEdits", cfg.Mode)
	}
	if cfg.RemoteControl != "captain" {
		t.Errorf("RemoteControl = %q, want captain", cfg.RemoteControl)
	}
	if cfg.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", cfg.Model)
	}
	if cfg.Effort != "high" {
		t.Errorf("Effort = %q, want high", cfg.Effort)
	}
	if len(cfg.Refused) != 0 {
		t.Errorf("Refused = %v, want none for presentation-only frontmatter", cfg.Refused)
	}
}

func TestResolvePersonaConfigCrewOverrideWins(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	writeAgent(t, "captain",
		"permissions:\n  mode: acceptEdits\nremoteControl: true\nmodel: claude-opus-4-7\neffort: high\n")

	cfgYAML := "crew:\n" +
		"  captain:\n" +
		"    permissions:\n" +
		"      mode: plan\n" +
		"    remoteControl: custom-handle\n" +
		"    model: claude-haiku-4-5-20251001\n" +
		"    effort: max\n"
	if err := os.WriteFile(ConfigName, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePersonaConfig("captain")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if got.Mode != "plan" {
		t.Errorf("Mode = %q, want plan (override should win)", got.Mode)
	}
	if got.RemoteControl != "custom-handle" {
		t.Errorf("RemoteControl = %q, want custom-handle (override should win)", got.RemoteControl)
	}
	if got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want claude-haiku-4-5-20251001 (override should win)", got.Model)
	}
	if got.Effort != "max" {
		t.Errorf("Effort = %q, want max (override should win)", got.Effort)
	}
}

func TestResolvePersonaConfigBerthFrontmatter(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	// berth stays repo-supplied: it selects a shipmates-computed worktree path
	// under .shipmates/berths/<persona>, it does not name one. cwd does name
	// one, so it is operator-only.
	writeAgent(t, "captain", "berth: auto\n")
	writeUserPersonas(t, "personas:\n  captain:\n    cwd: custom/dir\n")

	cfg, err := ResolvePersonaConfig("captain")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if cfg.Berth != "auto" {
		t.Errorf("Berth = %q, want auto", cfg.Berth)
	}
	if cfg.CWD != "custom/dir" {
		t.Errorf("CWD = %q, want custom/dir", cfg.CWD)
	}
}

func TestResolvePersonaConfigBerthCrewOverride(t *testing.T) {
	isolateHome(t)
	t.Chdir(t.TempDir())
	writeAgent(t, "backend", "berth: off\n")
	cfgYAML := "crew:\n  backend:\n    berth: require\n"
	if err := os.WriteFile(ConfigName, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	writeUserPersonas(t, "personas:\n  backend:\n    cwd: /tmp/custom\n")

	got, err := ResolvePersonaConfig("backend")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if got.Berth != "require" {
		t.Errorf("Berth = %q, want require (override should win)", got.Berth)
	}
	if got.CWD != "/tmp/custom" {
		t.Errorf("CWD = %q, want /tmp/custom (operator file should win)", got.CWD)
	}
}

func TestPersonaConfigFingerprint(t *testing.T) {
	base := PersonaConfig{Mode: "ask", Model: "opus", Effort: "high"}
	if base.Fingerprint() != base.Fingerprint() {
		t.Fatal("Fingerprint not stable for identical config")
	}

	// Baked settings (model, effort) must change the fingerprint -> auto-fresh.
	mustDiffer := []PersonaConfig{
		{Mode: "ask", Model: "haiku", Effort: "high"},
		{Mode: "ask", Model: "opus", Effort: "low"},
	}
	for i, c := range mustDiffer {
		if c.Fingerprint() == base.Fingerprint() {
			t.Errorf("baked change %d did not alter fingerprint", i)
		}
	}

	// Per-invocation settings (mode, dangerouslySkipPermissions, remoteControl)
	// are passed every call, so they must NOT change the fingerprint (no fresh).
	// CWD/Berth are also excluded — the persona-berths guardrail. If they
	// entered the hash, gaining a berth would auto-fresh the very session it
	// means to preserve.
	mustMatch := []PersonaConfig{
		{Mode: "plan", Model: "opus", Effort: "high"},
		{Mode: "ask", Model: "opus", Effort: "high", DangerouslySkipPermissions: true},
		{Mode: "ask", Model: "opus", Effort: "high", RemoteControl: "x"},
		{Mode: "ask", Model: "opus", Effort: "high", Berth: "auto"},
		{Mode: "ask", Model: "opus", Effort: "high", CWD: ".shipmates/berths/captain"},
	}
	for i, c := range mustMatch {
		if c.Fingerprint() != base.Fingerprint() {
			t.Errorf("per-invocation change %d wrongly altered fingerprint", i)
		}
	}
}

func TestLaunchFlags(t *testing.T) {
	cfg := PersonaConfig{
		Mode:                       "acceptEdits",
		Model:                      "opus",
		Effort:                     "high",
		DangerouslySkipPermissions: true,
		RemoteControl:              "handle", // never a launch flag (caller handles it)
	}

	with := cfg.LaunchFlags(true)
	wantWith := []string{"--dangerously-skip-permissions", "--permission-mode", "acceptEdits", "--model", "opus", "--effort", "high"}
	if !reflect.DeepEqual(with, wantWith) {
		t.Errorf("LaunchFlags(true) = %v, want %v", with, wantWith)
	}

	// permission=false (live-server path) drops the permission knobs, keeps model/effort.
	without := cfg.LaunchFlags(false)
	wantWithout := []string{"--model", "opus", "--effort", "high"}
	if !reflect.DeepEqual(without, wantWithout) {
		t.Errorf("LaunchFlags(false) = %v, want %v", without, wantWithout)
	}

	// Empty config yields no flags.
	if f := (PersonaConfig{}).LaunchFlags(true); len(f) != 0 {
		t.Errorf("empty LaunchFlags(true) = %v, want none", f)
	}
}

func TestSessionMetaRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	if _, ok := ReadSessionMeta("captain"); ok {
		t.Fatal("ReadSessionMeta ok=true before any session exists")
	}

	if err := WriteSessionMeta("captain", "repo-captain", "uuid-1", "abc123", ""); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	meta, ok := ReadSessionMeta("captain")
	if !ok {
		t.Fatal("ReadSessionMeta ok=false after write")
	}
	if meta.Name != "repo-captain" || meta.ID != "uuid-1" || meta.ConfigHash != "abc123" {
		t.Fatalf("meta = %+v, want {repo-captain uuid-1 abc123}", meta)
	}

	// Legacy plain-name marker: read as name with empty hash (suppresses auto-fresh).
	if err := os.WriteFile(SessionMarker("legacy"), []byte("repo-legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lm, ok := ReadSessionMeta("legacy")
	if !ok {
		t.Fatal("legacy marker not read as existing")
	}
	if lm.Name != "repo-legacy" || lm.ConfigHash != "" {
		t.Fatalf("legacy meta = %+v, want name=repo-legacy, empty hash", lm)
	}
	// A pre-berth meta (no cwd field) reads back with empty CWD — the
	// resume path preserves "no cwd override" for existing sessions.
	if lm.CWD != "" {
		t.Errorf("legacy CWD = %q, want empty", lm.CWD)
	}

	// A berth-era meta round-trips CWD.
	if err := WriteSessionMeta("berthed", "repo-berthed", "uuid-2", "hash-2", ".shipmates/berths/berthed"); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	bm, ok := ReadSessionMeta("berthed")
	if !ok {
		t.Fatal("berthed meta not read")
	}
	if bm.CWD != ".shipmates/berths/berthed" {
		t.Errorf("berthed CWD = %q, want .shipmates/berths/berthed", bm.CWD)
	}

	// ResumeCWD reads it directly.
	if got := ResumeCWD("berthed"); got != ".shipmates/berths/berthed" {
		t.Errorf("ResumeCWD(berthed) = %q, want .shipmates/berths/berthed", got)
	}
	if got := ResumeCWD("legacy"); got != "" {
		t.Errorf("ResumeCWD(legacy) = %q, want empty (pre-berth)", got)
	}
}

func TestDeleteSessionMeta(t *testing.T) {
	t.Chdir(t.TempDir())

	// Deleting when nothing exists is not an error — auto-repair should be
	// idempotent so retries after a partial state don't fail.
	if err := DeleteSessionMeta("ghost"); err != nil {
		t.Fatalf("DeleteSessionMeta(missing): %v", err)
	}

	// Write a marker, delete it, confirm it's gone.
	if err := WriteSessionMeta("picard", "project-one-picard", "uuid-stale", "hash", ""); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	if _, ok := ReadSessionMeta("picard"); !ok {
		t.Fatal("marker not written")
	}
	if err := DeleteSessionMeta("picard"); err != nil {
		t.Fatalf("DeleteSessionMeta: %v", err)
	}
	if _, ok := ReadSessionMeta("picard"); ok {
		t.Fatal("marker still readable after delete")
	}

	// Second delete of the same persona is a no-op, not an error.
	if err := DeleteSessionMeta("picard"); err != nil {
		t.Fatalf("DeleteSessionMeta(second): %v", err)
	}
}

func TestNewUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := NewUUID()
		if !re.MatchString(u) {
			t.Fatalf("NewUUID() = %q, not a v4 UUID", u)
		}
		if seen[u] {
			t.Fatalf("NewUUID() returned duplicate %q", u)
		}
		seen[u] = true
	}
}

func TestAgentPath(t *testing.T) {
	if got := AgentPath("captain"); got != filepath.Join(AgentsDir, "captain.md") {
		t.Fatalf("AgentPath(captain) = %q", got)
	}
}
