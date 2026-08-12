package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

func runPlanCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := &cli.Command{Name: "shipmates", Writer: &out, ErrWriter: &out, Commands: []*cli.Command{Plan()}}
	err := root.Run(context.Background(), append([]string{"shipmates", "plan"}, args...))
	return out.String(), err
}

func TestPlanWithoutDraftPointsAtTheWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())
	got, err := runPlanCommand(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no voyage draft", "first mate", "shipmates commission", "docs/sailing.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPlanRejectsInvalidDraftWithTheDefect(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(".shipmates", 0o700); err != nil {
		t.Fatal(err)
	}
	invalid := `{"version":1,"title":"test","objective":"test","commissioned":false,"tasks":[{"id":"verify","persona":"tester","summary":"verify","prompt":"verify","models":["a","b","c"],"efforts":["medium","high","xhigh"],"retry_safe":true}]}`
	if err := os.WriteFile(filepath.Join(".shipmates", "voyage.json"), []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runPlanCommand(t)
	if err == nil || !strings.Contains(err.Error(), "exceeds eight tiers") {
		t.Fatalf("invalid draft error = %v", err)
	}
}

func TestPlanShowsDraftAndCommissionedStatusTruthfully(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(".shipmates", 0o700); err != nil {
		t.Fatal(err)
	}
	plan := voyage.Plan{Version: 1, Title: "done", Objective: "Deliver the report", AcceptanceCriteria: []string{"Report is fact-checked"}, Commissioned: false, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work it", Prompt: "work"}}}
	writePlanDraft(t, plan)
	got, err := runPlanCommand(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DRAFT - awaiting the admiral's commission", "shipmates commission", "Deliver the report", "[pending] work [backend] work it"} {
		if !strings.Contains(got, want) {
			t.Fatalf("draft summary missing %q:\n%s", want, got)
		}
	}
	plan.Commissioned = true
	writePlanDraft(t, plan)
	got, err = runPlanCommand(t)
	if err != nil || !strings.Contains(got, "COMMISSIONED - run: shipmates sail") {
		t.Fatalf("commissioned summary = %q err=%v", got, err)
	}
}

// Task completion is not acceptance: a completed voyage without an explicit
// verdict renders acceptance unknown, never PASS; only a persisted, bound
// verdict flips the projection.
func TestPlanAcceptanceProjectionIsTruthful(t *testing.T) {
	cases := []struct {
		name    string
		verdict *voyage.AcceptanceVerdict
		want    []string
		forbid  []string
	}{
		{name: "unset", verdict: nil, want: []string{"COMPLETED - acceptance unknown"}, forbid: []string{"PASS -", "NO-GO -"}},
		{name: "pass", verdict: &voyage.AcceptanceVerdict{Status: voyage.AcceptancePass, RecordedAt: time.Unix(10, 0).UTC(), EvidenceRefs: []string{"report:pass"}}, want: []string{"COMPLETED - acceptance criteria passed", "PASS - Deliver the report", "PASS - Report is fact-checked"}},
		{name: "no-go", verdict: &voyage.AcceptanceVerdict{Status: voyage.AcceptanceNoGo, RecordedAt: time.Unix(10, 0).UTC(), EvidenceRefs: []string{"report:no-go"}}, want: []string{"COMPLETED - acceptance failed", "NO-GO - Deliver the report"}, forbid: []string{"PASS -"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			if err := os.MkdirAll(".shipmates", 0o700); err != nil {
				t.Fatal(err)
			}
			plan := voyage.Plan{Version: 1, Title: "acceptance", Objective: "Deliver the report", AcceptanceCriteria: []string{"Report is fact-checked"}, AcceptanceGateTask: "gate", Commissioned: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate"}}}
			writePlanDraft(t, plan)
			canonical, _ := json.Marshal(plan)
			hash := voyage.Hash(canonical)
			state := voyage.NewState(&plan, hash)
			entry := state.Tasks["gate"]
			entry.Status = voyage.Completed
			state.Tasks["gate"] = entry
			state.Acceptance = tt.verdict
			if err := voyage.SaveState(filepath.Join(".shipmates", "voyages", hash[:16]+".json"), state); err != nil {
				t.Fatal(err)
			}
			got, err := runPlanCommand(t)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range tt.forbid {
				if strings.Contains(got, forbidden) {
					t.Fatalf("contradictory %q:\n%s", forbidden, got)
				}
			}
		})
	}
}

// A hand-corrupted or binding-mismatched persisted state cannot masquerade as
// a passed voyage: the projection reverts to acceptance unknown.
func TestPlanRejectsMismatchedOrMalformedAcceptanceAsUnknown(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(".shipmates", 0o700); err != nil {
		t.Fatal(err)
	}
	plan := voyage.Plan{Version: 1, Title: "binding", Objective: "Bounded objective", AcceptanceCriteria: []string{"Bounded criterion"}, AcceptanceGateTask: "gate", Commissioned: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate"}}}
	writePlanDraft(t, plan)
	loadedPlan, planBytes, err := voyage.Load(filepath.Join(".shipmates", "voyage.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash := voyage.Hash(planBytes)
	state := voyage.NewState(loadedPlan, hash)
	entry := state.Tasks["gate"]
	entry.Status = voyage.Completed
	state.Tasks["gate"] = entry
	state.Acceptance = &voyage.AcceptanceVerdict{Status: voyage.AcceptancePass, RecordedAt: time.Unix(8, 0).UTC(), EvidenceRefs: []string{"binding:pass"}}
	statePath := filepath.Join(".shipmates", "voyages", hash[:16]+".json")
	if err := voyage.SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	if got, err := runPlanCommand(t); err != nil || !strings.Contains(got, "acceptance criteria passed") {
		t.Fatalf("valid pass missing: %v\n%s", err, got)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	badBinding := bytes.Replace(raw, []byte(state.GlobalFingerprint), []byte(strings.Repeat("f", 64)), 1)
	if bytes.Equal(raw, badBinding) {
		t.Fatal("test did not change binding")
	}
	if err := os.WriteFile(statePath, badBinding, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := runPlanCommand(t)
	if err != nil || !strings.Contains(got, "acceptance unknown") || strings.Contains(got, "PASS -") {
		t.Fatalf("mismatched binding projection: %v\n%s", err, got)
	}
	if err := os.WriteFile(statePath, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = runPlanCommand(t)
	if err != nil || !strings.Contains(got, "acceptance unknown") || strings.Contains(got, "PASS -") {
		t.Fatalf("malformed state projection: %v\n%s", err, got)
	}
}

func TestPlanFreshClearsOnlyTheDraft(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	active := filepath.Join(".shipmates", "voyage.json")
	state := filepath.Join(".shipmates", "voyages", "completed.json")
	report := filepath.Join(".shipmates", "reports", "final.md")
	for _, path := range []string{active, state, report} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runPlanCommand(t, "--fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active draft was not cleared: %v", err)
	}
	for _, path := range []string{state, report} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("durable evidence removed at %s: %v", path, err)
		}
	}
	// Repeated reset is idempotent; a non-regular path is refused.
	if _, err := runPlanCommand(t, "--fresh"); err != nil {
		t.Fatalf("repeated reset: %v", err)
	}
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runPlanCommand(t, "--fresh"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("non-regular draft error = %v", err)
	}
}

func TestInvalidVoyageDraftHashesOnlyRealDefects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voyage.json")
	invalid := `{"version":1,"title":"test","objective":"test","commissioned":false,"tasks":[{"id":"verify","persona":"tester","summary":"verify","prompt":"verify","models":["a","b","c"],"efforts":["medium","high","xhigh"],"retry_safe":true}]}`
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := invalidVoyageDraft(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds eight tiers") {
		t.Fatalf("expected bounded ladder validation error, got %v", err)
	}
	if hash == ([32]byte{}) {
		t.Fatal("invalid draft must return a stable content hash")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := invalidVoyageDraft(path); err != nil {
		t.Fatalf("missing draft is normal before planning begins: %v", err)
	}
}

func writePlanDraft(t *testing.T, plan voyage.Plan) {
	t.Helper()
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".shipmates", "voyage.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
