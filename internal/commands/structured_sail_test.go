package commands

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
)

func structuredTaskFixture() voyage.Task {
	return voyage.Task{ID: "structured", Persona: "crew", Summary: "structured", Prompt: "structured", RetrySafe: true, Recovery: &voyage.RecoveryTask{Enabled: true, MaxAttempts: 4, MaxInfrastructureRetries: 2, MaxTokens: 1000, Models: []string{"luna", "terra"}, Efforts: []string{"medium", "high"}, CorrectiveTemplates: []voyage.CorrectiveTemplate{{ID: "fix", Summary: "fix", Prompt: "fix", VerificationSummary: "verify", VerificationPrompt: "verify", RetrySafe: true}}}}
}

func TestStructuredAttemptReservationRefusesIndeterminateReplay(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if err := os.MkdirAll(filepath.Join(".shipmates", "voyages"), 0700); err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{Version: 1, Title: "p", Objective: "o", Approved: true, Tasks: []voyage.Task{structuredTaskFixture()}}
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
	if err := os.MkdirAll(filepath.Join(".shipmates", "voyages"), 0700); err != nil {
		t.Fatal(err)
	}
	task := structuredTaskFixture()
	plan := &voyage.Plan{Version: 1, Title: "semantic voyage", Objective: "bounded objective", Approved: true, Tasks: []voyage.Task{task}}
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
	items := structuredRecoverySidebar(planHash, plan)
	joined := strings.Join(items, "\n")
	for _, want := range []string{"current=luna/medium", "budget=1000 reserved,400 used,600 remaining", "outcome=retryable_failure", "reason=implementation_failure", "evidence=dddddddddddd", "fingerprint=cccccccccccc", "transition=luna/medium -> terra/high"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("structured sidebar missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "Prompt") || strings.Contains(joined, ".shipmates") {
		t.Fatalf("structured sidebar leaked untrusted detail: %s", joined)
	}

	model := &dashboard.Model{Persona: "captain", Connected: true, Sidebar: &dashboard.Sidebar{Title: plan.Title, Status: "SAILING", Sections: []dashboard.SidebarSection{{Heading: "Structured Sail recovery", Items: items}}}}
	wide := strings.Join(dashboard.Render(model, dashboard.Size{Width: 120, Height: 20}).Lines, "\n")
	narrow := strings.Join(dashboard.Render(model, dashboard.Size{Width: 60, Height: 8}).Lines, "\n")
	if !strings.Contains(wide, "terra/high") || strings.Contains(narrow, "terra/high") && len(narrow) > 60*8 {
		t.Fatalf("dashboard projection is not bounded: wide=%q narrow=%q", wide, narrow)
	}

	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.StructuredResult(task, recovery.CrewResult{Model: "luna", Effort: "medium", Outcome: recovery.OutcomeRetryableFailure, Reason: recovery.ReasonImplementationFailure, Tokens: recovery.TokenUsage{Reserved: 1000, Used: 400}, Verifier: recovery.VerifierResult{Status: recovery.VerifierFail}, Evidence: []recovery.CrewEvidence{{Code: "bounded", Digest: strings.Repeat("e", 64)}}}, recovery.RecoveryTransition{From: "running", To: "retrying", Action: recovery.ActionAdvanceTier, NextModel: "terra", NextEffort: "high"})
	if got := out.String(); !strings.Contains(got, "TRANSITION") || !strings.Contains(got, "luna/medium -> terra/high") || strings.Contains(got, "raw") {
		t.Fatalf("Sail semantic output=%q", got)
	}
}
