package openai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
)

func TestInstallPersona_WritesNativeFile(t *testing.T) {
	dir := t.TempDir()
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	spec := runtime.PersonaSpec{
		Name:         "captain",
		Description:  "Coordinates the crew.\nHolds the plan.",
		Capabilities: []string{"read", "edit", "bash"},
		Model:        "some-model",
		SystemPrompt: "You are the captain. Keep the ship pointed at the goal.",
		MemorySeeds:  []string{"seed.md"},
	}
	if err := r.InstallPersona(context.Background(), dir, spec); err != nil {
		t.Fatalf("InstallPersona: %v", err)
	}

	path := filepath.Join(dir, project.Dir, "runtimes", "openai", "personas", "captain.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persona file not written where expected: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"name: captain",
		"runtime: openai",
		"declaredCapabilities: [read, edit, bash]",
		"You are the captain.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("persona file missing %q:\n%s", want, got)
		}
	}
	// The file must say out loud that the declared capabilities are inert
	// here, so a reader is not misled by "edit".
	if !strings.Contains(got, "not exercised") {
		t.Errorf("persona file does not disclaim its capabilities:\n%s", got)
	}
	// A multi-line description must not break the frontmatter block.
	if strings.Count(got, "\n---\n") > 1 || !strings.Contains(got, "description: Coordinates the crew. Holds the plan.") {
		t.Errorf("description not folded to one line:\n%s", got)
	}

	// The installed body becomes the session's system prompt.
	sys, err := systemPrompt(dir, "captain", DefaultMaxSystemPromptBytes)
	if err != nil {
		t.Fatalf("systemPrompt: %v", err)
	}
	if !strings.Contains(sys, "You are the captain.") {
		t.Errorf("system prompt lost the persona body:\n%s", sys)
	}
	if strings.Contains(sys, "declaredCapabilities") {
		t.Errorf("frontmatter leaked into the system prompt:\n%s", sys)
	}

	// Uninstall removes it and is idempotent.
	if err := r.UninstallPersona(context.Background(), dir, "captain"); err != nil {
		t.Fatalf("UninstallPersona: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("persona file survived uninstall: %v", err)
	}
	if err := r.UninstallPersona(context.Background(), dir, "captain"); err != nil {
		t.Errorf("second UninstallPersona should be a no-op: %v", err)
	}
}

func TestInstallPersona_SynthesisesABodyWhenNoneGiven(t *testing.T) {
	dir := t.TempDir()
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	if err := r.InstallPersona(context.Background(), dir, runtime.PersonaSpec{
		Name:         "tester",
		Description:  "Writes and runs tests.",
		Capabilities: []string{"bash"},
	}); err != nil {
		t.Fatal(err)
	}
	sys, err := systemPrompt(dir, "tester", DefaultMaxSystemPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "You are tester") || !strings.Contains(sys, "Writes and runs tests.") {
		t.Errorf("synthesised body missing:\n%s", sys)
	}
	if !strings.Contains(sys, "none of them are executable") {
		t.Errorf("synthesised body should disclaim the declared capabilities:\n%s", sys)
	}
}

// Requirement 5: the persona's prompt becomes the system message — and the
// model is told plainly that it has no tools, because otherwise a strong
// instruct model offers to edit files it cannot touch.
func TestSystemPrompt_StatesTheRuntimeContract(t *testing.T) {
	sys, err := systemPrompt(t.TempDir(), "captain", DefaultMaxSystemPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Runtime contract",
		"cannot read, write, or edit files",
		"cannot run commands",
		"No persona file is installed",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sys)
		}
	}
}

func TestSystemPrompt_FoldsInBoundedMemory(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, project.MemoryDir("captain"))
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(memDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("01-decisions.md", "We chose SSE over websockets.")
	write("02-people.md", "The operator prefers short answers.")
	write("ignored.bin", "should not be read")
	write("huge.md", strings.Repeat("z", memoryFileBytes+4096))

	sys, err := systemPrompt(dir, "captain", DefaultMaxSystemPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sys, "We chose SSE over websockets.") || !strings.Contains(sys, "prefers short answers") {
		t.Errorf("memory not folded in:\n%s", sys)
	}
	if strings.Contains(sys, "should not be read") {
		t.Error("a non-markdown memory file was read")
	}
	if !strings.Contains(sys, "[truncated]") {
		t.Error("an oversized memory file was not truncated")
	}
	// Deterministic: same inputs, same prompt. Otherwise nothing about a
	// session is reproducible.
	again, err := systemPrompt(dir, "captain", DefaultMaxSystemPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if again != sys {
		t.Error("system prompt is not deterministic across calls")
	}
	// Memory is labelled as context, not as operator instructions.
	if !strings.Contains(sys, "Treat them as context") {
		t.Errorf("memory block is not framed as context:\n%s", sys)
	}
}

func TestSystemPrompt_TruncatesVisibly(t *testing.T) {
	dir := t.TempDir()
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	if err := r.InstallPersona(context.Background(), dir, runtime.PersonaSpec{
		Name:         "windbag",
		SystemPrompt: strings.Repeat("instructions. ", 5000),
	}); err != nil {
		t.Fatal(err)
	}
	const cap = 2048
	sys, err := systemPrompt(dir, "windbag", cap)
	if err != nil {
		t.Fatal(err)
	}
	if len(sys) > cap {
		t.Errorf("system prompt is %d bytes, over the %d cap", len(sys), cap)
	}
	if !strings.Contains(sys, "[truncated: system prompt exceeded max_system_prompt_bytes]") {
		t.Error("truncation was silent")
	}
}

func TestValidPersonaName(t *testing.T) {
	bad := []string{
		"", " ", "captain ", "../escape", "a/b", `a\b`, "C:evil", ".hidden", "nul\x00byte",
	}
	for _, name := range bad {
		if err := validPersonaName(name); err == nil {
			t.Errorf("persona name %q was accepted", name)
		}
	}
	for _, name := range []string{"captain", "back-end", "test_er", "架構師"} {
		if err := validPersonaName(name); err != nil {
			t.Errorf("persona name %q rejected: %v", name, err)
		}
	}
	// And the file-writing paths refuse traversal before touching disk.
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	dir := t.TempDir()
	if err := r.InstallPersona(context.Background(), dir, runtime.PersonaSpec{Name: "../../etc/passwd"}); err == nil {
		t.Error("InstallPersona accepted a traversal name")
	}
	if err := r.UninstallPersona(context.Background(), dir, "../../etc/passwd"); err == nil {
		t.Error("UninstallPersona accepted a traversal name")
	}
	if _, err := systemPrompt(dir, "../../etc/passwd", 1024); err == nil {
		t.Error("systemPrompt accepted a traversal name")
	}
}

func TestStripFrontmatter(t *testing.T) {
	cases := map[string]string{
		"---\nname: x\n---\n\nbody here": "body here",
		"no frontmatter at all":          "no frontmatter at all",
		"---\nunterminated: true\nbody":  "---\nunterminated: true\nbody",
		"\n\n---\nname: x\n---\nbody":    "body",
		"---\nname: x\n---\n---\nnot fm": "---\nnot fm",
	}
	for in, want := range cases {
		if got := strings.TrimSpace(stripFrontmatter(in)); got != want {
			t.Errorf("stripFrontmatter(%q) = %q, want %q", in, got, want)
		}
	}
}

// InstallMemoryHook has nothing to install because memory loading is not a hook
// here — it is unconditional in StartSession. Assert both halves: the call
// writes nothing, and a session really does get the memory anyway.
func TestInstallMemoryHook_IsAGuaranteeNotAHook(t *testing.T) {
	dir := t.TempDir()
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	if err := r.InstallMemoryHook(context.Background(), dir); err != nil {
		t.Fatalf("InstallMemoryHook: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("InstallMemoryHook wrote something: %v", entries)
	}

	memDir := filepath.Join(dir, project.MemoryDir("captain"))
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("the mast is cracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess, err := r.StartSession(context.Background(), runtime.SessionSpec{Persona: "captain", ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.(*session).system, "the mast is cracked") {
		t.Error("session did not load persona memory without a hook")
	}
}

func TestSession_HandleFields(t *testing.T) {
	dir := t.TempDir()
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	sess, err := r.StartSession(context.Background(), runtime.SessionSpec{
		Persona: "captain", ProjectDir: dir, WorkingDir: filepath.Join(dir, "sub"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Persona() != "captain" || sess.ProjectDir() != dir {
		t.Errorf("handle = %q %q", sess.Persona(), sess.ProjectDir())
	}
	if got := sess.(*session).workingDir; got != filepath.Join(dir, "sub") {
		t.Errorf("workingDir = %q", got)
	}
	// WorkingDir defaults to ProjectDir. It is recorded but unused: this
	// runtime has no shell whose cwd it could set.
	sess2, err := r.StartSession(context.Background(), runtime.SessionSpec{Persona: "cook", ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := sess2.(*session).workingDir; got != dir {
		t.Errorf("workingDir default = %q", got)
	}
}
