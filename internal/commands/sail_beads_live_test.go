// Opt-in tests against the real external bd CLI (see internal/beads/bdtest:
// they run only with SHIPMATES_TEST_BD or SHIPMATES_TEST_BD_REQUIRED=1, never
// merely because bd is on PATH). Unlike the shell-script stand-in in
// sail_tracking_beads_test.go, these prove the graph shipmates believes it
// built exists in bd: ids come back parsable, dependency edges are real, the
// injected prompt carries genuine `bd prime` and `bd show` output, and the
// lifecycle transitions land on real bd statuses.
//
// The reference kept these unix-only because voyage.SaveState fsynced a
// directory; that limitation is gone (project.DurableRename), so they run on
// Windows too when opted in.
package commands

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/beads/bdtest"
	"github.com/luthermonson/shipmates/internal/tracker"
	"github.com/luthermonson/shipmates/internal/voyage"
)

func TestPrepareSailTrackingAgainstRealBD(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	bdtest.OnPath(t, bd)
	t.Chdir(root)

	backend, err := tracker.NewBeads(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{
		Version: 1, Title: "Beads voyage", Objective: "Prove graph mirroring against real bd", Commissioned: true,
		Tasks: []voyage.Task{
			{ID: "design", Persona: "architect", Summary: "Design the adapter", Prompt: "Produce a design"},
			{ID: "build", Persona: "backend", Summary: "Build the adapter", Prompt: "Implement the design", DependsOn: []string{"design"}},
		},
	}
	hash := strings.Repeat("a", 64)
	state := voyage.NewState(plan, hash)
	statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")

	graph, err := prepareSailTracking(context.Background(), backend, plan, state, hash, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if graph == nil {
		t.Fatal("prepareSailTracking returned no graph inside a real workspace")
	}
	design, build := state.Tasks["design"].BeadID, state.Tasks["build"].BeadID
	if design == "" || build == "" || design == build {
		t.Fatalf("bead ids = %q, %q", design, build)
	}
	if !bdtest.DependsOn(t, bd, root, build, design) {
		t.Fatalf("bd does not record the voyage DAG edge %s -> %s", build, design)
	}
	record := bdtest.Issue(t, bd, root, design)
	if got := bdtest.Field(t, record, "external_ref"); got != "shipmates:voyage:"+hash[:16]+":design" {
		t.Errorf("bd recorded external_ref = %q", got)
	}

	prompt := graph.prompt("base prompt", plan.Tasks[1], state.Tasks["build"])
	for _, want := range []string{"base prompt", "VOYAGE TASK TRACKER", "Bead: " + build, "bd show " + build + " --json", "Current task record:"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	entry := state.Tasks["design"]
	if err := graph.start(context.Background(), plan.Tasks[0], entry); err != nil {
		t.Fatal(err)
	}
	if got := bdtest.Field(t, bdtest.Issue(t, bd, root, design), "status"); got != "in_progress" {
		t.Errorf("after start bd status = %q, want in_progress", got)
	}
	entry.Status, entry.Summary = voyage.Completed, "the crew reported success"
	if err := graph.finish(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	closed := bdtest.Issue(t, bd, root, design)
	if got := bdtest.Field(t, closed, "status"); got != "closed" {
		t.Errorf("after completed finish bd status = %q, want closed", got)
	}
	if got := bdtest.Comments(t, bd, root, design); !strings.Contains(got, "the crew reported success") {
		t.Errorf("completed finish did not record the crew report:\n%s", got)
	}

	for _, status := range []voyage.Status{voyage.Failed, voyage.Blocked, voyage.NeedsInput} {
		blockedEntry := voyage.TaskState{BeadID: build, Status: status, Error: "reason for " + string(status)}
		if err := graph.finish(context.Background(), blockedEntry); err != nil {
			t.Fatalf("finish(%s): %v", status, err)
		}
		if got := bdtest.Field(t, bdtest.Issue(t, bd, root, build), "status"); got != "blocked" {
			t.Errorf("after %s finish bd status = %q, want blocked", status, got)
		}
	}

	// A nil graph and a beadless entry are the no-tracking path.
	var absent *sailTracking
	if got := absent.prompt("base", plan.Tasks[0], entry); got != "base" {
		t.Errorf("nil graph prompt = %q", got)
	}
	if err := absent.start(context.Background(), plan.Tasks[0], entry); err != nil {
		t.Errorf("nil graph start = %v", err)
	}
	if err := absent.finish(context.Background(), entry); err != nil {
		t.Errorf("nil graph finish = %v", err)
	}
	if err := graph.finish(context.Background(), voyage.TaskState{BeadID: design, Status: voyage.Running}); err != nil {
		t.Errorf("non-terminal finish = %v", err)
	}
}
