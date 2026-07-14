package voyage

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lineagePlan() Plan {
	return Plan{Version: 1, Title: "Voyage", Objective: "Execute safely", Scope: []string{"backend"}, Approved: true, Tasks: []Task{
		{ID: "task-one", Persona: "backend", Summary: "First", Prompt: "complete first"},
		{ID: "task-two", Persona: "tester", Summary: "Second", Prompt: "complete second", DependsOn: []string{"task-one"}},
		{ID: "task-three", Persona: "security", Summary: "Third", Prompt: "complete third", DependsOn: []string{"task-two"}},
	}}
}

func writeLineagePlan(t *testing.T, path string, plan Plan) (string, []byte) {
	t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return Hash(b), b
}

func completedLineageState(t *testing.T, plan *Plan, hash, path string) []byte {
	t.Helper()
	now := time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC)
	s := NewState(plan, hash)
	for id, bead := range map[string]string{"task-one": "Shipmates-auz", "task-two": "ship-2", "task-three": "ship-3"} {
		e := s.Tasks[id]
		e.Status, e.Summary, e.FinishedAt, e.BeadID = Completed, "evidence for "+id, now, bead
		s.Tasks[id] = e
	}
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMigrateSuccessorInheritsOnlyUnchangedClosure(t *testing.T) {
	dir := t.TempDir()
	prePlanPath, successorPlanPath := filepath.Join(dir, "predecessor.json"), filepath.Join(dir, "successor.json")
	preStatePath, successorStatePath := filepath.Join(dir, "predecessor-state.json"), filepath.Join(dir, "successor-state.json")
	pre := lineagePlan()
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preBytes := completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Tasks[1].Prompt = "amended second"
	writeLineagePlan(t, successorPlanPath, successor)
	got, err := MigrateSuccessor(successorPlanPath, prePlanPath, preStatePath, successorStatePath, time.Date(2026, 7, 14, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Tasks["task-one"].Status != Completed || got.Tasks["task-one"].Inherited == nil || got.Tasks["task-one"].BeadID != "Shipmates-auz" {
		t.Fatalf("task one was not inherited: %+v", got.Tasks["task-one"])
	}
	if got.Tasks["task-two"].Status != Pending || got.Tasks["task-two"].Inherited != nil || got.Tasks["task-three"].Status != Pending {
		t.Fatalf("changed/downstream tasks not invalidated: %+v %+v", got.Tasks["task-two"], got.Tasks["task-three"])
	}
	if got.Tasks["task-one"].Inherited.PredecessorPlanHash != preHash || got.Tasks["task-one"].Inherited.Summary != "evidence for task-one" {
		t.Fatalf("inherited provenance = %+v", got.Tasks["task-one"].Inherited)
	}
	after, err := os.ReadFile(preStatePath)
	if err != nil || !bytes.Equal(preBytes, after) {
		t.Fatal("predecessor state was modified")
	}
	second, err := MigrateSuccessor(successorPlanPath, prePlanPath, preStatePath, successorStatePath, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || second.Lineage == nil || second.Lineage.CreatedAt != got.Lineage.CreatedAt {
		t.Fatalf("retry was not idempotent: %+v %v", second, err)
	}
}

func TestMigrateSuccessorConservativeGlobalAndEvidenceInvalidation(t *testing.T) {
	dir := t.TempDir()
	prePlanPath, successorPlanPath := filepath.Join(dir, "predecessor.json"), filepath.Join(dir, "successor.json")
	preStatePath, successorStatePath := filepath.Join(dir, "predecessor-state.json"), filepath.Join(dir, "successor-state.json")
	pre := lineagePlan()
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	s := NewState(&pre, preHash)
	for id := range s.Tasks {
		e := s.Tasks[id]
		e.Status, e.Summary = Completed, "evidence"
		s.Tasks[id] = e
	}
	if err := SaveState(preStatePath, s); err != nil {
		t.Fatal(err)
	}
	successor := pre
	successor.Objective = "changed authorization meaning"
	writeLineagePlan(t, successorPlanPath, successor)
	got, err := MigrateSuccessor(successorPlanPath, prePlanPath, preStatePath, successorStatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for id, entry := range got.Tasks {
		if entry.Status != Pending || entry.Inherited != nil {
			t.Fatalf("global change inherited %s: %+v", id, entry)
		}
	}
	if _, err := os.Stat(successorStatePath); err != nil {
		t.Fatal(err)
	}

	// A completed state without durable completion time is not evidence.
	successorStatePath = filepath.Join(dir, "successor-no-evidence.json")
	successor = pre
	successor.Tasks[1].Prompt = "changed task without evidence"
	writeLineagePlan(t, successorPlanPath, successor)
	got, err = MigrateSuccessor(successorPlanPath, prePlanPath, preStatePath, successorStatePath, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got.Tasks["task-one"].Status != Pending {
		t.Fatal("missing completion evidence was inherited")
	}
}

func TestMigrateSuccessorRejectsUnapprovedOrUnsafePredecessor(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.json")
	plan := lineagePlan()
	hash, _ := writeLineagePlan(t, planPath, plan)
	statePath := filepath.Join(dir, "state.json")
	completedLineageState(t, &plan, hash, statePath)
	unapproved := plan
	unapproved.Approved = false
	unapprovedPath := filepath.Join(dir, "unapproved.json")
	writeLineagePlan(t, unapprovedPath, unapproved)
	if _, err := MigrateSuccessor(planPath, unapprovedPath, statePath, filepath.Join(dir, "out.json"), time.Now().UTC()); err == nil {
		t.Fatal("unapproved predecessor accepted")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(statePath, link); err == nil {
		if _, err := MigrateSuccessor(planPath, planPath, link, filepath.Join(dir, "out2.json"), time.Now().UTC()); err == nil {
			t.Fatal("symlink predecessor accepted")
		}
	}
}

func TestMigrateSuccessorInvalidatesEveryTaskAndGlobalContractMutation(t *testing.T) {
	globalMutations := map[string]func(*Plan){
		"title":          func(p *Plan) { p.Title = "renamed" },
		"objective":      func(p *Plan) { p.Objective = "different objective" },
		"scope":          func(p *Plan) { p.Scope = []string{"security"} },
		"non-goals":      func(p *Plan) { p.NonGoals = []string{"new exclusion"} },
		"blast-area":     func(p *Plan) { p.BlastArea = []string{"new area"} },
		"risks":          func(p *Plan) { p.Risks = []string{"new risk"} },
		"acceptance":     func(p *Plan) { p.AcceptanceCriteria = []string{"new criterion"} },
		"open-decisions": func(p *Plan) { p.OpenDecisions = []string{"new decision"} },
	}
	for name, mutate := range globalMutations {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			prePlanPath := filepath.Join(dir, "predecessor.json")
			pre := lineagePlan()
			preHash, _ := writeLineagePlan(t, prePlanPath, pre)
			preStatePath := filepath.Join(dir, "predecessor-state.json")
			completedLineageState(t, &pre, preHash, preStatePath)
			successor := pre
			mutate(&successor)
			successorPath := filepath.Join(dir, "successor.json")
			writeLineagePlan(t, successorPath, successor)
			got, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "out.json"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			for id, entry := range got.Tasks {
				if entry.Status != Pending || entry.Inherited != nil {
					t.Fatalf("global mutation %s inherited %s: %+v", name, id, entry)
				}
			}
		})
	}
	// Version changes are rejected by the strict plan schema rather than
	// treated as an equivalent global contract.
	dir := t.TempDir()
	pre := lineagePlan()
	prePlanPath := filepath.Join(dir, "predecessor.json")
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "predecessor-state.json")
	completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Version = 2
	successorPath := filepath.Join(dir, "version-successor.json")
	writeLineagePlan(t, successorPath, successor)
	if _, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "version-out.json"), time.Now().UTC()); err == nil {
		t.Fatal("unsupported successor version accepted")
	}
}

func TestMigrateSuccessorInvalidatesTaskContractAndClosureConservatively(t *testing.T) {
	mutations := map[string]func(*Task){
		"summary":    func(task *Task) { task.Summary = "changed summary" },
		"prompt":     func(task *Task) { task.Prompt = "changed prompt" },
		"persona":    func(task *Task) { task.Persona = "security" },
		"models":     func(task *Task) { task.Models = []string{"small"} },
		"efforts":    func(task *Task) { task.Efforts = []string{"high"} },
		"retry":      func(task *Task) { task.RetrySafe = !task.RetrySafe },
		"dependency": func(task *Task) { task.DependsOn = nil },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			prePlanPath := filepath.Join(dir, "predecessor.json")
			pre := lineagePlan()
			preHash, _ := writeLineagePlan(t, prePlanPath, pre)
			preStatePath := filepath.Join(dir, "predecessor-state.json")
			completedLineageState(t, &pre, preHash, preStatePath)
			successor := pre
			mutate(&successor.Tasks[1])
			if name == "models" {
				successor.Tasks[1].RetrySafe = true
			}
			successorPath := filepath.Join(dir, "successor.json")
			writeLineagePlan(t, successorPath, successor)
			got, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "out.json"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if got.Tasks["task-one"].Status != Completed || got.Tasks["task-one"].Inherited == nil {
				t.Fatalf("unchanged prerequisite was not inherited for %s: %+v", name, got.Tasks["task-one"])
			}
			if got.Tasks["task-two"].Status != Pending || got.Tasks["task-three"].Status != Pending {
				t.Fatalf("changed task or downstream closure inherited for %s: %+v", name, got.Tasks)
			}
		})
	}

	// Changing the root task invalidates every transitive dependent, even when
	// each dependent's own local contract is unchanged.
	dir := t.TempDir()
	prePlanPath := filepath.Join(dir, "predecessor.json")
	pre := lineagePlan()
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "predecessor-state.json")
	completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Tasks[0].Prompt = "changed root"
	successorPath := filepath.Join(dir, "successor.json")
	writeLineagePlan(t, successorPath, successor)
	got, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "out.json"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for id, entry := range got.Tasks {
		if entry.Status != Pending || entry.Inherited != nil {
			t.Fatalf("root mutation inherited %s: %+v", id, entry)
		}
	}
}

func TestMigrateSuccessorRequiresCompletedEvidenceAndStrictPredecessorState(t *testing.T) {
	statuses := []Status{Pending, Running, Failed, Blocked, NeedsInput}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			prePlanPath := filepath.Join(dir, "predecessor.json")
			pre := lineagePlan()
			preHash, _ := writeLineagePlan(t, prePlanPath, pre)
			preStatePath := filepath.Join(dir, "predecessor-state.json")
			completedLineageState(t, &pre, preHash, preStatePath)
			state, err := LoadStateStrict(preStatePath, &pre, preHash)
			if err != nil {
				t.Fatal(err)
			}
			entry := state.Tasks["task-one"]
			entry.Status, entry.FinishedAt = status, time.Time{}
			state.Tasks["task-one"] = entry
			if err := SaveState(preStatePath, state); err != nil {
				t.Fatal(err)
			}
			successor := pre
			successor.Tasks[1].Prompt = "changed successor task"
			successorPath := filepath.Join(dir, "successor.json")
			writeLineagePlan(t, successorPath, successor)
			got, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "out.json"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if got.Tasks["task-one"].Status != Pending || got.Tasks["task-one"].Inherited != nil {
				t.Fatalf("non-completed state %s was inherited: %+v", status, got.Tasks["task-one"])
			}
		})
	}
}

func TestMigrateSuccessorRejectsMalformedStateAndUnsafeAncestor(t *testing.T) {
	dir := t.TempDir()
	prePlanPath := filepath.Join(dir, "predecessor.json")
	pre := lineagePlan()
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "predecessor-state.json")
	completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Tasks[1].Prompt = "changed"
	successorPath := filepath.Join(dir, "successor.json")
	writeLineagePlan(t, successorPath, successor)

	original, err := os.ReadFile(preStatePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"trailing":  append(append([]byte(nil), original...), []byte("{}\n")...),
		"malformed": []byte("{"),
		"oversized": append([]byte{'{'}, bytes.Repeat([]byte{'x'}, MaxStateBytes+1)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(preStatePath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, name+"-out.json"), time.Now().UTC()); err == nil {
				t.Fatal("malformed predecessor state accepted")
			}
		})
		if err := os.WriteFile(preStatePath, original, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	unsafeRoot := filepath.Join(dir, "unsafe")
	outside := t.TempDir()
	if err := os.Symlink(outside, unsafeRoot); err != nil {
		t.Fatal(err)
	}
	unsafeState := filepath.Join(unsafeRoot, "state.json")
	if err := os.WriteFile(filepath.Join(outside, "state.json"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSuccessor(successorPath, prePlanPath, unsafeState, filepath.Join(dir, "unsafe-out.json"), time.Now().UTC()); err == nil {
		t.Fatal("state through symlinked ancestor accepted")
	}
}

func TestMigrateSuccessorConcurrentPublicationAndAtomicRetry(t *testing.T) {
	dir := t.TempDir()
	prePlanPath := filepath.Join(dir, "predecessor.json")
	pre := lineagePlan()
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "predecessor-state.json")
	preBytes := completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Tasks[1].Prompt = "changed"
	successorPath := filepath.Join(dir, "successor.json")
	writeLineagePlan(t, successorPath, successor)
	out := filepath.Join(dir, "successor-state.json")
	now := time.Date(2026, 7, 14, 5, 0, 0, 0, time.UTC)
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			_, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, out, now)
			results <- err
		}()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent migration failed: %v", err)
		}
	}
	state, err := LoadStateStrict(out, &successor, Hash(mustRead(t, successorPath)))
	if err != nil || state.Tasks["task-one"].Inherited == nil {
		t.Fatalf("published state = %+v err=%v", state, err)
	}
	if after := mustRead(t, preStatePath); !bytes.Equal(preBytes, after) {
		t.Fatal("concurrent migration modified predecessor bytes")
	}
	corrupt := []byte("not a successor")
	if err := os.WriteFile(out, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, out, now); err == nil {
		t.Fatal("corrupt existing successor was accepted")
	}
	if after := mustRead(t, out); !bytes.Equal(corrupt, after) {
		t.Fatal("failed retry overwrote corrupt successor")
	}
}

func TestMigrateSuccessorRejectsStaleProvenanceHashMismatchAndAmbiguousPredecessor(t *testing.T) {
	dir := t.TempDir()
	pre := lineagePlan()
	prePlanPath := filepath.Join(dir, "predecessor.json")
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "predecessor-state.json")
	completedLineageState(t, &pre, preHash, preStatePath)
	successor := pre
	successor.Tasks = append([]Task(nil), pre.Tasks...)
	successor.Tasks[1].Prompt = "changed successor"
	successorPath := filepath.Join(dir, "successor.json")
	writeLineagePlan(t, successorPath, successor)

	state, err := LoadStateStrict(preStatePath, &pre, preHash)
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Tasks["task-one"]
	entry.Inherited = &InheritedTask{PredecessorPlanHash: strings.Repeat("a", 64), PredecessorTaskID: "task-one", TaskFingerprint: strings.Repeat("b", 64), ClosureFingerprint: strings.Repeat("c", 64), FinishedAt: time.Now().UTC()}
	state.Tasks["task-one"] = entry
	if err := SaveState(preStatePath, state); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "stale-out.json"), time.Now().UTC()); err == nil {
		t.Fatal("stale inherited provenance accepted")
	}

	state = NewState(&pre, strings.Repeat("f", 64))
	if err := SaveState(preStatePath, state); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "hash-out.json"), time.Now().UTC()); err == nil {
		t.Fatal("predecessor state with mismatched plan hash accepted")
	}
	validState := NewState(&pre, preHash)
	if err := SaveState(preStatePath, validState); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateSuccessor(prePlanPath, prePlanPath, preStatePath, filepath.Join(dir, "ambiguous-out.json"), time.Now().UTC()); err == nil {
		t.Fatal("same predecessor/successor revision accepted")
	}
}

func TestFleetShapedAmendmentPreservesCompletedTaskOne(t *testing.T) {
	// This fixture mirrors the preserved Fleet voyage's public plan/state shape
	// and opaque Bead evidence without importing credentials or Codex data.
	pre := Plan{Version: 1, Title: "Validate Fleet Command and adapt Codex for v0.4.0-beta.2", Objective: "Observe and control already-active turns safely", Scope: []string{"bootstrap", "observation", "control"}, NonGoals: []string{"remote start", "shell", "file transfer"}, BlastArea: []string{"Fleet identity", "observer", "exact-turn control"}, Risks: []string{"stale targets", "credential leakage"}, AcceptanceCriteria: []string{"bounded observation", "exact-turn refusal"}, OpenDecisions: []string{"Linux and WSL only"}, Approved: true, Tasks: []Task{
		{ID: "zero-to-observed-real-codex", Persona: "backend", Summary: "Build the public bootstrap and prove zero-to-observed real Codex", Prompt: "Use public commands to prove one already-active turn", Models: []string{"gpt-5.6-luna", "gpt-5.6-terra"}, Efforts: []string{"medium", "high"}, RetrySafe: true},
		{ID: "real-exact-turn-control", Persona: "backend", Summary: "Prove real TLS observation, steering, and interruption", Prompt: "Use the completed first-task state and prove exact-turn control", DependsOn: []string{"zero-to-observed-real-codex"}, Models: []string{"gpt-5.6-luna", "gpt-5.6-terra"}, Efforts: []string{"medium", "high"}, RetrySafe: true},
		{ID: "security-lifecycle-boundaries", Persona: "security", Summary: "Verify credential lifecycle and privacy", Prompt: "Audit the control surface", DependsOn: []string{"real-exact-turn-control"}},
	}}
	dir := t.TempDir()
	prePlanPath := filepath.Join(dir, "fleet-original.json")
	preHash, _ := writeLineagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(dir, "fleet-original-state.json")
	preState := NewState(&pre, preHash)
	firstState := preState.Tasks["zero-to-observed-real-codex"]
	firstState.Status, firstState.Summary, firstState.FinishedAt, firstState.BeadID = Completed, "observed real Codex evidence", time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC), "Shipmates-auz"
	preState.Tasks["zero-to-observed-real-codex"] = firstState
	if err := SaveState(preStatePath, preState); err != nil {
		t.Fatal(err)
	}
	preBytes := mustRead(t, preStatePath)
	successor := pre
	successor.Tasks[1].Prompt = "Changed exact-turn control contract"
	successorPath := filepath.Join(dir, "fleet-amended.json")
	writeLineagePlan(t, successorPath, successor)
	got, err := MigrateSuccessor(successorPath, prePlanPath, preStatePath, filepath.Join(dir, "fleet-amended-state.json"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	first := got.Tasks["zero-to-observed-real-codex"]
	if first.Status != Completed || first.Inherited == nil || first.BeadID != "Shipmates-auz" || first.Inherited.OriginalBeadID != "Shipmates-auz" {
		t.Fatalf("Fleet Task 1 inheritance = %+v", first)
	}
	if got.Tasks["real-exact-turn-control"].Status != Pending || got.Tasks["security-lifecycle-boundaries"].Status != Pending {
		t.Fatalf("Fleet changed/downstream tasks were inherited: %+v", got.Tasks)
	}
	if after := mustRead(t, preStatePath); !bytes.Equal(preBytes, after) {
		t.Fatal("Fleet predecessor state changed")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
