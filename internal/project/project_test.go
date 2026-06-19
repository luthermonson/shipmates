package project

import (
	"os"
	"path/filepath"
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
			".claude/agents/bosun.md": SHA([]byte("body")),
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
		{"with prefix", "sessionPrefix: myrepo\n", "bosun", "myrepo-bosun"},
		{"empty prefix", "sessionPrefix: \"\"\n", "bosun", "bosun"},
		{"no config file", "", "navigator", "navigator"},
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
	t.Chdir(t.TempDir())
	cfg, err := ResolvePersonaConfig("ghost")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig missing persona: %v", err)
	}
	if cfg != (PersonaConfig{}) {
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
	t.Chdir(t.TempDir())
	writeAgent(t, "bosun",
		"permissions:\n  mode: acceptEdits\nremoteControl: true\ndangerouslySkipPermissions: true\nmodel: claude-opus-4-7\n")

	cfg, err := ResolvePersonaConfig("bosun")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if cfg.Mode != "acceptEdits" {
		t.Errorf("Mode = %q, want acceptEdits", cfg.Mode)
	}
	if cfg.RemoteControl != "bosun" {
		t.Errorf("RemoteControl = %q, want bosun", cfg.RemoteControl)
	}
	if !cfg.DangerouslySkipPermissions {
		t.Error("DangerouslySkipPermissions = false, want true")
	}
	if cfg.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", cfg.Model)
	}
}

func TestResolvePersonaConfigCrewOverrideWins(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "bosun",
		"permissions:\n  mode: acceptEdits\nremoteControl: true\ndangerouslySkipPermissions: true\nmodel: claude-opus-4-7\n")

	cfgYAML := "crew:\n" +
		"  bosun:\n" +
		"    permissions:\n" +
		"      mode: plan\n" +
		"    remoteControl: custom-handle\n" +
		"    dangerouslySkipPermissions: false\n" +
		"    model: claude-haiku-4-5-20251001\n"
	if err := os.WriteFile(ConfigName, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePersonaConfig("bosun")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if got.Mode != "plan" {
		t.Errorf("Mode = %q, want plan (override should win)", got.Mode)
	}
	if got.RemoteControl != "custom-handle" {
		t.Errorf("RemoteControl = %q, want custom-handle (override should win)", got.RemoteControl)
	}
	if got.DangerouslySkipPermissions {
		t.Error("DangerouslySkipPermissions = true, want false (override should win)")
	}
	if got.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want claude-haiku-4-5-20251001 (override should win)", got.Model)
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
	if got := AgentPath("bosun"); got != filepath.Join(AgentsDir, "bosun.md") {
		t.Fatalf("AgentPath(bosun) = %q", got)
	}
}
