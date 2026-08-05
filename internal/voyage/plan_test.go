package voyage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validPlan() Plan {
	return Plan{Version: 1, Title: "Release", Objective: "Ship safely", Commissioned: true, Tasks: []Task{
		{ID: "build", Persona: "backend", Summary: "Build it", Prompt: "Implement and test."},
		{ID: "verify", Persona: "tester", Summary: "Verify it", Prompt: "Run release tests.", DependsOn: []string{"build"}},
	}}
}

func TestPlanValidateAndOrder(t *testing.T) {
	p := validPlan()
	order, err := p.Order()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "build,verify" {
		t.Fatalf("order = %v", order)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDraftAcceptsUncommissionedStructuredPlanningFields(t *testing.T) {
	p := validPlan()
	p.Commissioned = false
	p.Scope = []string{"API"}
	p.BlastArea = []string{"Worker"}
	p.AcceptanceCriteria = []string{"Tests pass"}
	b, _ := json.Marshal(p)
	path := filepath.Join(t.TempDir(), "draft.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadDraft(path)
	if err != nil || got.Commissioned || got.BlastArea[0] != "Worker" {
		t.Fatalf("draft = %+v, %v", got, err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not commissioned") {
		t.Fatalf("execution accepted draft: %v", err)
	}
}

func TestPlanRejectsUncommissionedCyclesAndUnknownFields(t *testing.T) {
	uncommissioned := validPlan()
	uncommissioned.Commissioned = false
	cycle := Plan{Version: 1, Title: "x", Objective: "y", Commissioned: true, Tasks: []Task{
		{ID: "one", Persona: "backend", Summary: "one", Prompt: "one", DependsOn: []string{"two"}},
		{ID: "two", Persona: "tester", Summary: "two", Prompt: "two", DependsOn: []string{"one"}},
	}}
	for name, plan := range map[string]Plan{"uncommissioned": uncommissioned, "cycle": cycle} {
		t.Run(name, func(t *testing.T) {
			if err := plan.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"title":"x","objective":"y","commissioned":true,"tasks":[],"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"title":"x","objective":"y","commissioned":true,"tasks":[]} garbage`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("expected malformed trailing content error")
	}
}

func TestPlanRejectsDescendingEffortLadder(t *testing.T) {
	p := validPlan()
	p.Tasks[0].Efforts = []string{"high", "low"}
	p.Tasks[0].RetrySafe = true
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "low to high") {
		t.Fatalf("descending effort error = %v", err)
	}
}

func TestStateRoundTripAndRunningRecovery(t *testing.T) {
	p := validPlan()
	hash := strings.Repeat("a", 64)
	state := NewState(&p, hash)
	entry := state.Tasks["build"]
	entry.Status = Running
	state.Tasks["build"] = entry
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path, &p, hash)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tasks["build"].Status != Pending {
		t.Fatalf("running task did not recover to pending: %#v", loaded.Tasks["build"])
	}
}

func TestPublishSuccessorRecoversOrphanedRunningTask(t *testing.T) {
	p := validPlan()
	hash := strings.Repeat("c", 64)
	lineage := &Lineage{
		PredecessorPlanHash:  strings.Repeat("d", 64),
		PredecessorStateHash: strings.Repeat("e", 64),
		CreatedAt:            NewState(&p, hash).StartedAt,
	}
	want := NewState(&p, hash)
	want.Lineage = lineage
	existing := NewState(&p, hash)
	existing.Lineage = lineage
	entry := existing.Tasks["build"]
	entry.Status = Running
	existing.Tasks["build"] = entry

	path := filepath.Join(t.TempDir(), "successor.json")
	if err := SaveState(path, existing); err != nil {
		t.Fatal(err)
	}
	got, err := PublishSuccessor(path, want, &p, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tasks["build"].Status != Pending {
		t.Fatalf("orphaned successor task was not recovered: %#v", got.Tasks["build"])
	}
	persisted, err := LoadStateStrict(path, &p, hash)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Tasks["build"].Status != Pending {
		t.Fatalf("recovery was not persisted: %#v", persisted.Tasks["build"])
	}
}

func TestStateRejectsUnknownExtraInvalidAndSymlink(t *testing.T) {
	p := validPlan()
	hash := strings.Repeat("b", 64)
	state := NewState(&p, hash)
	path := filepath.Join(t.TempDir(), "state.json")

	bad := *state
	bad.Tasks = map[string]TaskState{}
	for id, entry := range state.Tasks {
		bad.Tasks[id] = entry
	}
	bad.Tasks["extra"] = TaskState{Status: Completed}
	b, _ := json.Marshal(bad)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path, &p, hash); err == nil {
		t.Fatal("extra task accepted")
	}

	bad = *state
	bad.Tasks = map[string]TaskState{}
	for id, entry := range state.Tasks {
		bad.Tasks[id] = entry
	}
	entry := bad.Tasks["build"]
	entry.Status = "invented"
	bad.Tasks["build"] = entry
	b, _ = json.Marshal(bad)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path, &p, hash); err == nil {
		t.Fatal("invalid status accepted")
	}

	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "link.json")
	if err := os.Symlink(path, link); err == nil {
		if _, err := LoadState(link, &p, hash); err == nil {
			t.Fatal("symlink state accepted")
		}
	}
}

func TestSaveStateRejectsSymlinkedParentDirectory(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".shipmates"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(root, ".shipmates", "voyages")); err != nil {
		t.Fatal(err)
	}
	p := validPlan()
	state := NewState(&p, "hash")
	err := SaveState(filepath.Join(root, ".shipmates", "voyages", "state.json"), state)
	if err == nil || !strings.Contains(err.Error(), "must not contain symlinks") {
		t.Fatalf("symlinked parent error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state escaped repository: %v", err)
	}
}
