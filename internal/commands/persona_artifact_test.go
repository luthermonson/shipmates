package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
)

// personaArtifactCatalog is a one-persona catalog whose body is caller
// supplied, so a test can move the catalog under an installed project the
// way a shipmates upgrade does.
func personaArtifactCatalog(body string) *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/geordi/agent.md": {Data: []byte(
			"---\nname: geordi\ndescription: Warp core diagnostics.\nbyline: \"Geordi here,\"\n" +
				"memoryDir: .shipmates/memory/geordi\npermissions:\n  mode: ask\n---\n\n# Role\n\n" + body + "\n")},
	})
}

func readAgent(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(claude.AgentPath("geordi"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

// TestAddInstallsClaudeAgentForClaudeRuntime is the regression for the gap
// this closes. InstallPersona had no production caller, so a claude project
// had no .claude/agents/<persona>.md and `claude --agent <persona>` found no
// agent definition to load: the turn ran as a generic session with none of
// the persona's role, instructions or constraints.
func TestAddInstallsClaudeAgentForClaudeRuntime(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")

	if err := addPersona(personaArtifactCatalog("Diagnose the warp core."), "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	body, ok := readAgent(t)
	if !ok {
		t.Fatal("no .claude/agents/geordi.md was installed")
	}
	for _, want := range []string{"name: geordi", "description: Warp core diagnostics.", "# Role", "Diagnose the warp core."} {
		if !strings.Contains(body, want) {
			t.Errorf("agent file missing %q; got:\n%s", want, body)
		}
	}
	// Shipmates-only frontmatter has no meaning to Claude Code.
	for _, unwanted := range []string{"byline", "memoryDir", "permissions"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("shipmates-only field %q leaked into the agent file:\n%s", unwanted, body)
		}
	}
	// The canonical Codex artifact is shipmates' persona inventory and is
	// installed on every runtime.
	if _, err := os.Stat(project.CodexAgentPath("geordi")); err != nil {
		t.Errorf("canonical Codex artifact missing on the claude runtime: %v", err)
	}
	// It must be manifest-tracked, or update and remove cannot reason about it.
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, tracked := m.Files[claude.AgentPath("geordi")]; !tracked {
		t.Errorf("claude agent not recorded in the manifest: %v", m.Files)
	}
}

// TestAddDoesNotInstallClaudeAgentForCodexRuntime proves only the runtime in
// use gets its artifact: a codex project must not grow a .claude/ directory
// it will never read.
func TestAddDoesNotInstallClaudeAgentForCodexRuntime(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "codex")

	if err := addPersona(personaArtifactCatalog("Diagnose the warp core."), "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if _, err := os.Stat(project.CodexAgentPath("geordi")); err != nil {
		t.Fatalf("codex artifact missing: %v", err)
	}
	if body, ok := readAgent(t); ok {
		t.Fatalf("a codex project grew a claude agent file:\n%s", body)
	}
	if _, err := os.Stat(".claude"); !os.IsNotExist(err) {
		t.Errorf(".claude exists in a codex-only project: %v", err)
	}
}

// TestUpdateInstallsUpdatesAndIsIdempotent covers the three states `update`
// has to get right for the new artifact: install it when the project just
// switched runtimes, move it when the catalog moves, and report nothing on a
// second run.
func TestUpdateInstallsUpdatesAndIsIdempotent(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	// Installed as a codex project first — no claude artifact yet.
	selectRuntime(t, "codex")
	if err := addPersona(personaArtifactCatalog("old duties"), "geordi"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAgent(t); ok {
		t.Fatal("claude artifact written for a codex project")
	}

	// The operator switches the project to claude; `update` is how that is
	// picked up.
	selectRuntime(t, "claude")
	if err := runUpdate(personaArtifactCatalog("old duties"), "geordi", "theirs"); err != nil {
		t.Fatal(err)
	}
	body, ok := readAgent(t)
	if !ok {
		t.Fatal("update did not install the claude artifact after a runtime switch")
	}
	if !strings.Contains(body, "old duties") {
		t.Fatalf("agent body wrong:\n%s", body)
	}

	// A moved catalog reaches the claude artifact too.
	if err := runUpdate(personaArtifactCatalog("new duties"), "geordi", "theirs"); err != nil {
		t.Fatal(err)
	}
	body, _ = readAgent(t)
	if !strings.Contains(body, "new duties") {
		t.Fatalf("catalog change did not reach the claude artifact:\n%s", body)
	}

	// Idempotent: a second identical update changes nothing on disk.
	before, err := os.ReadFile(claude.AgentPath("geordi"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(personaArtifactCatalog("new duties"), "geordi", "theirs"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(claude.AgentPath("geordi"))
	if string(after) != string(before) {
		t.Errorf("second update rewrote the artifact:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// TestUpdatePreservesAHandEditedClaudeAgent mirrors what the codex artifact
// already guarantees: an operator who tunes their persona keeps their edits,
// and a non-interactive conflict is never resolved by clobbering them.
func TestUpdatePreservesAHandEditedClaudeAgent(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")
	if err := addPersona(personaArtifactCatalog("old duties"), "geordi"); err != nil {
		t.Fatal(err)
	}
	edited := "---\nname: geordi\n---\n\n# Role\n\nmy own instructions\n"
	if err := os.WriteFile(claude.AgentPath("geordi"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	// --accept ours is the non-interactive "keep mine".
	if err := runUpdate(personaArtifactCatalog("new duties"), "geordi", "ours"); err != nil {
		t.Fatal(err)
	}
	body, _ := readAgent(t)
	if body != edited {
		t.Errorf("hand-edited claude agent was clobbered:\n%s", body)
	}

	// …and --accept theirs is the explicit opt-in to the shipped version.
	if err := runUpdate(personaArtifactCatalog("new duties"), "geordi", "theirs"); err != nil {
		t.Fatal(err)
	}
	body, _ = readAgent(t)
	if !strings.Contains(body, "new duties") {
		t.Errorf("--accept theirs did not install the shipped agent:\n%s", body)
	}
}

// TestRemoveDeletesTheClaudeAgentAndKeepsMemory covers the inverse of
// install. Memory is preserved for the same reason codex removal preserves
// it: it is the persona's durable knowledge, not an installed artifact.
func TestRemoveDeletesTheClaudeAgentAndKeepsMemory(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")
	if err := addPersona(personaArtifactCatalog("duties"), "geordi"); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(project.MemoryDir("geordi"), "notes.md")
	if err := os.WriteFile(memory, []byte("durable knowledge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAgent(t); !ok {
		t.Fatal("precondition: no claude agent installed")
	}

	if err := runRemove("geordi", false, false); err != nil {
		t.Fatalf("runRemove: %v", err)
	}
	if body, ok := readAgent(t); ok {
		t.Errorf("claude agent survived removal:\n%s", body)
	}
	if _, err := os.Stat(project.CodexAgentPath("geordi")); !os.IsNotExist(err) {
		t.Errorf("codex artifact survived removal: %v", err)
	}
	got, err := os.ReadFile(memory)
	if err != nil || string(got) != "durable knowledge\n" {
		t.Errorf("memory not preserved: %q %v", got, err)
	}
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, tracked := m.Files[claude.AgentPath("geordi")]; tracked {
		t.Errorf("removed claude agent still tracked in the manifest: %v", m.Files)
	}
}

// TestRemoveRefusesWhenTheClaudeAgentWasEdited holds the claude artifact to
// the same rule as every other managed target: shipmates does not destroy
// work it did not write.
func TestRemoveRefusesWhenTheClaudeAgentWasEdited(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")
	if err := addPersona(personaArtifactCatalog("duties"), "geordi"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claude.AgentPath("geordi"), []byte("---\nname: geordi\n---\n\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runRemove("geordi", false, false)
	if err == nil {
		t.Fatal("expected removal to be refused for a modified claude agent")
	}
	if !strings.Contains(err.Error(), "modified") {
		t.Errorf("error = %v, want it to name the modification", err)
	}
	if _, ok := readAgent(t); !ok {
		t.Error("the refused removal deleted the file anyway")
	}
	if _, err := os.Stat(project.CodexAgentPath("geordi")); err != nil {
		t.Errorf("a refused removal partially deleted the persona: %v", err)
	}
}

// TestRemoveIgnoresAnUntrackedClaudeAgent proves the preflight only claims
// files shipmates installed. A .claude/agents/<persona>.md the operator
// wrote themselves in a codex project is neither deleted nor allowed to
// block the removal.
func TestRemoveIgnoresAnUntrackedClaudeAgent(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "codex")
	if err := addPersona(personaArtifactCatalog("duties"), "geordi"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(claude.AgentPath("geordi")), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "---\nname: geordi\n---\n\nhand written, never installed\n"
	if err := os.WriteFile(claude.AgentPath("geordi"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runRemove("geordi", false, false); err != nil {
		t.Fatalf("an untracked file blocked removal: %v", err)
	}
	body, ok := readAgent(t)
	if !ok || body != mine {
		t.Errorf("shipmates touched a file it never installed: ok=%v body=%q", ok, body)
	}
}
