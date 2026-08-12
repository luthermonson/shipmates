package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
)

func structuredTaskFixture() voyage.Task {
	return voyage.Task{ID: "structured", Persona: "crew", Summary: "structured", Prompt: "structured", RetrySafe: true, Recovery: &voyage.RecoveryTask{Enabled: true, MaxAttempts: 4, MaxInfrastructureRetries: 2, MaxTokens: 1000, Models: []string{"luna", "terra"}, Efforts: []string{"medium", "high"}, CorrectiveTemplates: []voyage.CorrectiveTemplate{{ID: "fix", Summary: "fix", Prompt: "fix", VerificationSummary: "verify", VerificationPrompt: "verify", RetrySafe: true}}}}
}

func TestStructuredAttemptReservationRefusesIndeterminateReplay(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(".shipmates", "voyages"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{Version: 1, Title: "p", Objective: "o", Commissioned: true, Tasks: []voyage.Task{structuredTaskFixture()}}
	task := plan.Tasks[0]
	entry := voyage.TaskState{Attempt: 0, TaskFingerprint: voyage.TaskFingerprint(task)}
	hash := voyage.Hash([]byte(`{"plan":"p"}`))
	ledger, attempt, err := prepareStructuredAttempt(hash, plan, task, entry)
	if err != nil || attempt == "" || ledger == nil {
		t.Fatalf("prepare=%v %q", err, attempt)
	}
	if _, _, err := prepareStructuredAttempt(hash, plan, task, entry); !errors.Is(err, errIndeterminateStructuredTurn) {
		t.Fatalf("replay err=%v", err)
	}
	if _, err := ledger.Append("infrastructure_retry", attempt, recovery.DispatchRecord{AttemptID: attempt, Model: "luna", Effort: "medium"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareStructuredAttempt(hash, plan, task, entry); err != nil {
		t.Fatalf("retry prepare=%v", err)
	}
}

func TestStructuredSailConfigIsEffortFirst(t *testing.T) {
	t.Chdir(t.TempDir())
	task := structuredTaskFixture()
	for i, want := range [][2]string{{"luna", "medium"}, {"luna", "high"}, {"terra", "medium"}, {"terra", "high"}} {
		cfg, err := sailTaskConfig(task, i)
		if err != nil || cfg.Model != want[0] || cfg.Effort != want[1] {
			t.Fatalf("tier %d cfg=%+v err=%v", i, cfg, err)
		}
	}
}

func TestStructuredPresentationIsBoundedAndReloadable(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(".shipmates", "voyages"), 0o700); err != nil {
		t.Fatal(err)
	}
	task := structuredTaskFixture()
	plan := &voyage.Plan{Version: 1, Title: "semantic voyage", Objective: "bounded objective", Commissioned: true, Tasks: []voyage.Task{task}}
	planHash := strings.Repeat("a", 64)
	ledger, err := recovery.OpenAttemptLedger(structuredLedgerPath(planHash, task))
	if err != nil {
		t.Fatal(err)
	}
	attempt := "structured-1"
	if _, err = ledger.Append("reservation", attempt, recovery.AttemptReservation{PlanHash: planHash, GlobalFingerprint: strings.Repeat("b", 64), TaskID: task.ID, TaskFingerprint: strings.Repeat("c", 64), AttemptID: attempt, Model: "luna", Effort: "medium", Tokens: recovery.TokenUsage{Reserved: 1000}}, time.Unix(1, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Append("result", attempt, recovery.ResultRecord{AttemptID: attempt, ResultHash: strings.Repeat("d", 64), Model: "luna", Effort: "medium", Outcome: recovery.OutcomeRetryableFailure, Reason: recovery.ReasonImplementationFailure, TaskFingerprint: strings.Repeat("c", 64), Tokens: recovery.TokenUsage{Reserved: 1000, Used: 400}, EvidenceCount: 1, VerifierStatus: recovery.VerifierFail}, time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.Append("transition", attempt, recovery.TransitionRecord{AttemptID: attempt, Transition: recovery.RecoveryTransition{From: "running", To: "retrying", Action: recovery.ActionAdvanceTier, NextModel: "terra", NextEffort: "high"}}, time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	items := structuredRecoverySummary(planHash, plan)
	joined := strings.Join(items, "\n")
	for _, want := range []string{"current=luna/medium", "budget=1000 reserved,400 used,600 remaining", "outcome=retryable_failure", "reason=implementation_failure", "evidence=dddddddddddd", "fingerprint=cccccccccccc", "transition=luna/medium -> terra/high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("structured summary missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "Prompt") || strings.Contains(joined, ".shipmates") {
		t.Fatalf("structured summary leaked untrusted detail: %s", joined)
	}

	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.StructuredResult(task, recovery.CrewResult{Model: "luna", Effort: "medium", Outcome: recovery.OutcomeRetryableFailure, Reason: recovery.ReasonImplementationFailure, Tokens: recovery.TokenUsage{Reserved: 1000, Used: 400}, Verifier: recovery.VerifierResult{Status: recovery.VerifierFail}, Evidence: []recovery.CrewEvidence{{Code: "bounded", Digest: strings.Repeat("e", 64)}}}, recovery.RecoveryTransition{From: "running", To: "retrying", Action: recovery.ActionAdvanceTier, NextModel: "terra", NextEffort: "high"})
	if got := out.String(); !strings.Contains(got, "TRANSITION") || !strings.Contains(got, "luna/medium -> terra/high") || strings.Contains(got, "raw") {
		t.Fatalf("Sail semantic output=%q", got)
	}
}

// A structured task whose crew turn fails must persist its terminal state and
// its ledger records; the reference branch's advisory stage is deferred and
// its absence must never abort the voyage.
func TestStructuredFailurePersistsWithoutAdvisoryRuntime(t *testing.T) {
	task := structuredTaskFixture()
	task.Persona = "backend"
	plan := voyage.Plan{Version: 1, Title: "structured", Objective: "prove ledger without advisory", Commissioned: true, Tasks: []voyage.Task{task}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	// The structured recovery ladder uses its own models; the project ladder
	// must contain them.
	if err := os.WriteFile(filepath.Join(root, "shipmates.yaml"), []byte("sessionPrefix: test\nmodelLadder: [luna, terra]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	sailTaskDispatcher = func(_ context.Context, _, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		_, _ = fmt.Fprintln(stdout, "not a crew.result.v1 object")
		return nil
	}
	err := runSailCommand(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "voyage incomplete") {
		t.Fatalf("structured failure err = %v", err)
	}
	b, _ := json.Marshal(plan)
	hash := voyage.Hash(b)
	state, err := voyage.LoadState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), &plan, hash)
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Tasks[task.ID]
	if entry.Status != voyage.Failed || !strings.Contains(entry.Error, "result_contract_failure") {
		t.Fatalf("structured task state = %+v", entry)
	}
	ledger, err := recovery.OpenAttemptLedger(structuredLedgerPath(hash, task))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, record := range ledger.Records {
		kinds[record.Kind]++
	}
	if kinds["reservation"] != 1 || kinds["dispatch"] != 1 || kinds["validation"] != 1 {
		t.Fatalf("ledger kinds = %v", kinds)
	}
}
