package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
)

var errIndeterminateStructuredTurn = errors.New("indeterminate_started_turn")

func structuredEnabled(task voyage.Task) bool { return task.Recovery != nil && task.Recovery.Enabled }

func structuredLedgerPath(planHash string, task voyage.Task) string {
	return filepath.Join(project.Dir, "voyages", planHash[:16]+"-"+task.ID+".attempts.jsonl")
}

func prepareStructuredAttempt(planHash string, plan *voyage.Plan, task voyage.Task, entry voyage.TaskState) (*recovery.AttemptLedger, string, error) {
	if !structuredEnabled(task) {
		return nil, "", nil
	}
	path := structuredLedgerPath(planHash, task)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	ledger, err := recovery.OpenAttemptLedger(path)
	if err != nil {
		return nil, "", err
	}
	reservations := 0
	for _, record := range ledger.Records {
		if record.Kind == "reservation" {
			reservations++
		}
	}
	if reservations > 0 {
		last := ledger.Records[0].AttemptID
		for _, record := range ledger.Records {
			if record.Kind == "reservation" {
				last = record.AttemptID
			}
		}
		started, terminal := false, false
		for _, record := range ledger.Records {
			if record.AttemptID == last {
				if record.Kind == "dispatch" {
					started = true
				}
				if record.Kind == "result" || record.Kind == "infrastructure_retry" || record.Kind == "validation" {
					terminal = true
				}
			}
		}
		if started && !terminal {
			return nil, "", errIndeterminateStructuredTurn
		}
	}
	attemptID := fmt.Sprintf("%s-%d", task.ID, reservations+1)
	reserved, err := recovery.ReserveTokens(*task.Recovery, task.Recovery.MaxTokens)
	if err != nil {
		return nil, "", err
	}
	tierCount := len(task.Recovery.Models) * len(task.Recovery.Efforts)
	if entry.Attempt >= tierCount {
		return nil, "", errors.New("structured tier budget exhausted")
	}
	binding := recovery.AttemptReservation{PlanHash: planHash, GlobalFingerprint: voyage.GlobalFingerprint(plan), TaskID: task.ID, TaskFingerprint: entry.TaskFingerprint, AttemptID: attemptID, Model: task.Recovery.Models[entry.Attempt/len(task.Recovery.Efforts)], Effort: task.Recovery.Efforts[entry.Attempt%len(task.Recovery.Efforts)], Tokens: reserved}
	if _, err := ledger.Append("reservation", attemptID, binding, time.Now().UTC()); err != nil {
		return nil, "", err
	}
	if _, err := ledger.Append("dispatch", attemptID, recovery.DispatchRecord{AttemptID: attemptID, Model: binding.Model, Effort: binding.Effort}, time.Now().UTC()); err != nil {
		return nil, "", err
	}
	return ledger, attemptID, nil
}

func mustCanonicalPlanForStructured(plan *voyage.Plan) []byte {
	b, _ := json.Marshal(plan)
	return b
}

func structuredBinding(planHash string, plan *voyage.Plan, task voyage.Task, entry voyage.TaskState, attemptID string) recovery.ResultBinding {
	return recovery.ResultBinding{PlanHash: planHash, GlobalFingerprint: voyage.GlobalFingerprint(plan), TaskID: task.ID, TaskFingerprint: entry.TaskFingerprint, AttemptID: attemptID}
}

func structuredInfrastructureRetries(ledger *recovery.AttemptLedger, attemptID string) uint8 {
	var count uint8
	model, effort := "", ""
	for _, record := range ledger.Records {
		if record.AttemptID == attemptID && record.Kind == "reservation" {
			var reservation recovery.AttemptReservation
			if json.Unmarshal(record.Payload, &reservation) == nil {
				model, effort = reservation.Model, reservation.Effort
			}
		}
	}
	for _, record := range ledger.Records {
		if record.Kind != "infrastructure_retry" {
			continue
		}
		var dispatch recovery.DispatchRecord
		if json.Unmarshal(record.Payload, &dispatch) == nil && dispatch.Model == model && dispatch.Effort == effort {
			count++
		}
	}
	return count
}

func persistStructuredResult(ledger *recovery.AttemptLedger, attemptID string, result recovery.CrewResult, canonical []byte, transition recovery.RecoveryTransition) error {
	if _, err := ledger.Append("result", attemptID, recovery.ResultRecord{
		AttemptID: attemptID, ResultHash: voyage.Hash(canonical), Model: result.Model, Effort: result.Effort,
		Outcome: result.Outcome, Reason: result.Reason, TaskFingerprint: result.TaskFingerprint,
		Tokens: result.Tokens, EvidenceCount: uint8(len(result.Evidence) + len(result.Verifier.Evidence)),
		VerifierStatus: result.Verifier.Status, CorrectiveTemplateID: result.CorrectiveTemplateID,
	}, result.CreatedAt); err != nil {
		return err
	}
	if _, err := ledger.Append("validation", attemptID, recovery.ValidationRecord{AttemptID: attemptID, Accepted: true}, time.Now().UTC()); err != nil {
		return err
	}
	if _, err := ledger.Append("transition", attemptID, recovery.TransitionRecord{AttemptID: attemptID, Transition: transition}, time.Now().UTC()); err != nil {
		return err
	}
	_, err := ledger.Append("action", attemptID, recovery.ActionRecord{AttemptID: attemptID, Action: transition.Action}, time.Now().UTC())
	return err
}

func structuredContractFailure(ledger *recovery.AttemptLedger, attemptID string, err error) error {
	_, appendErr := ledger.Append("validation", attemptID, recovery.ValidationRecord{AttemptID: attemptID, Accepted: false, ErrorCode: "result_contract_failure"}, time.Now().UTC())
	if appendErr != nil {
		return appendErr
	}
	return fmt.Errorf("result_contract_failure: %w", err)
}

func structuredErrorText(err error) string {
	if err == nil {
		return ""
	}
	return boundedSailText(strings.ReplaceAll(err.Error(), "\n", " "), 2048)
}

func adviseStructuredBlocker(ctx context.Context, runtime *sailRecovery, plan *voyage.Plan, planHash string, task voyage.Task, entry voyage.TaskState, reason recovery.ReasonCode, detail string, display sailDisplay) error {
	if runtime == nil {
		return errors.New("structured recovery runtime unavailable")
	}
	advice, advised, err := runtime.advise(ctx, plan, planHash, task, entry, reason, detail)
	if err != nil {
		return err
	}
	if advised {
		display.Recovery(task, entry.Attempt, advice, string(reason), detail, "recommendation recorded; no Sol authority granted")
	}
	return nil
}
