package recovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/voyage"
)

func crewBinding() ResultBinding {
	return ResultBinding{PlanHash: strings.Repeat("a", 64), GlobalFingerprint: strings.Repeat("b", 64), TaskID: "task-one", TaskFingerprint: strings.Repeat("c", 64), AttemptID: "attempt-one"}
}

func crewResult() CrewResult {
	b := crewBinding()
	return CrewResult{Schema: CrewResultSchema, PlanHash: b.PlanHash, GlobalFingerprint: b.GlobalFingerprint, TaskID: b.TaskID, TaskFingerprint: b.TaskFingerprint, AttemptID: b.AttemptID, Model: "gpt-5.6-luna", Effort: "medium", Outcome: OutcomeRetryableFailure, Reason: ReasonImplementationFailure, Summary: "bounded retry", Tokens: TokenUsage{Reserved: 100, Used: 90}, Verifier: VerifierResult{Status: VerifierFail}, CreatedAt: time.Unix(10, 0).UTC()}
}

func TestCrewResultCanonicalAndStrict(t *testing.T) {
	r := crewResult()
	raw, err := jsonMarshal(r)
	if err != nil {
		t.Fatal(err)
	}
	parsed, canonical, err := ParseCrewResult(raw, crewBinding())
	if err != nil || parsed.AttemptID != "attempt-one" || len(canonical) == 0 {
		t.Fatalf("parse=%+v err=%v", parsed, err)
	}
	if _, _, err := ParseCrewResult([]byte(`{"schema":"crew.result.v1","schema":"crew.result.v1"}`), crewBinding()); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if _, _, err := ParseCrewResult(append(raw, []byte(` {}`)...), crewBinding()); err == nil {
		t.Fatal("trailing value accepted")
	}
	r.Outcome = OutcomeNoGo
	r.Reason = ReasonVerifierNoGo
	r.Verifier.Status = VerifierNoGo
	r.Verifier.Role = "designated-verifier"
	r.Verifier.CriterionIDs = []string{"criterion-one"}
	if err := r.Validate(crewBinding()); err != nil {
		t.Fatal(err)
	}
	r.Verifier.Status = VerifierPass
	if err := r.Validate(crewBinding()); err == nil {
		t.Fatal("verifier no-go was weakened")
	}
}

func TestRecoveryBudgetsPreserveLegacyAndAdvanceDeterministically(t *testing.T) {
	legacy := voyage.Task{ID: "task-one", Persona: "crew", Summary: "work", Prompt: "work", RetrySafe: true}
	if err := legacy.Recovery.Validate(); err != nil {
		t.Fatal(err)
	}
	task := legacy
	task.Recovery = &voyage.RecoveryTask{Enabled: true, MaxAttempts: 4, MaxInfrastructureRetries: 1, MaxTokens: 1000, Models: []string{"luna", "terra"}, Efforts: []string{"medium", "high"}, CorrectiveTemplates: []voyage.CorrectiveTemplate{{ID: "fix-one", Summary: "fix", Prompt: "fix", VerificationSummary: "verify", VerificationPrompt: "verify", RetrySafe: true}}}
	r := crewResult()
	next, err := AdvanceRecovery(task, 0, 0, r)
	if err != nil || next.Action != ActionAdvanceTier || next.NextModel != "luna" || next.NextEffort != "high" {
		t.Fatalf("transition=%+v err=%v", next, err)
	}
	next, err = AdvanceRecovery(task, 1, 0, r)
	if err != nil || next.NextModel != "terra" || next.NextEffort != "medium" {
		t.Fatalf("second transition=%+v err=%v", next, err)
	}
	// The closed budget, rather than a saturated model index, terminates the
	// final configured attempt.
	next, err = AdvanceRecovery(task, 3, 0, r)
	if err != nil || next.Action != ActionStop {
		t.Fatalf("budget stop=%+v err=%v", next, err)
	}
	r.Outcome, r.Reason = OutcomeInfrastructureRetry, ReasonInfrastructureUnavailable
	next, err = AdvanceRecovery(task, 0, 0, r)
	if err != nil || next.Action != ActionInfrastructureRetry || next.InfrastructureAttempt != 1 {
		t.Fatalf("infra=%+v err=%v", next, err)
	}
	r.Outcome, r.Reason = OutcomeAuthorityBlocked, ReasonAuthorityRequired
	next, err = AdvanceRecovery(task, 0, 0, r)
	if err != nil || next.Action != ActionAutoCaptain || next.To != "needs_input" {
		t.Fatalf("authority=%+v err=%v", next, err)
	}
}

func TestAttemptLedgerHashReplayAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempts.jsonl")
	l, err := OpenAttemptLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append("reservation", "attempt-one", map[string]any{"tokens": 10}, time.Unix(1, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append("result", "attempt-one", map[string]any{"outcome": "retryable_failure"}, time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenAttemptLedger(path)
	if err != nil || len(reloaded.Records) != 2 || reloaded.Records[1].PreviousHash != reloaded.Records[0].Hash {
		t.Fatalf("reloaded=%+v err=%v", reloaded, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, []byte("corrupt\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAttemptLedger(path); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corruption err=%v", err)
	}
}

// jsonMarshal keeps the fixture explicit without exposing a second parser.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
