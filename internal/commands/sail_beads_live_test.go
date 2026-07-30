//go:build unix

// prepareSailBeads and ensureDerivativeTask persist voyage state through
// voyage.SaveState, which finishes with a directory fsync that Windows refuses
// ("Access is denied") — the same pre-existing limitation that keeps
// internal/voyage off the Windows CI matrix. The Beads *mirroring* path is
// therefore unix-only today even though nothing in internal/beads is. The
// portable half is covered on Windows by sail_beads_portable_live_test.go and by
// internal/beads/live_test.go.
package commands

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/luthermonson/shipmates/internal/beads/bdtest"
	"github.com/luthermonson/shipmates/internal/voyage"
)

// TestPrepareSailBeadsAgainstRealBD mirrors a voyage plan into a real Beads
// workspace and then asks bd what it actually stored. Unlike the shell-script
// stand-in in sail_beads_test.go, this proves the graph shipmates believes it
// built exists in bd: ids come back parsable, dependency edges are real, the
// injected prompt carries genuine `bd prime` and `bd show` output, and the
// lifecycle transitions land on real bd statuses.
//
// prepareSailBeads is platform-independent (only Sail's scheduler is unix-only),
// so this runs on Windows too.
func TestPrepareSailBeadsAgainstRealBD(t *testing.T) {
	bd, root := bdtest.Workspace(t)
	bdtest.OnPath(t, bd)
	t.Chdir(root)

	plan := &voyage.Plan{
		Version: 1, Title: "Beads voyage", Objective: "Prove graph mirroring against real bd", Approved: true,
		Tasks: []voyage.Task{
			{ID: "design", Persona: "architect", Summary: "Design the adapter", Prompt: "Produce a design"},
			{ID: "build", Persona: "backend", Summary: "Build the adapter", Prompt: "Implement the design", DependsOn: []string{"design"}},
		},
	}
	hash := strings.Repeat("a", 64)
	state := voyage.NewState(plan, hash)
	statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")

	graph, err := prepareSailBeads(context.Background(), plan, state, hash, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if graph == nil || graph.client == nil {
		t.Fatal("prepareSailBeads returned no Beads graph inside a real workspace")
	}

	design, build := state.Tasks["design"].BeadID, state.Tasks["build"].BeadID
	if design == "" || build == "" || design == build {
		t.Fatalf("bead ids = %q, %q", design, build)
	}
	if !state.Tasks["build"].BeadDependenciesLinked {
		t.Fatal("dependency link was not persisted")
	}

	// bd is the authority: verify the mirrored graph from bd's own records.
	for taskID, beadID := range map[string]string{"design": design, "build": build} {
		record := bdtest.Issue(t, bd, root, beadID)
		task := plan.Tasks[0]
		if taskID == "build" {
			task = plan.Tasks[1]
		}
		if got := bdtest.Field(t, record, "title"); got != task.Summary {
			t.Errorf("%s title = %q, want %q", taskID, got, task.Summary)
		}
		if got := bdtest.Field(t, record, "assignee"); got != task.Persona {
			t.Errorf("%s assignee = %q, want %q", taskID, got, task.Persona)
		}
		if got := bdtest.Field(t, record, "external_ref"); got != "shipmates:voyage:"+hash[:16]+":"+taskID {
			t.Errorf("%s external_ref = %q", taskID, got)
		}
	}
	if !bdtest.DependsOn(t, bd, root, build, design) {
		t.Fatalf("bd does not record %s depending on %s:\n%s", build, design, bdtest.Run(t, bd, root, "dep", "list", build))
	}

	// The prompt carries real bd output, not a canned sentence.
	prompt := graph.prompt("base prompt", plan.Tasks[1], state.Tasks["build"])
	for _, want := range []string{"base prompt", "BEADS TASK GRAPH", "Bead: " + build, "bd show " + build + " --json", "Beads Workflow Context", `"status": "open"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// Lifecycle, in the order sail actually produces it: a task is only
	// dispatched once its dependencies completed, so Beads are closed
	// dependency-first. Real bd enforces that ordering — it refuses to close an
	// issue whose blockers are still open — which the shell-script stand-in
	// cannot show because it exits 0 for every subcommand.
	entry := state.Tasks["build"]
	if err := graph.start(context.Background(), plan.Tasks[1], entry); err != nil {
		t.Fatal(err)
	}
	if got := bdtest.Field(t, bdtest.Issue(t, bd, root, build), "status"); got != "in_progress" {
		t.Errorf("after start bd status = %q, want in_progress", got)
	}

	designEntry := state.Tasks["design"]
	designEntry.Status, designEntry.Summary = voyage.Completed, "design delivered"
	if err := graph.finish(context.Background(), designEntry); err != nil {
		t.Fatal(err)
	}
	if got := bdtest.Field(t, bdtest.Issue(t, bd, root, design), "status"); got != "closed" {
		t.Errorf("after design finish bd status = %q, want closed", got)
	}

	entry.Status, entry.Summary = voyage.Completed, "real bd verified the closure"
	if err := graph.finish(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	closed := bdtest.Issue(t, bd, root, build)
	if got := bdtest.Field(t, closed, "status"); got != "closed" {
		t.Errorf("after finish bd status = %q, want closed", got)
	}
	if got := bdtest.Comments(t, bd, root, build); !strings.Contains(got, "real bd verified the closure") {
		t.Errorf("finish did not record the crew summary:\n%s", got)
	}

	// ensureDerivativeTask creates only new successor work, wired to its parent.
	derivative := voyage.Task{ID: "followup", Persona: "tester", Summary: "Verify the adapter", Prompt: "Run the suite", DependsOn: []string{"build"}}
	plan.Tasks = append(plan.Tasks, derivative)
	state.Tasks["followup"] = voyage.TaskState{}
	if err := graph.ensureDerivativeTask(context.Background(), derivative, hash, state, statePath); err != nil {
		t.Fatal(err)
	}
	followup := state.Tasks["followup"].BeadID
	if followup == "" {
		t.Fatal("derivative bead was not created")
	}
	if !bdtest.DependsOn(t, bd, root, followup, build) {
		t.Fatalf("derivative bead %s is not linked to %s", followup, build)
	}
	// A second call must be a no-op rather than a duplicate.
	before := len(bdtest.OpenIDs(t, bd, root))
	if err := graph.ensureDerivativeTask(context.Background(), derivative, hash, state, statePath); err != nil {
		t.Fatal(err)
	}
	if after := len(bdtest.OpenIDs(t, bd, root)); after != before {
		t.Fatalf("ensureDerivativeTask duplicated work: %d beads -> %d", before, after)
	}

	followupEntry := state.Tasks["followup"]
	followupEntry.Status, followupEntry.Error = voyage.Blocked, "needs a Captain decision"
	if err := graph.finish(context.Background(), followupEntry); err != nil {
		t.Fatal(err)
	}
	if got := bdtest.Field(t, bdtest.Issue(t, bd, root, followup), "status"); got != "blocked" {
		t.Errorf("after blocked finish bd status = %q, want blocked", got)
	}
	if got := bdtest.Comments(t, bd, root, followup); !strings.Contains(got, "needs a Captain decision") {
		t.Errorf("blocked finish did not record its reason:\n%s", got)
	}
}

// TestPrepareSailBeadsInheritedPrerequisiteAgainstRealBD covers the amendment
// path, where task one's completion — and its Bead — are inherited from the
// predecessor voyage and are read-only.
//
// The "absent" case is the one the shell-script stand-in structurally could not
// catch: that fake exits 0 for `dep add`, while real bd rejects a dependency on
// an id it cannot resolve. A predecessor state carried in from another checkout,
// or a `bd prune`/`bd gc` that reclaimed the closed predecessor Bead, leaves an
// inherited Bead id that no longer resolves. Treating that provenance edge as a
// dispatch prerequisite refused the entire successor voyage.
func TestPrepareSailBeadsInheritedPrerequisiteAgainstRealBD(t *testing.T) {
	for _, mode := range []string{"predecessor bead present", "predecessor bead absent"} {
		t.Run(mode, func(t *testing.T) {
			bd, root := bdtest.Workspace(t)
			bdtest.OnPath(t, bd)
			t.Chdir(root)

			plan := &voyage.Plan{Version: 1, Title: "successor", Objective: "inherit safely", Approved: true, Tasks: []voyage.Task{
				{ID: "one", Persona: "backend", Summary: "Already done upstream", Prompt: "one"},
				{ID: "two", Persona: "tester", Summary: "Still to do", Prompt: "two", DependsOn: []string{"one"}},
			}}
			hash := strings.Repeat("b", 64)
			state := voyage.NewState(plan, hash)

			inheritedBead := "ship-was-pruned-away"
			preexisting := 0
			if mode == "predecessor bead present" {
				// The realistic amendment: the predecessor voyage ran in this same
				// workspace, so its closed Bead is still resolvable.
				client, err := beads.New(root)
				if err != nil {
					t.Fatal(err)
				}
				inheritedBead, err = client.CreateTask(context.Background(), beads.Task{Title: "Already done upstream", Assignee: "backend"})
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Complete(context.Background(), inheritedBead, "original completion"); err != nil {
					t.Fatal(err)
				}
				preexisting = 1
			}
			state.Tasks["one"] = voyage.TaskState{
				Status: voyage.Completed, BeadID: inheritedBead,
				Inherited: &voyage.InheritedTask{
					PredecessorPlanHash: strings.Repeat("c", 64), PredecessorTaskID: "one",
					TaskFingerprint: strings.Repeat("d", 64), ClosureFingerprint: strings.Repeat("e", 64),
					Summary: "original", FinishedAt: time.Now().UTC(), OriginalBeadID: inheritedBead,
				},
			}
			statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")

			if _, err := prepareSailBeads(context.Background(), plan, state, hash, statePath); err != nil {
				t.Fatalf("successor voyage refused to prepare: %v", err)
			}
			pending := state.Tasks["two"].BeadID
			if pending == "" {
				t.Fatal("pending successor task got no bead")
			}
			ids := bdtest.OpenIDs(t, bd, root)
			if len(ids) != preexisting+1 {
				t.Fatalf("bd holds %d beads, want %d: %v", len(ids), preexisting+1, ids)
			}
			if state.Tasks["one"].BeadID != inheritedBead {
				t.Fatalf("inherited bead id was rewritten to %q", state.Tasks["one"].BeadID)
			}
			if got := bdtest.Field(t, bdtest.Issue(t, bd, root, pending), "title"); got != "Still to do" {
				t.Fatalf("pending bead title = %q", got)
			}
			if !state.Tasks["two"].BeadDependenciesLinked {
				t.Fatal("dependency linking was not marked complete")
			}
			// The edge is recorded when the predecessor Bead resolves and quietly
			// skipped when it does not.
			if got := bdtest.DependsOn(t, bd, root, pending, inheritedBead); got != (mode == "predecessor bead present") {
				t.Fatalf("DependsOn(%s, %s) = %v in mode %q", pending, inheritedBead, got, mode)
			}
		})
	}
}
