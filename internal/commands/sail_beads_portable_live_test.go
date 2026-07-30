package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/luthermonson/shipmates/internal/beads/bdtest"
	"github.com/luthermonson/shipmates/internal/voyage"
)

// TestSailBeadsPromptStartFinishAgainstRealBD covers the part of the sail/Beads
// bridge that carries no voyage-state persistence, so it runs on Windows as well
// as unix: prompt injection, the in-progress transition, and both terminal
// transitions, all against the real external bd CLI.
//
// The state-persisting half (prepareSailBeads, ensureDerivativeTask) is covered
// in sail_beads_live_test.go, which is unix-only because voyage.SaveState fsyncs
// a directory.
func TestSailBeadsPromptStartFinishAgainstRealBD(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	bdtest.OnPath(t, bd)

	client, err := beads.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	completedID, err := client.CreateTask(ctx, beads.Task{Title: "Ship the work", Assignee: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	blockedID, err := client.CreateTask(ctx, beads.Task{Title: "Await a decision", Assignee: "tester"})
	if err != nil {
		t.Fatal(err)
	}

	graph := &sailBeads{
		client:  client,
		prime:   client.Prime(ctx),
		records: map[string]string{"ship": client.Show(ctx, completedID)},
	}
	if graph.prime == "" {
		t.Fatal("real bd prime returned nothing to inject")
	}

	task := voyage.Task{ID: "ship", Persona: "backend", Summary: "Ship the work", Prompt: "do it"}
	entry := voyage.TaskState{BeadID: completedID}
	prompt := graph.prompt("base prompt", task, entry)
	for _, want := range []string{
		"base prompt",
		"BEADS TASK GRAPH",
		"Bead: " + completedID,
		"bd show " + completedID + " --json",
		"Project Beads guidance:",
		"Current Bead record:",
		`"status": "open"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	// A task without a bead must be handed the base prompt untouched.
	if got := graph.prompt("base prompt", task, voyage.TaskState{}); got != "base prompt" {
		t.Fatalf("beadless prompt = %q", got)
	}

	if err := graph.start(ctx, task, entry); err != nil {
		t.Fatal(err)
	}
	if got := bdtest.Field(t, bdtest.Issue(t, bd, root, completedID), "status"); got != "in_progress" {
		t.Errorf("after start bd status = %q, want in_progress", got)
	}

	entry.Status, entry.Summary = voyage.Completed, "the crew reported success"
	if err := graph.finish(ctx, entry); err != nil {
		t.Fatal(err)
	}
	closed := bdtest.Issue(t, bd, root, completedID)
	if got := bdtest.Field(t, closed, "status"); got != "closed" {
		t.Errorf("after completed finish bd status = %q, want closed", got)
	}
	if got := bdtest.Comments(t, bd, root, completedID); !strings.Contains(got, "the crew reported success") {
		t.Errorf("completed finish did not record the crew report:\n%s", got)
	}

	for _, status := range []voyage.Status{voyage.Failed, voyage.Blocked, voyage.NeedsInput} {
		blockedEntry := voyage.TaskState{BeadID: blockedID, Status: status, Error: "reason for " + string(status)}
		if err := graph.finish(ctx, blockedEntry); err != nil {
			t.Fatalf("finish(%s): %v", status, err)
		}
		if got := bdtest.Field(t, bdtest.Issue(t, bd, root, blockedID), "status"); got != "blocked" {
			t.Errorf("after %s finish bd status = %q, want blocked", status, got)
		}
		if got := bdtest.Comments(t, bd, root, blockedID); !strings.Contains(got, "reason for "+string(status)) {
			t.Errorf("%s finish did not record its reason:\n%s", status, got)
		}
	}

	// A nil graph and a beadless entry are the no-Beads voyage path.
	var absent *sailBeads
	if got := absent.prompt("base", task, entry); got != "base" {
		t.Errorf("nil graph prompt = %q", got)
	}
	if err := absent.start(ctx, task, entry); err != nil {
		t.Errorf("nil graph start = %v", err)
	}
	if err := absent.finish(ctx, entry); err != nil {
		t.Errorf("nil graph finish = %v", err)
	}
	if err := graph.finish(ctx, voyage.TaskState{BeadID: completedID, Status: voyage.Running}); err != nil {
		t.Errorf("non-terminal finish = %v", err)
	}
}
