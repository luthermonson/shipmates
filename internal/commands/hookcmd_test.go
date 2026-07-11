package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

// TestRunLoadMemory_HappyPath: given a memory dir with two files, the hook
// emits a SessionStart hookSpecificOutput whose additionalContext contains
// both filenames and both bodies.
func TestRunLoadMemory_HappyPath(t *testing.T) {
	t.Chdir(t.TempDir())
	memDir := project.MemoryDir("backend")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "01-decisions.md"), []byte("- picked postgres over sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "02-patterns.md"), []byte("- prefer functional options\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin := strings.NewReader(`{"session_id":"abc","agent_type":"backend","source":"startup","cwd":"/proj"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}

	var got hookOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, stdout.String())
	}
	if got.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", got.HookSpecificOutput.HookEventName)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "01-decisions.md") || !strings.Contains(ctx, "02-patterns.md") {
		t.Errorf("expected both filenames in additionalContext, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "picked postgres over sqlite") || !strings.Contains(ctx, "prefer functional options") {
		t.Errorf("expected both bodies in additionalContext, got:\n%s", ctx)
	}
	// Header should name the persona.
	if !strings.Contains(ctx, "backend") {
		t.Errorf("expected persona name in header, got:\n%s", ctx)
	}
	// File order must be stable (lexicographic).
	if strings.Index(ctx, "01-decisions") > strings.Index(ctx, "02-patterns") {
		t.Errorf("expected 01- before 02-, got:\n%s", ctx)
	}
}

// TestRunLoadMemory_NoAgentType: a payload with no agent_type is a silent
// no-op. This matches CC launches without --agent (e.g. someone runs bare
// `claude` in the repo) — nothing to load, don't wedge the session.
func TestRunLoadMemory_NoAgentType(t *testing.T) {
	t.Chdir(t.TempDir())
	stdin := strings.NewReader(`{"session_id":"abc","source":"startup"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout, got: %q", stdout.String())
	}
}

// TestRunLoadMemory_MissingMemoryDir: a persona whose memory dir doesn't
// exist yet (fresh persona, memory not seeded) yields empty stdout, exit 0.
func TestRunLoadMemory_MissingMemoryDir(t *testing.T) {
	t.Chdir(t.TempDir())
	stdin := strings.NewReader(`{"agent_type":"newbie"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout for missing memory dir, got: %q", stdout.String())
	}
}

// TestRunLoadMemory_EmptyMemoryDir: a persona whose memory dir exists but is
// empty is also a silent no-op — no header, no output.
func TestRunLoadMemory_EmptyMemoryDir(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(project.MemoryDir("backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdin := strings.NewReader(`{"agent_type":"backend"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout for empty memory dir, got: %q", stdout.String())
	}
}

// TestRunLoadMemory_NonMdFilesSkipped: only .md files are loaded. .DS_Store
// and editor swap files are ignored — a footgun to avoid.
func TestRunLoadMemory_NonMdFilesSkipped(t *testing.T) {
	t.Chdir(t.TempDir())
	memDir := project.MemoryDir("backend")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("real memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, ".DS_Store"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "notes.md.swp"), []byte("swap\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdin := strings.NewReader(`{"agent_type":"backend"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	body := stdout.String()
	if !strings.Contains(body, "real memory") {
		t.Errorf("expected real memory content, got:\n%s", body)
	}
	if strings.Contains(body, "garbage") || strings.Contains(body, "swap") {
		t.Errorf("expected non-md files skipped, got:\n%s", body)
	}
}

// TestRunLoadMemory_BadJSON: an unparseable payload is a silent no-op (with
// a stderr warning) — a broken hook must never wedge session launch.
func TestRunLoadMemory_BadJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	stdin := strings.NewReader(`{not json at all`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory should not error on bad JSON, got: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout for bad JSON, got: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("expected a stderr warning on bad JSON")
	}
}

// TestRunLoadMemory_EmptyStdin: an empty stdin (no bytes at all) is a
// silent no-op — some hosts might invoke the hook without a payload.
func TestRunLoadMemory_EmptyStdin(t *testing.T) {
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout, got: %q", stdout.String())
	}
}

// TestRunLoadMemory_SubdirWalked: .md files in subdirectories are walked
// too. Some operators like to organize memory into subfolders (e.g.
// `.shipmates/memory/backend/patterns/*.md`); we shouldn't require a flat
// layout.
func TestRunLoadMemory_SubdirWalked(t *testing.T) {
	t.Chdir(t.TempDir())
	memDir := project.MemoryDir("backend")
	sub := filepath.Join(memDir, "patterns")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "options.md"), []byte("prefer functional options\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin := strings.NewReader(`{"agent_type":"backend"}`)
	var stdout, stderr bytes.Buffer
	if err := runLoadMemory(stdin, &stdout, &stderr); err != nil {
		t.Fatalf("runLoadMemory: %v", err)
	}
	if !strings.Contains(stdout.String(), "prefer functional options") {
		t.Errorf("expected subdir content, got:\n%s", stdout.String())
	}
}

// Sanity: buildMemoryContext returns "" for a non-directory path (e.g. the
// operator has an oddly-named file where the memory dir should be).
func TestBuildMemoryContext_NotADirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	// Create a *file* where the memory dir would live.
	if err := os.MkdirAll(filepath.Join(project.Dir, project.MemoryDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.MemoryDir("backend"), []byte("not a dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := buildMemoryContext("backend")
	if err != nil {
		t.Fatalf("buildMemoryContext: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty context when path is not a directory, got: %q", got)
	}
}

// Ensure io.Reader interface still compiles — guard rail for accidental
// signature drift.
var _ = func(r io.Reader) {}
