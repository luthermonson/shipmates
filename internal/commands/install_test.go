package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
)

// skipIfNoPolicyLock skips tests that require withPolicyWriteLock. Secure
// policy mutation locking exists on unix (flock, policylock_unix.go) and on
// Windows (LockFileEx, policylock_windows.go); it is gated on the same
// capability flag the loader publishes so a platform with neither still
// skips rather than fails.
func skipIfNoPolicyLock(t *testing.T) {
	t.Helper()
	if !policy.SecureLoadSupported() {
		t.Skip("secure policy mutation locking is unsupported on this platform")
	}
}

// skipIfNoRoutingTransactions skips tests that drive `routing apply`, which
// commits through an atomic directory-entry exchange (renameat2
// RENAME_EXCHANGE on Linux, renamex_np on Darwin). Windows and the remaining
// unix platforms have no equivalent primitive, and that gap is independent of
// policy locking — see internal/project/routing_exchange_*.go.
func skipIfNoRoutingTransactions(t *testing.T) {
	t.Helper()
	if !project.RoutingTransactionsSupported() {
		t.Skip("atomic routing transactions are unsupported on this platform")
	}
}

func TestComposeAgent(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/routing/github.md": {Data: []byte("## GitHub routing conventions\nROUTING BLOCK\n")},
	})
	base := []byte("---\nname: geordi\n---\n\n# Role\nbody\n")

	t.Run("no routing leaves base unchanged", func(t *testing.T) {
		t.Chdir(t.TempDir())
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Errorf("composeAgent changed base when no routing declared:\n%s", out)
		}
	})

	t.Run("routing appends a marked block", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if !strings.HasPrefix(s, "---\nname: geordi\n") {
			t.Error("base content not preserved at the top")
		}
		if !strings.Contains(s, "ROUTING BLOCK") {
			t.Error("routing block not appended")
		}
		if !strings.Contains(s, "<!-- shipmates:routing:github -->") || !strings.Contains(s, "<!-- /shipmates:routing:github -->") {
			t.Error("routing markers missing")
		}
	})

	t.Run("unknown routing name is a no-op", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Error("unknown routing name should leave base unchanged")
		}
	})
}

func TestAddPersonaReplacesLegacyCatalogPolicyWithM5Policy(t *testing.T) {
	skipIfNoPolicyLock(t)
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/agent.md":    {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
		"catalog/geordi/policy.yaml": {Data: []byte("allow:\n  - Bash(git status)\ndeny:\n  - Bash(rm -rf /)\n")},
	})
	t.Chdir(t.TempDir())
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	// Policy should have landed under .shipmates/policies/.
	polPath := filepath.Join(".shipmates", "policies", "geordi.yaml")
	b, err := os.ReadFile(polPath)
	if err != nil {
		t.Fatalf("expected vendored policy at %s: %v", polPath, err)
	}
	if string(b) != emptyStrictPolicy {
		t.Errorf("persona policy = %q, want strict M5 empty policy", b)
	}
	if _, err := os.Stat(project.CodexAgentPath("geordi")); err != nil {
		t.Fatalf("expected Codex agent at %s: %v", project.CodexAgentPath("geordi"), err)
	}
	if _, err := os.Stat(".legacy-runtime"); !os.IsNotExist(err) {
		t.Fatalf("add created .legacy-runtime: %v", err)
	}
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != project.ManifestVersion {
		t.Fatalf("manifest version = %q", m.Version)
	}
	if len(m.Files) != 2 || m.Files[project.CodexAgentPath("geordi")] == "" || m.Files[project.PolicyPath("geordi")] == "" {
		t.Fatalf("manifest files = %#v, want only Codex agent and policy", m.Files)
	}
}

func TestAddPersonaWithoutCatalogPolicyInstallsFailClosedOverlay(t *testing.T) {
	skipIfNoPolicyLock(t)
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/agent.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
	})
	t.Chdir(t.TempDir())
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(".shipmates", "policies", "geordi.yaml"))
	if err != nil {
		t.Fatalf("required persona policy missing: %v", err)
	}
	if string(b) != emptyStrictPolicy {
		t.Fatalf("persona policy = %q", b)
	}
}

func TestAddPersonaMigratesManagedLegacyPolicyButPreservesModified(t *testing.T) {
	skipIfNoPolicyLock(t)
	legacy := []byte("allow:\n  - Bash(git status)\nask: []\ndeny: []\n")
	cat := catalog.New(fstest.MapFS{
		"catalog/captain/agent.md":    {Data: []byte("---\nname: captain\n---\n\nLead.\n")},
		"catalog/captain/policy.yaml": {Data: legacy},
	})

	for _, tc := range []struct {
		name, disk string
		want       []byte
	}{
		{name: "managed legacy migrates", disk: string(legacy), want: []byte(emptyStrictPolicy)},
		{name: "modified legacy is preserved", disk: string(legacy) + "# user change\n", want: []byte(string(legacy) + "# user change\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := addPersona(cat, "captain"); err != nil {
				t.Fatal(err)
			}
			path := project.PolicyPath("captain")
			if err := os.WriteFile(path, []byte(tc.disk), 0o644); err != nil {
				t.Fatal(err)
			}
			m, err := project.LoadManifest()
			if err != nil {
				t.Fatal(err)
			}
			m.Files[path] = project.SHA(legacy)
			if err := m.Save(); err != nil {
				t.Fatal(err)
			}

			if err := addPersona(cat, "captain"); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tc.want) {
				t.Fatalf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCatalogM5PolicyAcceptsOnlyStrictV1(t *testing.T) {
	valid := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	for _, tc := range []struct {
		name string
		body []byte
		want []byte
	}{
		{name: "valid v1", body: valid, want: valid},
		{name: "legacy", body: []byte("allow: []\nask: []\ndeny: []\n"), want: []byte(emptyStrictPolicy)},
		{name: "future version", body: []byte("version: 2\nallow: []\nask: []\ndeny: []\n"), want: []byte(emptyStrictPolicy)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := catalog.New(fstest.MapFS{"catalog/p/policy.yaml": {Data: tc.body}})
			got, err := catalogM5Policy(cat, "p")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tc.want) {
				t.Fatalf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpdateMigratesManagedLegacyCatalogPolicy(t *testing.T) {
	skipIfNoPolicyLock(t)
	legacy := []byte("allow: [Bash(git status)]\nask: []\ndeny: []\n")
	cat := catalog.New(fstest.MapFS{
		"catalog/captain/agent.md":    {Data: []byte("---\nname: captain\n---\n\nLead.\n")},
		"catalog/captain/policy.yaml": {Data: legacy},
	})
	t.Chdir(t.TempDir())
	if err := addPersona(cat, "captain"); err != nil {
		t.Fatal(err)
	}
	path := project.PolicyPath("captain")
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	m.Files[path] = project.SHA(legacy)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(cat, "captain", "ours"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != emptyStrictPolicy {
		t.Fatalf("policy = %q, want strict M5 empty policy", got)
	}
}

func TestAddPersonaPreservesMemoryAndCodexSession(t *testing.T) {
	skipIfNoPolicyLock(t)
	cat := catalog.New(fstest.MapFS{
		"catalog/security/agent.md":              {Data: []byte("---\nname: security\n---\n\nReview security.\n")},
		"catalog/security/memory-seeds/notes.md": {Data: []byte("seed\n")},
	})
	t.Chdir(t.TempDir())
	if err := addPersona(cat, "security"); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(project.MemoryDir("security"), "notes.md")
	if err := os.WriteFile(notes, []byte("user knowledge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.MemoryDir("security"), "extra.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "codex", "thread-1", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(project.BackendSessionMarker("security", "codex"))

	if err := addPersona(cat, "security"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "user knowledge\n" {
		t.Fatalf("memory overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(project.MemoryDir("security"), "extra.md")); err != nil {
		t.Fatalf("existing memory removed: %v", err)
	}
	after, err := os.ReadFile(project.BackendSessionMarker("security", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("Codex session marker changed during add")
	}
}

func TestPersonaInstallStatusUsesOnlyValidCodexArtifacts(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(".legacy-runtime", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".legacy-runtime", "agents", "security.md"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := personaInstallStatus("security"); got != "" {
		t.Fatalf("LegacyRuntime-only status = %q", got)
	}

	if err := os.MkdirAll(project.CodexAgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.CodexAgentPath("security"), []byte("developer_instructions = \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := personaInstallStatus("security"); got != "invalid-codex" {
		t.Fatalf("invalid Codex status = %q", got)
	}
	if err := os.WriteFile(project.CodexAgentPath("security"), []byte("developer_instructions = \"Review.\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := personaInstallStatus("security"); got != "installed" {
		t.Fatalf("valid Codex status = %q", got)
	}
}

func TestPersonaInstallStatusDoesNotTouchManifest(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(project.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("legacy manifest bytes\n")
	if err := os.WriteFile(project.ManifestPath(), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = personaInstallStatus("security")
	got, err := os.ReadFile(project.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(manifest) {
		t.Fatal("list discovery modified manifest")
	}
}
