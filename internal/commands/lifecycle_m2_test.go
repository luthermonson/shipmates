package commands

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestAddPreservesExistingCodexAgentByM2BaselineRules(t *testing.T) {
	t.Run("untracked", func(t *testing.T) {
		t.Chdir(t.TempDir())
		path := project.CodexAgentPath("security")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		local := []byte("developer_instructions = \"untracked local\"\n")
		if err := os.WriteFile(path, local, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := addPersona(lifecycleCatalog("shipped role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, local) {
			t.Fatalf("untracked agent overwritten: %q", got)
		}
		m, _ := project.LoadManifest()
		if _, tracked := m.Files[path]; tracked {
			t.Fatal("preserved untracked agent was silently adopted")
		}
	})

	t.Run("locally modified", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := addPersona(lifecycleCatalog("old role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		path := project.CodexAgentPath("security")
		local := []byte("developer_instructions = \"tracked local edit\"\n")
		if err := os.WriteFile(path, local, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := addPersona(lifecycleCatalog("new shipped role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, local) {
			t.Fatalf("modified agent overwritten: %q", got)
		}
		m, _ := project.LoadManifest()
		if m.Files[path] == project.SHA(local) {
			t.Fatal("local edit replaced the shipped manifest baseline")
		}
	})

	t.Run("clean tracked baseline advances", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := addPersona(lifecycleCatalog("old role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		if err := addPersona(lifecycleCatalog("new role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		path := project.CodexAgentPath("security")
		got, _ := os.ReadFile(path)
		if !strings.Contains(string(got), "new role") {
			t.Fatalf("clean baseline not advanced: %q", got)
		}
		m, _ := project.LoadManifest()
		if m.Files[path] != project.SHA(got) {
			t.Fatal("advanced agent baseline not saved")
		}
	})
}

func lifecycleCatalog(role, policy string) *catalog.Catalog {
	fsys := fstest.MapFS{
		"catalog/security/agent.md":              {Data: []byte("---\nname: security\ndescription: Security.\n---\n\n" + role + "\n")},
		"catalog/security/memory-seeds/notes.md": {Data: []byte("seed\n")},
	}
	if policy != "" {
		fsys["catalog/security/policy.yaml"] = &fstest.MapFile{Data: []byte(policy)}
	}
	return catalog.New(fsys)
}

func lifecyclePolicy(command string) string {
	return "version: 1\nallow:\n  - id: test.allow\n    kind: process.exec\n    match:\n      command_exact: \"" + command + "\"\n    reason: test policy\nask: []\ndeny: []\n"
}

func TestCodexUpdatePreservesMemorySessionsAndLegacyLegacyRuntime(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := addPersona(lifecycleCatalog("old role", lifecyclePolicy("old")), "security"); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(project.MemoryDir("security"), "notes.md")
	if err := os.WriteFile(memory, []byte("durable knowledge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "codex", "thread", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	sessionBefore, _ := os.ReadFile(project.BackendSessionMarker("security", "codex"))
	if err := os.MkdirAll(filepath.Join(".legacy-runtime", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(".legacy-runtime", "agents", "security.md")
	if err := os.WriteFile(legacy, []byte("legacy user file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runUpdate(lifecycleCatalog("new role", lifecyclePolicy("new")), "security", "theirs"); err != nil {
		t.Fatal(err)
	}
	agent, err := os.ReadFile(project.CodexAgentPath("security"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agent), "new role") {
		t.Fatalf("Codex agent not updated: %s", agent)
	}
	policy, err := os.ReadFile(project.PolicyPath("security"))
	if err != nil {
		t.Fatal(err)
	}
	if string(policy) != lifecyclePolicy("new") {
		t.Fatalf("policy = %q", policy)
	}
	gotMemory, _ := os.ReadFile(memory)
	if string(gotMemory) != "durable knowledge\n" {
		t.Fatalf("memory changed: %q", gotMemory)
	}
	sessionAfter, _ := os.ReadFile(project.BackendSessionMarker("security", "codex"))
	if string(sessionAfter) != string(sessionBefore) {
		t.Fatal("session marker changed")
	}
	legacyAfter, _ := os.ReadFile(legacy)
	if string(legacyAfter) != "legacy user file\n" {
		t.Fatal("legacy LegacyRuntime file changed")
	}
}

func TestCodexUpdateConflictOursTheirsAndSidecar(t *testing.T) {
	t.Run("ours then theirs", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := addPersona(lifecycleCatalog("old role", ""), "security"); err != nil {
			t.Fatal(err)
		}
		path := project.CodexAgentPath("security")
		if err := os.WriteFile(path, []byte("developer_instructions = \"local edit\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := runUpdate(lifecycleCatalog("new role", ""), "security", "ours"); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), "local edit") {
			t.Fatal("accept ours overwrote local edit")
		}
		if err := runUpdate(lifecycleCatalog("new role", ""), "security", "theirs"); err != nil {
			t.Fatal(err)
		}
		raw, _ = os.ReadFile(path)
		if !strings.Contains(string(raw), "new role") {
			t.Fatal("accept theirs did not install catalog version")
		}
	})

	t.Run("sidecar", func(t *testing.T) {
		t.Chdir(t.TempDir())
		path := project.CodexAgentPath("security")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		baseline := []byte("developer_instructions = \"old\"\n")
		if err := os.WriteFile(path, []byte("developer_instructions = \"local\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := &project.Manifest{Version: project.ManifestVersion, Files: map[string]string{path: project.SHA(baseline)}}
		st := &updateState{in: bufio.NewScanner(strings.NewReader("")), stickyAll: true, stickyRes: resSidecar}
		shipped := []byte("developer_instructions = \"new\"\n")
		if err := reconcileFile(m, st, path, shipped, "Codex agent"); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), "local") {
			t.Fatal("sidecar changed original")
		}
		side, err := os.ReadFile(path + ".new")
		if err != nil || string(side) != string(shipped) {
			t.Fatalf("sidecar = %q, %v", side, err)
		}
	})
}

func TestRemovePreflightIsAtomicAndPreservesState(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := lifecycleCatalog("role", lifecyclePolicy("safe"))
	if err := addPersona(cat, "security"); err != nil {
		t.Fatal(err)
	}
	agent := project.CodexAgentPath("security")
	policy := project.PolicyPath("security")
	if err := os.WriteFile(policy, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRemove("security", false); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("remove error = %v", err)
	}
	if _, err := os.Stat(agent); err != nil {
		t.Fatalf("agent partially removed: %v", err)
	}
	if err := os.WriteFile(policy, []byte(lifecyclePolicy("safe")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "codex", "thread", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".legacy-runtime", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(".legacy-runtime", "agents", "security.md")
	if err := os.WriteFile(legacy, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runRemove("security", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{agent, policy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("managed target remains %s: %v", path, err)
		}
	}
	if _, err := os.Stat(project.MemoryDir("security")); err != nil {
		t.Fatalf("memory removed: %v", err)
	}
	if _, err := os.Stat(project.BackendSessionMarker("security", "codex")); err != nil {
		t.Fatalf("session removed: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy LegacyRuntime file removed: %v", err)
	}
}

func TestRemoveRefusesUntrackedManagedTargetBeforeDeletingAny(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := addPersona(lifecycleCatalog("role", ""), "security"); err != nil {
		t.Fatal(err)
	}
	agent := project.CodexAgentPath("security")
	policy := project.PolicyPath("security")
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	delete(m.Files, policy)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runRemove("security", false); err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatalf("remove error = %v", err)
	}
	if _, err := os.Stat(agent); err != nil {
		t.Fatalf("agent partially removed: %v", err)
	}
	if _, err := os.Stat(policy); err != nil {
		t.Fatalf("untracked policy removed: %v", err)
	}
}

func TestRemoveRollsBackLaterDeletionFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := addPersona(lifecycleCatalog("role", lifecyclePolicy("safe")), "security"); err != nil {
		t.Fatal(err)
	}
	agent, policy := project.CodexAgentPath("security"), project.PolicyPath("security")
	agentBefore, _ := os.ReadFile(agent)
	policyBefore, _ := os.ReadFile(policy)
	manifestBefore, _ := os.ReadFile(project.ManifestPath())

	original := personaRemoveOps
	t.Cleanup(func() { personaRemoveOps = original })
	calls := 0
	personaRemoveOps.stage = func(old, new string) error {
		calls++
		if calls == 2 {
			return errors.New("injected deletion failure")
		}
		return original.stage(old, new)
	}
	if err := runRemove("security", false); err == nil || !strings.Contains(err.Error(), "injected deletion failure") {
		t.Fatalf("remove error = %v", err)
	}
	assertRemovalStateRestored(t, agent, agentBefore, policy, policyBefore, manifestBefore)
}

func TestRemoveRollsBackManifestSaveFailureAndPreservesDurableState(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := addPersona(lifecycleCatalog("role", lifecyclePolicy("safe")), "security"); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "codex", "thread", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	agent, policy := project.CodexAgentPath("security"), project.PolicyPath("security")
	agentBefore, _ := os.ReadFile(agent)
	policyBefore, _ := os.ReadFile(policy)
	manifestBefore, _ := os.ReadFile(project.ManifestPath())

	original := personaRemoveOps
	t.Cleanup(func() { personaRemoveOps = original })
	personaRemoveOps.save = func(*project.Manifest) error { return errors.New("injected manifest save failure") }
	if err := runRemove("security", false); err == nil || !strings.Contains(err.Error(), "injected manifest save failure") {
		t.Fatalf("remove error = %v", err)
	}
	assertRemovalStateRestored(t, agent, agentBefore, policy, policyBefore, manifestBefore)
	if _, err := os.Stat(project.MemoryDir("security")); err != nil {
		t.Fatalf("memory not preserved: %v", err)
	}
	if _, err := os.Stat(project.BackendSessionMarker("security", "codex")); err != nil {
		t.Fatalf("session not preserved: %v", err)
	}
}

func assertRemovalStateRestored(t *testing.T, agent string, agentBefore []byte, policy string, policyBefore, manifestBefore []byte) {
	t.Helper()
	for path, want := range map[string][]byte{agent: agentBefore, policy: policyBefore, project.ManifestPath(): manifestBefore} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s not restored: %q, %v", path, got, err)
		}
	}
	for _, parent := range []string{filepath.Dir(agent), filepath.Dir(policy)} {
		matches, err := filepath.Glob(filepath.Join(parent, ".shipmates-remove-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("removal staging remains in %s: %v, %v", parent, matches, err)
		}
	}
}

func TestRemovePurgeDeletesOnlyPersonaMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := addPersona(lifecycleCatalog("role", ""), "security"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project.MemoryDir("tester"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.MemoryDir("tester"), "keep.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "codex", "thread", "thread-1", "hash"); err != nil {
		t.Fatal(err)
	}
	if err := runRemove("security", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(project.MemoryDir("security")); !os.IsNotExist(err) {
		t.Fatalf("security memory remains: %v", err)
	}
	if _, err := os.Stat(project.MemoryDir("tester")); err != nil {
		t.Fatalf("tester memory removed: %v", err)
	}
	if _, err := os.Stat(project.BackendSessionMarker("security", "codex")); err != nil {
		t.Fatalf("session removed by purge: %v", err)
	}
}
