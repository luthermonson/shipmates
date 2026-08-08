package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/beads/bdtest"
)

// TestRealBDClientLifecycle runs every Client method that parses or otherwise
// depends on bd's output against the real external CLI, and verifies the effect
// by asking bd itself rather than by inspecting a call log.
//
// This is the only test that proves the external contract: issueID parses real
// `bd create --json` stdout, every flag the adapter passes is actually accepted,
// and the status names it writes ("in_progress", "blocked") are real bd statuses.
func TestRealBDClientLifecycle(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	bdtest.OnPath(t, bd)

	// New goes through Available/exec.LookPath, so PATH resolution is covered.
	client, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if client.Command == "" || client.Root != root {
		t.Fatalf("client = %+v", client)
	}
	ctx := context.Background()

	parent, err := client.CreateTask(ctx, Task{
		Title:       "Design the bounded adapter",
		Description: "Shipmates voyage: prove the external contract\nPersona: architect",
		Assignee:    "architect",
		ExternalRef: "shipmates:voyage:0123456789abcdef:design",
		Labels:      []string{"shipmates", "voyage", "architect"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !validID(parent) {
		t.Fatalf("bd returned an id the adapter rejects: %q", parent)
	}

	// Everything the adapter claims to send must actually be stored by bd.
	record := bdtest.Issue(t, bd, root, parent)
	for field, want := range map[string]string{
		"title":        "Design the bounded adapter",
		"assignee":     "architect",
		"external_ref": "shipmates:voyage:0123456789abcdef:design",
		"issue_type":   "task",
		"status":       "open",
	} {
		if got := bdtest.Field(t, record, field); got != want {
			t.Errorf("bd recorded %s = %q, want %q", field, got, want)
		}
	}
	if got := bdtest.Field(t, record, "description"); !strings.Contains(got, "prove the external contract") {
		t.Errorf("bd recorded description = %q", got)
	}
	if labels, _ := record["labels"].([]any); len(labels) != 3 {
		t.Errorf("bd recorded labels = %v, want three", record["labels"])
	}

	child, err := client.CreateTask(ctx, Task{Title: "Build on the design", Assignee: "backend"})
	if err != nil {
		t.Fatalf("CreateTask child: %v", err)
	}
	if child == parent {
		t.Fatalf("bd reused issue id %q", child)
	}
	if err := client.AddDependency(ctx, child, parent); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if !bdtest.DependsOn(t, bd, root, child, parent) {
		t.Fatalf("bd does not record %s depending on %s:\n%s", child, parent, bdtest.Run(t, bd, root, "dep", "list", child))
	}

	if err := client.Start(ctx, parent, "architect"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	started := bdtest.Issue(t, bd, root, parent)
	if got := bdtest.Field(t, started, "status"); got != "in_progress" {
		t.Errorf("after Start bd status = %q, want in_progress", got)
	}
	if got := bdtest.Field(t, started, "assignee"); got != "architect" {
		t.Errorf("after Start bd assignee = %q, want architect", got)
	}

	// Show is injected verbatim into crew prompts; it must be real JSON naming
	// the bead, and it must survive the client's output bounding.
	shown := client.Show(ctx, parent)
	if !strings.Contains(shown, parent) || !strings.Contains(shown, "in_progress") {
		t.Fatalf("Show(%s) = %q", parent, shown)
	}

	// Prime is bounded guidance injected into every crew prompt.
	prime := client.Prime(ctx)
	if !strings.Contains(prime, "bd ") {
		t.Fatalf("Prime returned no bd guidance: %q", prime)
	}
	if len(prime) > 32<<10 {
		t.Fatalf("Prime returned %d bytes, past its bound", len(prime))
	}

	// Documented real-bd rule, discovered by this test and not visible to a fake
	// that exits 0 for everything: bd refuses to close an issue whose blockers
	// are still open. Complete therefore only succeeds dependency-first, which is
	// the order sail produces (a task is dispatched only once its dependencies
	// completed). Shipmates deliberately does not pass bd's --force override.
	if err := client.Complete(ctx, child, "closing out of order"); err == nil {
		t.Error("bd closed a bead whose blocker is still open")
	} else if !strings.Contains(err.Error(), "blocked by open issues") {
		t.Errorf("out-of-order close error = %v", err)
	}

	if err := client.Complete(ctx, parent, "verified against the real bd CLI"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	completed := bdtest.Issue(t, bd, root, parent)
	if got := bdtest.Field(t, completed, "status"); got != "closed" {
		t.Errorf("after Complete bd status = %q, want closed", got)
	}
	if got := bdtest.Field(t, completed, "close_reason"); got != "Shipmates voyage task completed" {
		t.Errorf("after Complete bd close_reason = %q", got)
	}
	comments := bdtest.Comments(t, bd, root, parent)
	if !strings.Contains(comments, "verified against the real bd CLI") || !strings.Contains(comments, "shipmates") {
		t.Errorf("Complete did not record an authored summary comment:\n%s", comments)
	}

	if err := client.Block(ctx, child, "waiting on the Captain"); err != nil {
		t.Fatalf("Block: %v", err)
	}
	blocked := bdtest.Issue(t, bd, root, child)
	if got := bdtest.Field(t, blocked, "status"); got != "blocked" {
		t.Errorf("after Block bd status = %q, want blocked", got)
	}
	if got := bdtest.Comments(t, bd, root, child); !strings.Contains(got, "waiting on the Captain") {
		t.Errorf("Block did not record its reason:\n%s", got)
	}
}

// TestRealBDRejectsFlagLikeIssueIDsBeforeExec proves the validID guard is what
// stops flag-shaped ids, not bd's own argument parsing: the same ids that the
// adapter refuses are ones a real bd would otherwise interpret as options.
func TestRealBDRejectsFlagLikeIssueIDsBeforeExec(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	client := &Client{Root: root, Command: bd, Timeout: DefaultTimeout}
	ctx := context.Background()
	for _, id := range []string{"--help", "-h", "ship 1; rm -rf /"} {
		if err := client.Start(ctx, id, "backend"); err == nil {
			t.Errorf("Start accepted untrusted id %q", id)
		}
		if err := client.Complete(ctx, id, "x"); err == nil {
			t.Errorf("Complete accepted untrusted id %q", id)
		}
		if err := client.Block(ctx, id, "x"); err == nil {
			t.Errorf("Block accepted untrusted id %q", id)
		}
		if got := client.Show(ctx, id); got != "" {
			t.Errorf("Show accepted untrusted id %q -> %q", id, got)
		}
	}
}

// TestRealBDFailureIsReportedFromStderr checks the error surface against real bd
// rather than a fake that always succeeds.
func TestRealBDFailureIsReportedFromStderr(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	client := &Client{Root: root, Command: bd, Timeout: DefaultTimeout}
	if err := client.Start(context.Background(), "ship-does-not-exist", "backend"); err == nil {
		t.Fatal("updating a nonexistent bead succeeded")
	} else if !strings.Contains(err.Error(), "bd command failed") {
		t.Fatalf("error = %v", err)
	}
}

// TestRealBDNewRequiresInitializedWorkspace covers the guard that keeps sail
// from mirroring into a directory Beads does not own.
func TestRealBDNewRequiresInitializedWorkspace(t *testing.T) {
	bd := bdtest.Binary(t)
	bdtest.OnPath(t, bd)
	bare := t.TempDir()
	if _, err := New(bare); err == nil {
		t.Fatal("New accepted a directory with no .beads")
	}
	if err := os.Mkdir(filepath.Join(bare, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(bare); err != nil {
		t.Fatalf("New rejected a Beads workspace: %v", err)
	}
}

// TestAvailableResolvesFromPATHOnly pins the resolution policy: bd is taken
// from PATH and nowhere else. A bd dropped beside the shipmates executable is
// not a trusted install location.
func TestAvailableResolvesFromPATHOnly(t *testing.T) {
	bd := bdtest.Binary(t)
	bdtest.OnPath(t, bd)
	resolved, err := Available()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(resolved) != filepath.Base(bd) && !strings.HasPrefix(filepath.Base(resolved), "bd") {
		t.Fatalf("Available resolved %q", resolved)
	}
	t.Setenv("PATH", t.TempDir())
	if command, err := Available(); err == nil {
		t.Fatalf("Available found %q with bd off PATH", command)
	}

	// Regression guard for the removed os.Executable()-adjacent fallback: a bd
	// planted beside the running binary must never be executed.
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test executable: %v", err)
	}
	for _, name := range []string{"bd", "bd.exe"} {
		planted := filepath.Join(filepath.Dir(self), name)
		if _, statErr := os.Lstat(planted); statErr == nil {
			continue // Something legitimately named bd already lives there.
		}
		if writeErr := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o700); writeErr != nil {
			continue // Not writable; nothing to prove here.
		}
		t.Cleanup(func() { _ = os.Remove(planted) })
	}
	if command, err := Available(); err == nil {
		t.Fatalf("Available executed a bd planted next to the binary: %q", command)
	}
}
