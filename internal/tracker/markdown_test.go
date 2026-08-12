package tracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func markdownFixture(t *testing.T) (*Markdown, string, string) {
	t.Helper()
	root := t.TempDir()
	m := NewMarkdown(root)
	id, err := m.CreateTask(context.Background(), Task{
		Title:       "Implement the export",
		Description: "Shipmates voyage: markdown backend\nPersona: backend",
		Assignee:    "backend",
		ExternalRef: "shipmates:voyage:0123456789abcdef:implement-export",
		Labels:      []string{"shipmates", "voyage", "backend"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, root, id
}

func readTask(t *testing.T, root, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".shipmates", "voyage", id+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMarkdownIDIsHumanReadableAndStable(t *testing.T) {
	_, _, id := markdownFixture(t)
	if id != "0123456789abcdef-implement-export" {
		t.Fatalf("id = %q, want the plan-hash-prefix + task id form", id)
	}
}

func TestMarkdownLifecycleRoundTripsStatusInFrontmatter(t *testing.T) {
	m, root, id := markdownFixture(t)
	ctx := context.Background()
	if got := readTask(t, root, id); !strings.Contains(got, "status: open") || !strings.Contains(got, "# Implement the export") {
		t.Fatalf("created file = %s", got)
	}
	if err := m.Start(ctx, id, "backend"); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, root, id); !strings.Contains(got, "status: in_progress") {
		t.Fatalf("after Start: %s", got)
	}
	if err := m.Complete(ctx, id, "exported all accounts; tests pass"); err != nil {
		t.Fatal(err)
	}
	got := readTask(t, root, id)
	for _, want := range []string{"status: closed", "close_reason: Shipmates voyage task completed", "completed: exported all accounts; tests pass", "## Log"} {
		if !strings.Contains(got, want) {
			t.Fatalf("after Complete missing %q:\n%s", want, got)
		}
	}
	if err := m.Block(ctx, id, "captain decision required"); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, root, id); !strings.Contains(got, "status: blocked") || !strings.Contains(got, "blocked: captain decision required") {
		t.Fatalf("after Block: %s", got)
	}
}

func TestMarkdownDependenciesRecordAndRefuseUnknown(t *testing.T) {
	m, root, id := markdownFixture(t)
	ctx := context.Background()
	dep, err := m.CreateTask(ctx, Task{Title: "Design first", Assignee: "architect", ExternalRef: "shipmates:voyage:0123456789abcdef:design"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddDependency(ctx, id, dep); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, root, id); !strings.Contains(got, "depends_on:") || !strings.Contains(got, dep) {
		t.Fatalf("dependency not recorded:\n%s", got)
	}
	// Idempotent re-add.
	if err := m.AddDependency(ctx, id, dep); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, root, id); strings.Count(got, "- "+dep) != 1 {
		t.Fatalf("dependency duplicated:\n%s", got)
	}
	// Real bd rejects `bd dep add <id> <missing>`; the markdown backend must
	// behave the same because sail's inherited-prerequisite handling depends
	// on the failure.
	if err := m.AddDependency(ctx, id, "0123456789abcdef-vanished"); err == nil {
		t.Fatal("dependency onto a missing task was accepted")
	}
}

func TestMarkdownCreateIsIdempotentForSameExternalRefOnly(t *testing.T) {
	m, _, id := markdownFixture(t)
	again, err := m.CreateTask(context.Background(), Task{Title: "Implement the export", ExternalRef: "shipmates:voyage:0123456789abcdef:implement-export"})
	if err != nil || again != id {
		t.Fatalf("idempotent recreate = %q, %v", again, err)
	}
	// The same file claimed by a different task is refused, never overwritten.
	other := Task{Title: "Different work", ExternalRef: "shipmates:derivative:0123456789abcdef:implement-export"}
	otherID := taskID(other)
	if otherID != id {
		t.Fatalf("fixture ids diverged: %q vs %q", otherID, id)
	}
	if _, err := m.CreateTask(context.Background(), other); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("conflicting recreate error = %v", err)
	}
}

// TestMarkdownSurvivesCrashMidWrite proves the atomic-write pattern: a stray
// temp file from a crashed writer is inert, and the task file itself is
// always either the old or the new complete record.
func TestMarkdownSurvivesCrashMidWrite(t *testing.T) {
	m, root, id := markdownFixture(t)
	dir := filepath.Join(root, ".shipmates", "voyage")
	if err := os.WriteFile(filepath.Join(dir, ".task-crashed-1234.tmp"), []byte("---\ntorn frontmatt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), id, "backend"); err != nil {
		t.Fatalf("stray temp file broke the tracker: %v", err)
	}
	if got := readTask(t, root, id); !strings.Contains(got, "status: in_progress") {
		t.Fatalf("update lost next to stray temp file:\n%s", got)
	}
	if got := m.Show(context.Background(), id); !strings.Contains(got, "status: in_progress") {
		t.Fatalf("Show = %q", got)
	}
}

// TestMarkdownToleratesHandEdits: a human fixing a typo in the body or
// flipping a status by hand must parse and keep working; their body edit must
// survive the next tracker write.
func TestMarkdownToleratesHandEdits(t *testing.T) {
	m, root, id := markdownFixture(t)
	path := filepath.Join(root, ".shipmates", "voyage", id+".md")
	raw := readTask(t, root, id)
	edited := strings.Replace(raw, "status: open", "status: in_progress", 1)
	edited = strings.Replace(edited, "Implement the export", "Implement the account export (clarified by the captain)", 1)
	edited += "\nHuman note: keep the retention policy out of scope.\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(context.Background(), id, "done"); err != nil {
		t.Fatalf("hand-edited file stopped the tracker: %v", err)
	}
	got := readTask(t, root, id)
	for _, want := range []string{"status: closed", "Human note: keep the retention policy out of scope."} {
		if !strings.Contains(got, want) {
			t.Fatalf("hand edit lost, missing %q:\n%s", want, got)
		}
	}
}

func TestMarkdownGarbageNamesTheFile(t *testing.T) {
	m, root, id := markdownFixture(t)
	path := filepath.Join(root, ".shipmates", "voyage", id+".md")
	for name, garbage := range map[string]string{
		"no frontmatter":  "just some text\n",
		"unterminated":    "---\nid: " + id + "\n",
		"unreadable yaml": "---\nid: [unclosed\n---\nbody\n",
		"invented status": "---\nid: " + id + "\ntitle: x\nstatus: shipped\n---\nbody\n",
		"mismatched id":   "---\nid: someone-else\ntitle: x\nstatus: open\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
				t.Fatal(err)
			}
			err := m.Start(context.Background(), id, "backend")
			if err == nil {
				t.Fatal("garbage task file accepted")
			}
			if !strings.Contains(err.Error(), id+".md") {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

// TestMarkdownResumeAfterRestart: a brand-new tracker instance on the same
// root reads every task purely from the files.
func TestMarkdownResumeAfterRestart(t *testing.T) {
	m, root, id := markdownFixture(t)
	if err := m.Start(context.Background(), id, "backend"); err != nil {
		t.Fatal(err)
	}
	restarted := NewMarkdown(root)
	if got := restarted.Show(context.Background(), id); !strings.Contains(got, "status: in_progress") {
		t.Fatalf("restarted Show = %q", got)
	}
	again, err := restarted.CreateTask(context.Background(), Task{Title: "Implement the export", ExternalRef: "shipmates:voyage:0123456789abcdef:implement-export"})
	if err != nil || again != id {
		t.Fatalf("restarted recreate = %q, %v", again, err)
	}
	if err := restarted.Complete(context.Background(), id, "finished after restart"); err != nil {
		t.Fatal(err)
	}
	if got := readTask(t, root, id); !strings.Contains(got, "status: closed") {
		t.Fatalf("restart lost state:\n%s", got)
	}
}

// TestMarkdownLogNoteCannotForgeStructure: crew summaries flow into the log;
// newlines must not let them fabricate frontmatter or log entries.
func TestMarkdownLogNoteCannotForgeStructure(t *testing.T) {
	m, root, id := markdownFixture(t)
	if err := m.Complete(context.Background(), id, "done\n---\nstatus: open\n---\n- forged log line"); err != nil {
		t.Fatal(err)
	}
	got := readTask(t, root, id)
	if strings.Count(got, "\n---\n") != 1 {
		t.Fatalf("summary forged a frontmatter block:\n%s", got)
	}
	meta, _, err := m.load(id)
	if err != nil || meta.Status != "closed" {
		t.Fatalf("meta = %+v err=%v", meta, err)
	}
}

func TestMarkdownRejectsSymlinkedTaskFile(t *testing.T) {
	m, root, id := markdownFixture(t)
	dir := filepath.Join(root, ".shipmates", "voyage")
	target := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(target, []byte("---\nid: linked\ntitle: x\nstatus: open\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := m.Start(context.Background(), "linked", "backend"); err == nil {
		t.Fatal("symlinked task file accepted")
	}
	_ = id
}
