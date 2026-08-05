package recovery

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/voyage"
)

func correctiveFixture() (*voyage.Plan, voyage.Task, CrewResult) {
	task := voyage.Task{ID: "task-one", Persona: "crew", Summary: "original", Prompt: "original", RetrySafe: true, Recovery: &voyage.RecoveryTask{Enabled: true, MaxAttempts: 2, MaxInfrastructureRetries: 2, MaxTokens: 1000, Models: []string{"luna", "terra"}, Efforts: []string{"medium", "high"}, ApprovedCriterionIDs: []string{"criterion-one"}, CorrectiveTemplates: []voyage.CorrectiveTemplate{{ID: "template-one", Summary: "correct", Prompt: "correct", VerificationSummary: "verify", VerificationPrompt: "verify", CriterionIDs: []string{"criterion-one"}, RetrySafe: true}}}}
	plan := &voyage.Plan{Version: 1, Title: "p", Objective: "o", Approved: true, Tasks: []voyage.Task{task}}
	canonical, _ := json.Marshal(plan)
	result := CrewResult{Schema: CrewResultSchema, PlanHash: voyage.Hash(canonical), GlobalFingerprint: voyage.GlobalFingerprint(plan), TaskID: task.ID, TaskFingerprint: voyage.TaskFingerprint(task), AttemptID: "task-one-1", Model: "luna", Effort: "medium", Outcome: OutcomeNoGo, Reason: ReasonVerifierNoGo, Summary: "verification failed", Tokens: TokenUsage{Reserved: 100, Used: 100}, Verifier: VerifierResult{Status: VerifierNoGo, Role: "designated-verifier", CriterionIDs: []string{"criterion-one"}, Evidence: []CrewEvidence{{Code: "check", Digest: strings.Repeat("d", 64)}}}, CorrectiveTemplateID: "template-one", CreatedAt: time.Now().UTC()}
	return plan, task, result
}

func TestCorrectiveSuccessorIsFrozenAndIdempotent(t *testing.T) {
	plan, task, result := correctiveFixture()
	first, err := InstantiateCorrective(plan, task, result, []byte(`{"result":"immutable"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := InstantiateCorrective(plan, task, result, []byte(`{"result":"immutable"}`))
	if err != nil || first.Task.ID != second.Task.ID || first.VerificationTask.ID != second.VerificationTask.ID || first.TaskBeadID == first.VerificationBeadID {
		t.Fatalf("first=%+v second=%+v err=%v", first, second, err)
	}
	if first.Task.Persona != task.Persona || len(first.Task.DependsOn) != 1 || first.Task.DependsOn[0] != task.ID || first.VerificationTask.DependsOn[0] != first.Task.ID {
		t.Fatalf("scope widened: %+v", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectiveValidationRejectsSubstitutionAndAbsentTemplate(t *testing.T) {
	plan, task, result := correctiveFixture()
	result.Verifier.CriterionIDs = []string{"other-criterion"}
	if _, err := InstantiateCorrective(plan, task, result, []byte("evidence")); err == nil {
		t.Fatal("criterion substitution accepted")
	}
	_, _, result = correctiveFixture()
	result.CorrectiveTemplateID = "missing"
	if _, err := InstantiateCorrective(plan, task, result, []byte("evidence")); !errors.Is(err, ErrNoAuthorizedTemplate) {
		t.Fatalf("missing template err=%v", err)
	}
	_, _, result = correctiveFixture()
	result.Verifier.Role = "crew"
	if _, err := InstantiateCorrective(plan, task, result, []byte("evidence")); err == nil {
		t.Fatal("invalid verifier role accepted")
	}
}
