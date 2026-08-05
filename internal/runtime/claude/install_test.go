package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// pinHome points os.UserHomeDir at a fresh temp dir so tests never read the
// developer's real ~/.shipmates/config.yaml — the brig posture under test is
// always the default (enabled) unless a test writes its own config.
func pinHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOME", home)        // Unix
	return home
}

// TestRenderPersona_ArticlesBlockFollowsBrigConfig proves the prompt layer
// obeys the operator's switch: enabled (default) splices the reminder,
// disabled removes it, and re-rendering is idempotent either way.
func TestRenderPersona_ArticlesBlockFollowsBrigConfig(t *testing.T) {
	home := pinHome(t)
	spec := runtime.PersonaSpec{Name: "backend", SystemPrompt: "# Role\n\nBody.\n"}

	rendered, err := RenderPersona(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), brig.PromptStartMarker) {
		t.Fatalf("default posture: artifact lacks the Articles reminder:\n%s", rendered)
	}
	again, err := RenderPersona(runtime.PersonaSpec{Name: "backend", SystemPrompt: string(rendered[strings.Index(string(rendered), "# Role"):])})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(again), brig.PromptStartMarker) != 1 {
		t.Errorf("re-rendering a body that already carries the block must not stack a second copy:\n%s", again)
	}

	// Disable the brig in the operator's config: the block disappears.
	confDir := filepath.Join(home, ".shipmates")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.yaml"), []byte("brig:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, err := RenderPersona(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), brig.PromptStartMarker) {
		t.Fatalf("disabled brig: artifact still carries the Articles reminder:\n%s", off)
	}
}

func TestInstallPersona_WritesFileWithFrontmatter(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	spec := runtime.PersonaSpec{
		Name:         "architect",
		Description:  "Cross-cutting design review.",
		Model:        "sonnet",
		Capabilities: []string{"read", "edit", "bash"},
		SystemPrompt: "# Custom prompt\n\nBody text.\n",
	}
	if err := rt.InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatalf("install: %v", err)
	}
	path := filepath.Join(proj, ".claude", "agents", "architect.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(data)
	if !strings.HasPrefix(body, "---\n") {
		t.Errorf("missing frontmatter start")
	}
	// "read"/"edit"/"bash" are canonical shipmates capabilities; the artifact
	// must carry the Claude Code tool names they translate to.
	for _, want := range []string{"name: architect", "description: Cross-cutting design review", "model: sonnet", "- Read", "- Edit", "- Bash", "# Custom prompt"} {
		if !strings.Contains(body, want) {
			t.Errorf("output missing %q; got:\n%s", want, body)
		}
	}
}

func TestInstallPersona_EmptyPromptGeneratesDefault(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	err := rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{
		Name:        "backend",
		Description: "Backend review.",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(proj, ".claude", "agents", "backend.md"))
	body := string(data)
	if !strings.Contains(body, "# backend") {
		t.Errorf("expected default heading; got:\n%s", body)
	}
	if !strings.Contains(body, "Backend review.") {
		t.Errorf("expected description in body")
	}
}

// TestWriteClaudeAgent_TranslatesCanonicalCapabilities pins the mapping the
// live binary forced. claude 2.1.153 drops every frontmatter tool name it
// does not recognize: an agent declaring `tools: [read, edit, bash]` came up
// with "tools":[] in its init frame — no tools at all — while
// `tools: [Read, Glob, Grep, Bash]` came up with exactly those four.
func TestWriteClaudeAgent_TranslatesCanonicalCapabilities(t *testing.T) {
	out, err := RenderPersona(runtime.PersonaSpec{
		Name:         "tester",
		Capabilities: []string{"read", "bash"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	for _, want := range []string{"- Read", "- Glob", "- Grep", "- Bash"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"- read", "- bash"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("canonical capability %q leaked into the artifact, which claude silently drops:\n%s", unwanted, body)
		}
	}
}

// TestWriteClaudeAgent_UnknownCapabilityPassesThrough keeps an operator able
// to name a Claude Code (or MCP) tool directly, and proves de-duplication
// across overlapping capabilities.
func TestWriteClaudeAgent_UnknownCapabilityPassesThrough(t *testing.T) {
	out, err := RenderPersona(runtime.PersonaSpec{
		Name:         "tester",
		Capabilities: []string{"read", "edit", "TodoWrite"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "- TodoWrite") {
		t.Errorf("unknown tool name dropped:\n%s", out)
	}
	if n := strings.Count(string(out), "- Read\n"); n != 1 {
		t.Errorf("Read listed %d times (read and edit both map to it), want 1:\n%s", n, out)
	}
}

// TestWriteClaudeAgent_NoCapabilitiesOmitsTools proves the default is the
// full toolbox. An empty `tools:` key would be the worst outcome — a persona
// that cannot act — and shipmates governs tool use through policy and the
// can_use_tool approval path instead.
func TestWriteClaudeAgent_NoCapabilitiesOmitsTools(t *testing.T) {
	out, err := RenderPersona(runtime.PersonaSpec{Name: "tester", Description: "QA."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "tools:") {
		t.Errorf("emitted a tools key for a persona with no capabilities:\n%s", out)
	}
}

// TestRenderPersonaMatchesInstallPersona is what lets `shipmates add` write
// the artifact itself (through the install manifest, so local edits survive)
// without the two paths drifting apart.
func TestRenderPersonaMatchesInstallPersona(t *testing.T) {
	spec := runtime.PersonaSpec{
		Name:         "architect",
		Description:  "Cross-cutting design review.",
		Model:        "sonnet",
		Capabilities: []string{"read", "edit"},
		SystemPrompt: "# Role\n\nBody text.\n",
	}
	rendered, err := RenderPersona(spec)
	if err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	if err := New(Config{}).InstallPersona(context.Background(), proj, spec); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(proj, AgentPath("architect")))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(rendered) {
		t.Errorf("InstallPersona and RenderPersona disagree:\n--- installed\n%s\n--- rendered\n%s", onDisk, rendered)
	}
}

// splitFrontmatter cuts a rendered artifact into its YAML frontmatter and its
// body, failing the test if the file is not in the `---\n…\n---\n\n` shape
// Claude Code requires.
func splitFrontmatter(t *testing.T, artifact []byte) ([]byte, string) {
	t.Helper()
	const fence = "---\n"
	s := string(artifact)
	if !strings.HasPrefix(s, fence) {
		t.Fatalf("artifact does not open with a frontmatter fence:\n%s", s)
	}
	rest := s[len(fence):]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		t.Fatalf("artifact frontmatter is never closed:\n%s", s)
	}
	return []byte(rest[:end+1]), rest[end+1+len(fence):]
}

// TestRenderPersona_EmitsOnlyClaudeFrontmatter closes the loop the runtime
// interface promises: a canonical shipmates persona translated into the claude
// artifact keeps its identity, model, tools and body — and carries NOTHING
// else. Shipmates-only frontmatter (byline, domainGlob, memoryDir,
// permissions, remoteControl) and `berth:` must never reach a runtime
// artifact; the assertion is on the exact key set so a new field cannot leak
// in unnoticed.
//
// NOTE: the reference implementation asserts this by re-parsing the artifact
// with internal/runtime/persona. That package does not exist on this branch's
// base, so the frontmatter is decoded directly here instead. Restore the
// round-trip through persona.Parse once it lands.
func TestRenderPersona_EmitsOnlyClaudeFrontmatter(t *testing.T) {
	pinHome(t) // default brig posture regardless of the developer's config
	spec := runtime.PersonaSpec{
		Name:         "backend",
		Description:  "APIs and request lifecycle.",
		Model:        "sonnet",
		Capabilities: []string{"read", "bash"},
		SystemPrompt: "# Role\n\nReview the request lifecycle.\n",
	}
	artifact, err := RenderPersona(spec)
	if err != nil {
		t.Fatal(err)
	}
	fm, body := splitFrontmatter(t, artifact)

	var keys map[string]any
	if err := yaml.Unmarshal(fm, &keys); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v\n%s", err, fm)
	}
	want := map[string]bool{"name": true, "description": true, "model": true, "tools": true}
	for k := range keys {
		if !want[k] {
			t.Errorf("unexpected frontmatter key %q leaked into the claude artifact: %v", k, keys)
		}
	}
	for k := range want {
		if _, ok := keys[k]; !ok {
			t.Errorf("frontmatter key %q missing: %v", k, keys)
		}
	}
	if keys["name"] != "backend" || keys["description"] != "APIs and request lifecycle." || keys["model"] != "sonnet" {
		t.Errorf("identity lost: %v", keys)
	}
	tools, _ := keys["tools"].([]any)
	var got []string
	for _, tl := range tools {
		s, _ := tl.(string)
		got = append(got, s)
	}
	if strings.Join(got, ",") != "Read,Glob,Grep,Bash" {
		t.Errorf("tools = %v, want [Read Glob Grep Bash]", got)
	}
	// The body is the system prompt verbatim, plus the marker-delimited
	// Ship's Articles reminder the brig splices in (default posture).
	idx := strings.Index(body, brig.PromptStartMarker)
	if idx < 0 {
		t.Fatalf("body lacks the Ship's Articles reminder block:\n%s", body)
	}
	if !strings.Contains(body, brig.PromptEndMarker) {
		t.Errorf("Articles block is never closed:\n%s", body)
	}
	if got := strings.TrimSpace(body[:idx]); got != strings.TrimSpace(spec.SystemPrompt) {
		t.Errorf("body changed:\n--- got\n%q\n--- want\n%q", got, spec.SystemPrompt)
	}
	// Exactly one blank line between the closing fence and the body, whichever
	// caller produced the spec.
	if !bytes.Contains(artifact, []byte("---\n\n# Role")) {
		t.Errorf("body spacing changed:\n%s", artifact)
	}
}

func TestInstallPersona_NoName(t *testing.T) {
	rt := New(Config{})
	err := rt.InstallPersona(context.Background(), t.TempDir(), runtime.PersonaSpec{})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestUninstallPersona_Idempotent(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	// Install then uninstall twice — second uninstall must not error.
	_ = rt.InstallPersona(context.Background(), proj, runtime.PersonaSpec{Name: "tester"})
	if err := rt.UninstallPersona(context.Background(), proj, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := rt.UninstallPersona(context.Background(), proj, "tester"); err != nil {
		t.Errorf("second uninstall should be idempotent, got %v", err)
	}
	if err := rt.UninstallPersona(context.Background(), proj, "never-installed"); err != nil {
		t.Errorf("uninstalling never-existed should not error, got %v", err)
	}
}

// readSessionStart returns the parsed settings file and its SessionStart
// hook groups.
func readSessionStart(t *testing.T, proj string) (map[string]any, []any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(proj, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("no hooks block; got %v", settings)
	}
	groups, _ := hooks["SessionStart"].([]any)
	return settings, groups
}

// countMemoryHooks counts our marker command across every SessionStart
// matcher group, in the nested shape Claude Code actually executes.
func countMemoryHooks(groups []any) int { return countHookCommand(groups, "load-memory") }

// countBrigHooks counts the Brig reminder hook the same way.
func countBrigHooks(groups []any) int { return countHookCommand(groups, "brig-reminder") }

func countHookCommand(groups []any, marker string) int {
	n := 0
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); strings.Contains(cmd, marker) {
				n++
			}
		}
	}
	return n
}

// TestInstallMemoryHook_WritesTheShapeClaudeExecutes pins the matcher-group
// form. The flatter {"SessionStart":[{"type":"command",…}]} spelling parses
// but is silently ignored by claude 2.1.153 — verified with
// --include-hook-events: only the nested form produced hook_started /
// hook_response frames.
func TestInstallMemoryHook_WritesTheShapeClaudeExecutes(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
		t.Fatal(err)
	}
	_, groups := readSessionStart(t, proj)
	if len(groups) != 2 {
		t.Fatalf("SessionStart len = %d, want 2 (load-memory + brig-reminder)", len(groups))
	}
	wantCommands := []string{MemoryHookCommand, BrigHookCommand}
	for i, g := range groups {
		group := g.(map[string]any)
		if _, flat := group["command"]; flat {
			t.Fatal("hook written in the flat shape claude ignores")
		}
		inner, _ := group["hooks"].([]any)
		if len(inner) != 1 {
			t.Fatalf("group hooks len = %d, want 1: %v", len(inner), group)
		}
		entry := inner[0].(map[string]any)
		if entry["type"] != "command" || entry["command"] != wantCommands[i] {
			t.Errorf("hook entry %d = %v, want command %q", i, entry, wantCommands[i])
		}
		// No matcher: hooks must fire on resumed sessions too, not only fresh ones.
		if _, ok := group["matcher"]; ok {
			t.Errorf("group pins a matcher, which would skip non-startup sessions: %v", group)
		}
	}
}

func TestInstallMemoryHook_IdempotentAndPreservesExisting(t *testing.T) {
	rt := New(Config{})
	proj := t.TempDir()
	// Pre-write settings.json with an unrelated user hook and unrelated keys.
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	pre := []byte(`{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command", "command": "echo hi" } ] }
    ],
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "echo tool" } ] }
    ]
  },
  "theme": "dark"
}`)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), pre, 0o644)

	// Install twice.
	for range 2 {
		if err := rt.InstallMemoryHook(context.Background(), proj); err != nil {
			t.Fatal(err)
		}
	}
	settings, groups := readSessionStart(t, proj)
	if settings["theme"] != "dark" {
		t.Errorf("unrelated key lost; got %v", settings)
	}
	if _, ok := settings["hooks"].(map[string]any)["PreToolUse"]; !ok {
		t.Errorf("unrelated hook event lost; got %v", settings)
	}
	if len(groups) != 3 {
		t.Errorf("SessionStart len = %d, want 3 (the operator's group + load-memory + brig-reminder, not doubled)", len(groups))
	}
	if n := countMemoryHooks(groups); n != 1 {
		t.Errorf("found %d load-memory hooks, want exactly 1", n)
	}
	if n := countBrigHooks(groups); n != 1 {
		t.Errorf("found %d brig-reminder hooks, want exactly 1", n)
	}
	// The operator's own hook must survive verbatim.
	first := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if first["command"] != "echo hi" {
		t.Errorf("operator hook was rewritten: %v", first)
	}
}

// TestInstallMemoryHook_MigratesTheLegacyFlatEntry proves a previously
// written flat entry — which claude never executed — is replaced by a
// working group rather than left behind as dead configuration.
func TestInstallMemoryHook_MigratesTheLegacyFlatEntry(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	pre := []byte(`{"hooks":{"SessionStart":[{"type":"command","command":"` + MemoryHookCommand + `"}]}}`)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), pre, 0o644)

	if err := InstallMemoryHookAt(proj); err != nil {
		t.Fatal(err)
	}
	_, groups := readSessionStart(t, proj)
	if len(groups) != 2 {
		t.Fatalf("SessionStart len = %d, want 2 (memory migrated in place + brig-reminder)", len(groups))
	}
	for _, g := range groups {
		if _, flat := g.(map[string]any)["command"]; flat {
			t.Fatal("legacy flat entry survived migration")
		}
	}
	if n := countMemoryHooks(groups); n != 1 {
		t.Errorf("found %d load-memory hooks after migration, want 1", n)
	}
	if n := countBrigHooks(groups); n != 1 {
		t.Errorf("found %d brig-reminder hooks after migration, want 1", n)
	}
}

// TestInstallMemoryHook_RejectsUnparseableSettings proves an operator's
// broken settings file is reported, never silently overwritten.
func TestInstallMemoryHook_RejectsUnparseableSettings(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "settings.json")
	_ = os.WriteFile(path, []byte(`{"theme": "dark"`), 0o644)

	if err := InstallMemoryHookAt(proj); err == nil {
		t.Fatal("expected an error for unparseable settings")
	}
	data, _ := os.ReadFile(path)
	if string(data) != `{"theme": "dark"` {
		t.Errorf("broken settings file was modified: %s", data)
	}
}

// TestInstallMemoryHook_TreatsEmptyFileAsAbsent covers the file an editor
// or a failed write can leave behind.
func TestInstallMemoryHook_TreatsEmptyFileAsAbsent(t *testing.T) {
	proj := t.TempDir()
	dir := filepath.Join(proj, ".claude")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte("  \n"), 0o644)

	if err := InstallMemoryHookAt(proj); err != nil {
		t.Fatal(err)
	}
	if _, groups := readSessionStart(t, proj); countMemoryHooks(groups) != 1 {
		t.Errorf("hook not installed over an empty file: %v", groups)
	}
}
