package project

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
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
		Version: ManifestVersion,
		Files: map[string]string{
			".codex/agents/captain.toml":       SHA([]byte("body")),
			".shipmates/policies/captain.yaml": SHA([]byte("policy")),
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
	if m.Version != ManifestVersion {
		t.Fatalf("Version = %q, want %q", m.Version, ManifestVersion)
	}
}

func TestResolvePersonaConfigMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := ResolvePersonaConfig("ghost")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig missing persona: %v", err)
	}
	if !reflect.DeepEqual(cfg, PersonaConfig{}) {
		t.Fatalf("missing persona = %+v, want Codex default", cfg)
	}
}

func TestResolvePersonaConfigCodexOverrides(t *testing.T) {
	t.Chdir(t.TempDir())
	cfgYAML := "crew:\n" +
		"  captain:\n" +
		"    model: gpt-5\n" +
		"    effort: max\n"
	if err := os.WriteFile(ConfigName, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePersonaConfig("captain")
	if err != nil {
		t.Fatalf("ResolvePersonaConfig: %v", err)
	}
	if got.Model != "gpt-5" {
		t.Errorf("Model = %q, want gpt-5", got.Model)
	}
	if got.Effort != "max" {
		t.Errorf("Effort = %q, want max (override should win)", got.Effort)
	}
}

func TestPersonaConfigFingerprint(t *testing.T) {
	base := PersonaConfig{Model: "opus", Effort: "high"}
	if base.Fingerprint() != base.Fingerprint() {
		t.Fatal("Fingerprint not stable for identical config")
	}

	// Baked settings (model, effort) must change the fingerprint -> auto-fresh.
	mustDiffer := []PersonaConfig{
		{Model: "haiku", Effort: "high"},
		{Model: "opus", Effort: "low"},
	}
	for i, c := range mustDiffer {
		if c.Fingerprint() == base.Fingerprint() {
			t.Errorf("baked change %d did not alter fingerprint", i)
		}
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

func TestPersonaValidationAndDispatchLockIsolation(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, bad := range []string{"", "../security", "Security", "security/other", "."} {
		if err := ValidatePersonaName(bad); err == nil {
			t.Errorf("ValidatePersonaName(%q) succeeded", bad)
		}
	}
	releaseSecurity, err := AcquireDispatchLock("security")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecurity()
	if _, err := AcquireDispatchLock("security"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("second same-persona lock = %v", err)
	}
	releaseTester, err := AcquireDispatchLock("tester")
	if err != nil {
		t.Fatalf("different persona blocked: %v", err)
	}
	releaseTester()
}

func TestDispatchLockReclaimsVerifiedStalePID(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(SessionsDir(), "security.dispatch.lock")
	if err := os.WriteFile(path, []byte("2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireDispatchLock("security")
	if err != nil {
		t.Fatalf("reclaim stale lock: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("reclaimed lock = %q", raw)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("released lock still exists: %v", err)
	}
}

func TestDispatchLockRejectsMalformedContents(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(SessionsDir(), "security.dispatch.lock")
	for _, contents := range []string{"not-a-pid\n", "0\n", "12 13\n", "   \n"} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireDispatchLock("security"); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("contents %q error = %v", contents, err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != contents {
			t.Fatalf("malformed lock was changed: got %q want %q", raw, contents)
		}
	}
}

func TestDispatchLockRefusesLivePIDWithoutKernelOwner(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(SessionsDir(), "security.dispatch.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireDispatchLock("security"); err == nil || !strings.Contains(err.Error(), "live PID") {
		t.Fatalf("live PID error = %v", err)
	}
}

func TestWriteBackendSessionMetaIsPrivate(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := WriteBackendSessionMeta("security", "codex", "thread-1", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(BackendSessionMarker("security", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker permissions = %o", got)
	}
}

func TestResolvePersonaConfigDefaultsToCodex(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(CodexAgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CodexAgentPath("backend"), []byte("developer_instructions = \"role\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ResolvePersonaConfig("backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, PersonaConfig{}) {
		t.Fatalf("default config = %+v", cfg)
	}

	cfg, err = ResolvePersonaConfig("backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, PersonaConfig{}) {
		t.Fatalf("Codex-only config = %+v", cfg)
	}

}

func TestResolvePersonaConfigRejectsIncompatibleSettings(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"backend field", "backend: command", "field backend not found"},
		{"command field", "command: [tool]", "field command not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.WriteFile(ConfigName, []byte("crew:\n  security:\n    "+tt.body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ResolvePersonaConfig("security")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestInstalledPersonaRequiresSafeValidCodexArtifact(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(CodexAgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := CodexAgentPath("security")
	for _, content := range []string{"", "name = \"security\"\n", "developer_instructions = \"   \"\n", "developer_instructions = nope\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := InstalledPersonaPath("security"); err == nil {
			t.Fatalf("invalid artifact accepted: %q", content)
		}
	}
	if err := os.WriteFile(path, []byte("developer_instructions = \"Review safely.\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstalledPersonaPath("security"); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/outside-persona.toml", path); err != nil {
		t.Fatal(err)
	}
	if _, err := InstalledPersonaPath("security"); err == nil || !strings.Contains(err.Error(), "unsafe Codex artifact") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestFindRootFromNestedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigName), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "docs", "guide")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("FindRoot() = %q, want %q", got, want)
	}
}
