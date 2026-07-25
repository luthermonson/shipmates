package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
)

// selectRuntime writes the project runtime config the memory-hook install
// path resolves against.
func selectRuntime(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(".shipmates", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".shipmates", "config.yaml"), []byte("runtime: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// memoryHookCount reports how many SessionStart hooks in
// .claude/settings.json run `shipmates hook load-memory`, counting only the
// nested matcher-group shape Claude Code actually executes. -1 means the
// settings file does not exist at all.
func memoryHookCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".claude", "settings.json"))
	if os.IsNotExist(err) {
		return -1
	}
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json invalid: %v\n%s", err, data)
	}
	n := 0
	for _, g := range settings.Hooks.SessionStart {
		for _, h := range g.Hooks {
			if strings.Contains(h.Command, "load-memory") {
				n++
			}
		}
	}
	return n
}

func hookTestCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/geordi/agent.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
	})
}

// TestAddInstallsMemoryHookForClaude is the regression for the gap this
// closes: InstallMemoryHook had no production caller, so a claude project
// got no .claude/settings.json and a real session ran with no durable
// memory injected. Adding a persona must install it, and doing so twice
// must not duplicate it.
func TestAddInstallsMemoryHookForClaude(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")
	cat := hookTestCatalog()

	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if got := memoryHookCount(t); got != 1 {
		t.Fatalf("memory hooks after add = %d, want 1", got)
	}
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("second addPersona: %v", err)
	}
	if got := memoryHookCount(t); got != 1 {
		t.Fatalf("memory hooks after a second add = %d, want 1 (must not duplicate)", got)
	}
}

// TestAddDoesNotWriteClaudeSettingsForCodex proves the hook is installed
// only for the runtime in use: a codex project must not grow a .claude/
// directory it will never read.
func TestAddDoesNotWriteClaudeSettingsForCodex(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "codex")

	if err := addPersona(hookTestCatalog(), "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if got := memoryHookCount(t); got != -1 {
		t.Fatalf("a codex project grew .claude/settings.json (hooks=%d)", got)
	}
	if _, err := os.Stat(".claude"); !os.IsNotExist(err) {
		t.Fatalf(".claude exists in a codex-only project: %v", err)
	}
}

// TestAddDefaultsToCodexWithNoRuntimeConfig keeps the documented default
// honest: with no config at all the project is codex, so nothing claude
// specific is written.
func TestAddDefaultsToCodexWithNoRuntimeConfig(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())

	if err := addPersona(hookTestCatalog(), "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if _, err := os.Stat(".claude"); !os.IsNotExist(err) {
		t.Fatalf(".claude written for the default (codex) runtime: %v", err)
	}
}

// TestInstallMemoryHookPreservesOperatorSettings proves the install merges
// into whatever the operator already has rather than replacing it.
func TestInstallMemoryHookPreservesOperatorSettings(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "claude")
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"model":"opus","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`
	if err := os.WriteFile(filepath.Join(".claude", "settings.json"), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := addPersona(hookTestCatalog(), "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, `"model": "opus"`) {
		t.Errorf("operator settings clobbered: %s", body)
	}
	if !strings.Contains(body, "echo mine") {
		t.Errorf("operator hook clobbered: %s", body)
	}
	if got := memoryHookCount(t); got != 1 {
		t.Errorf("memory hooks = %d, want 1", got)
	}
	if !strings.Contains(body, claude.MemoryHookCommand) {
		t.Errorf("hook command missing: %s", body)
	}
}

// TestUpdateReassertsMemoryHook proves `update` is enough to repair a
// project whose settings.json lost the hook, and to wire one that switched
// runtimes after init.
func TestUpdateReassertsMemoryHook(t *testing.T) {
	skipIfNoPolicyLock(t)
	t.Chdir(t.TempDir())
	selectRuntime(t, "codex")
	cat := hookTestCatalog()
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if _, err := os.Stat(".claude"); !os.IsNotExist(err) {
		t.Fatalf("codex project has .claude before the switch: %v", err)
	}

	selectRuntime(t, "claude")
	if err := runUpdate(cat, "", "theirs"); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if got := memoryHookCount(t); got != 1 {
		t.Fatalf("memory hooks after update = %d, want 1", got)
	}
	if err := runUpdate(cat, "", "theirs"); err != nil {
		t.Fatalf("second runUpdate: %v", err)
	}
	if got := memoryHookCount(t); got != 1 {
		t.Fatalf("memory hooks after a second update = %d, want 1", got)
	}
}
