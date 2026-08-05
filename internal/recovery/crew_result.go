package recovery

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/voyage"
)

const (
	CrewResultSchema      = "crew.result.v1"
	MaxCrewResultBytes    = 64 << 10
	MaxAttemptLedgerBytes = 8 << 20
	MaxLedgerRecordBytes  = 32 << 10
)

type CrewOutcome string

const (
	OutcomeCompleted           CrewOutcome = "completed"
	OutcomeRetryableFailure    CrewOutcome = "retryable_failure"
	OutcomeInfrastructureRetry CrewOutcome = "infrastructure_retry"
	OutcomeAuthorityBlocked    CrewOutcome = "authority_blocked"
	OutcomeInputRequired       CrewOutcome = "input_required"
	OutcomeNoGo                CrewOutcome = "no_go"
)

type CrewReason string

const (
	ReasonImplementationFailure     CrewReason = "implementation_failure"
	ReasonVerificationFailure       CrewReason = "verification_failure"
	ReasonInfrastructureUnavailable CrewReason = "infrastructure_unavailable"
	ReasonAuthorityRequired         CrewReason = "authority_required"
	ReasonInputRequired             CrewReason = "input_required"
	ReasonBudgetExhausted           CrewReason = "budget_exhausted"
	ReasonVerifierNoGo              CrewReason = "verifier_no_go"
)

type VerifierStatus string

const (
	VerifierPass VerifierStatus = "pass"
	VerifierFail VerifierStatus = "fail"
	VerifierNoGo VerifierStatus = "no_go"
)

type CrewEvidence struct {
	Code   string `json:"code"`
	Digest string `json:"digest"`
}

type TokenUsage struct {
	Reserved uint32 `json:"reserved"`
	Used     uint32 `json:"used"`
}

type VerifierResult struct {
	Status       VerifierStatus `json:"status"`
	Role         string         `json:"role,omitempty"`
	CriterionIDs []string       `json:"criterion_ids,omitempty"`
	Evidence     []CrewEvidence `json:"evidence,omitempty"`
}

type CrewResult struct {
	Schema               string         `json:"schema"`
	PlanHash             string         `json:"plan_hash"`
	GlobalFingerprint    string         `json:"global_fingerprint"`
	TaskID               string         `json:"task_id"`
	TaskFingerprint      string         `json:"task_fingerprint"`
	AttemptID            string         `json:"attempt_id"`
	Model                string         `json:"model"`
	Effort               string         `json:"effort"`
	Outcome              CrewOutcome    `json:"outcome"`
	Reason               CrewReason     `json:"reason"`
	Summary              string         `json:"summary"`
	Evidence             []CrewEvidence `json:"evidence,omitempty"`
	Tokens               TokenUsage     `json:"tokens"`
	Verifier             VerifierResult `json:"verifier"`
	CorrectiveTemplateID string         `json:"corrective_template_id,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
}

type ResultBinding struct{ PlanHash, GlobalFingerprint, TaskID, TaskFingerprint, AttemptID string }

func (r CrewResult) Validate(binding ResultBinding) error {
	if r.Schema != CrewResultSchema || r.PlanHash != binding.PlanHash || r.GlobalFingerprint != binding.GlobalFingerprint || r.TaskID != binding.TaskID || r.TaskFingerprint != binding.TaskFingerprint || r.AttemptID != binding.AttemptID {
		return errors.New("crew result binding mismatch")
	}
	for name, value := range map[string]string{"plan_hash": r.PlanHash, "global_fingerprint": r.GlobalFingerprint, "task_fingerprint": r.TaskFingerprint} {
		if !hashPattern.MatchString(value) {
			return fmt.Errorf("invalid crew result %s", name)
		}
	}
	if !taskPattern.MatchString(r.TaskID) || !taskPattern.MatchString(r.AttemptID) || len(r.Model) == 0 || len(r.Model) > 128 || !validEffort(r.Effort) || len(r.Summary) == 0 || len(r.Summary) > MaxText || strings.ContainsAny(r.Summary, "\r\n") || r.CreatedAt.IsZero() {
		return errors.New("invalid crew result identity or summary")
	}
	if !validOutcomeReason(r.Outcome, r.Reason) {
		return errors.New("invalid crew result outcome/reason")
	}
	if r.Tokens.Reserved == 0 || r.Tokens.Used > r.Tokens.Reserved || r.Tokens.Reserved > 1<<20 {
		return errors.New("invalid crew result token accounting")
	}
	if len(r.Evidence) > 8 || len(r.Verifier.Evidence) > 8 {
		return errors.New("crew result evidence exceeds bound")
	}
	for _, e := range append(append([]CrewEvidence{}, r.Evidence...), r.Verifier.Evidence...) {
		if len(e.Code) == 0 || len(e.Code) > 128 || !hashPattern.MatchString(e.Digest) || strings.ContainsAny(e.Code, "\r\n") {
			return errors.New("invalid crew result evidence")
		}
	}
	if r.Verifier.Status != VerifierPass && r.Verifier.Status != VerifierFail && r.Verifier.Status != VerifierNoGo {
		return errors.New("invalid verifier status")
	}
	if r.Outcome == OutcomeNoGo && r.Verifier.Status != VerifierNoGo {
		return errors.New("no-go result requires verifier no-go")
	}
	if r.Verifier.Status == VerifierNoGo && r.Outcome != OutcomeNoGo {
		return errors.New("verifier no-go is authoritative")
	}
	if len(r.Verifier.CriterionIDs) > 8 || (r.Verifier.Status == VerifierNoGo && (r.Verifier.Role != "designated-verifier" || len(r.Verifier.CriterionIDs) == 0)) {
		return errors.New("invalid designated verifier result")
	}
	for _, id := range r.Verifier.CriterionIDs {
		if !taskPattern.MatchString(id) {
			return errors.New("invalid verifier criterion id")
		}
	}
	if r.CorrectiveTemplateID != "" && !taskPattern.MatchString(r.CorrectiveTemplateID) {
		return errors.New("invalid corrective template id")
	}
	return nil
}

func validEffort(v string) bool {
	switch v {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}
func validOutcomeReason(o CrewOutcome, r CrewReason) bool {
	switch o {
	case OutcomeCompleted:
		return r == ReasonVerificationFailure || r == ReasonImplementationFailure
	case OutcomeRetryableFailure:
		return r == ReasonImplementationFailure || r == ReasonVerificationFailure || r == ReasonBudgetExhausted
	case OutcomeInfrastructureRetry:
		return r == ReasonInfrastructureUnavailable
	case OutcomeAuthorityBlocked:
		return r == ReasonAuthorityRequired
	case OutcomeInputRequired:
		return r == ReasonInputRequired
	case OutcomeNoGo:
		return r == ReasonVerifierNoGo
	default:
		return false
	}
}

func ParseCrewResult(raw []byte, binding ResultBinding) (CrewResult, []byte, error) {
	if len(raw) == 0 || len(raw) > MaxCrewResultBytes || !utf8.Valid(raw) || rejectDuplicateJSON(raw) != nil {
		return CrewResult{}, nil, errors.New("invalid crew result encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var result CrewResult
	if err := dec.Decode(&result); err != nil {
		return CrewResult{}, nil, fmt.Errorf("decode crew result: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return CrewResult{}, nil, errors.New("crew result has trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return CrewResult{}, nil, errors.New("crew result trailing content")
	}
	if err := result.Validate(binding); err != nil {
		return CrewResult{}, nil, err
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return CrewResult{}, nil, err
	}
	return result, canonical, nil
}

type RecoveryAction string

const (
	ActionAdvanceTier         RecoveryAction = "advance_tier"
	ActionInfrastructureRetry RecoveryAction = "infrastructure_retry"
	ActionAutoCaptain         RecoveryAction = "auto_captain"
	ActionStop                RecoveryAction = "stop"
)

type RecoveryTransition struct {
	From                  string
	To                    string
	Action                RecoveryAction
	NextModel             string
	NextEffort            string
	InfrastructureAttempt uint8
}

func AdvanceRecovery(task voyage.Task, attempts uint8, infrastructure uint8, result CrewResult) (RecoveryTransition, error) {
	if task.Recovery == nil || !task.Recovery.Enabled {
		return RecoveryTransition{}, errors.New("structured recovery is not opted in")
	}
	if err := task.Recovery.Validate(); err != nil {
		return RecoveryTransition{}, err
	}
	if result.Outcome == OutcomeInfrastructureRetry {
		if infrastructure >= task.Recovery.MaxInfrastructureRetries {
			return RecoveryTransition{From: "running", To: "stopped", Action: ActionStop}, nil
		}
		return RecoveryTransition{From: "running", To: "retrying_infrastructure", Action: ActionInfrastructureRetry, InfrastructureAttempt: infrastructure + 1}, nil
	}
	if result.Outcome == OutcomeAuthorityBlocked || result.Outcome == OutcomeInputRequired {
		return RecoveryTransition{From: "running", To: "needs_input", Action: ActionAutoCaptain}, nil
	}
	if result.Outcome == OutcomeNoGo {
		return RecoveryTransition{From: "running", To: "no_go", Action: ActionStop}, nil
	}
	if result.Outcome == OutcomeCompleted {
		return RecoveryTransition{From: "running", To: "completed", Action: ActionStop}, nil
	}
	// attempts is the zero-based slot just dispatched.  A retry advances to
	// the next Cartesian model/effort slot; it never repeats the current slot
	// or skips the effort-first ordering used by Sail dispatch.
	if attempts+1 >= task.Recovery.MaxAttempts {
		return RecoveryTransition{From: "running", To: "stopped", Action: ActionStop}, nil
	}
	next := int(attempts) + 1
	return RecoveryTransition{From: "running", To: "retrying", Action: ActionAdvanceTier, NextModel: task.Recovery.Models[next/len(task.Recovery.Efforts)], NextEffort: task.Recovery.Efforts[next%len(task.Recovery.Efforts)]}, nil
}

type LedgerRecord struct {
	Schema       string          `json:"schema"`
	Sequence     uint64          `json:"sequence"`
	PreviousHash string          `json:"previous_hash"`
	Kind         string          `json:"kind"`
	AttemptID    string          `json:"attempt_id"`
	Payload      json.RawMessage `json:"payload"`
	Hash         string          `json:"hash"`
	CreatedAt    time.Time       `json:"created_at"`
}

type AttemptLedger struct {
	Path     string
	Records  []LedgerRecord
	lastHash string
}

type AttemptReservation struct {
	PlanHash          string     `json:"plan_hash"`
	GlobalFingerprint string     `json:"global_fingerprint"`
	TaskID            string     `json:"task_id"`
	TaskFingerprint   string     `json:"task_fingerprint"`
	AttemptID         string     `json:"attempt_id"`
	Model             string     `json:"model"`
	Effort            string     `json:"effort"`
	Tokens            TokenUsage `json:"tokens"`
}

type DispatchRecord struct {
	AttemptID string `json:"attempt_id"`
	Model     string `json:"model"`
	Effort    string `json:"effort"`
}
type ResultRecord struct {
	AttemptID            string         `json:"attempt_id"`
	ResultHash           string         `json:"result_hash"`
	Model                string         `json:"model"`
	Effort               string         `json:"effort"`
	Outcome              CrewOutcome    `json:"outcome"`
	Reason               CrewReason     `json:"reason"`
	TaskFingerprint      string         `json:"task_fingerprint"`
	Tokens               TokenUsage     `json:"tokens"`
	EvidenceCount        uint8          `json:"evidence_count"`
	VerifierStatus       VerifierStatus `json:"verifier_status"`
	CorrectiveTemplateID string         `json:"corrective_template_id,omitempty"`
}
type ValidationRecord struct {
	AttemptID string `json:"attempt_id"`
	Accepted  bool   `json:"accepted"`
	ErrorCode string `json:"error_code,omitempty"`
}
type TransitionRecord struct {
	AttemptID  string             `json:"attempt_id"`
	Transition RecoveryTransition `json:"transition"`
}
type ActionRecord struct {
	AttemptID string         `json:"attempt_id"`
	Action    RecoveryAction `json:"action"`
}

func ReserveTokens(budget voyage.RecoveryTask, requested uint32) (TokenUsage, error) {
	if budget.Enabled == false || requested == 0 || requested > budget.MaxTokens {
		return TokenUsage{}, errors.New("token reservation exceeds recovery budget")
	}
	return TokenUsage{Reserved: requested}, nil
}

func ReconcileTokens(reserved TokenUsage, used uint32) (TokenUsage, error) {
	if reserved.Reserved == 0 || used > reserved.Reserved {
		return TokenUsage{}, errors.New("token reconciliation exceeds reservation")
	}
	return TokenUsage{Reserved: reserved.Reserved, Used: used}, nil
}

func OpenAttemptLedger(path string) (*AttemptLedger, error) {
	l := &AttemptLedger{Path: path}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() > MaxAttemptLedgerBytes {
		return nil, errors.New("attempt ledger exceeds bound")
	}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), MaxLedgerRecordBytes)
	var previous string
	var seq uint64
	for s.Scan() {
		var r LedgerRecord
		raw := s.Bytes()
		if json.Unmarshal(raw, &r) != nil || r.Schema != "crew.attempt.v1" || r.Sequence != seq+1 || r.PreviousHash != previous || r.Kind == "" || len(r.Payload) > MaxLedgerRecordBytes || r.CreatedAt.IsZero() {
			return nil, errors.New("corrupt attempt ledger")
		}
		expected := ledgerHash(r)
		if r.Hash != expected {
			return nil, errors.New("attempt ledger hash mismatch")
		}
		l.Records = append(l.Records, r)
		previous, seq = r.Hash, r.Sequence
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	l.lastHash = previous
	return l, nil
}

func ledgerHash(r LedgerRecord) string {
	copy := r
	copy.Hash = ""
	b, _ := json.Marshal(copy)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (l *AttemptLedger) Append(kind, attemptID string, payload any, now time.Time) (LedgerRecord, error) {
	if l == nil || kind == "" || !taskPattern.MatchString(attemptID) || len(l.Records) >= 128 {
		return LedgerRecord{}, errors.New("invalid attempt ledger append")
	}
	b, err := json.Marshal(payload)
	if err != nil || len(b) > MaxLedgerRecordBytes {
		return LedgerRecord{}, errors.New("invalid attempt ledger payload")
	}
	r := LedgerRecord{Schema: "crew.attempt.v1", Sequence: uint64(len(l.Records) + 1), PreviousHash: l.lastHash, Kind: kind, AttemptID: attemptID, Payload: b, CreatedAt: now.UTC()}
	r.Hash = ledgerHash(r)
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return LedgerRecord{}, err
	}
	defer f.Close()
	enc, _ := json.Marshal(r)
	if _, err = f.Write(append(enc, '\n')); err != nil {
		return LedgerRecord{}, err
	}
	if err = f.Sync(); err != nil {
		return LedgerRecord{}, err
	}
	l.Records = append(l.Records, r)
	l.lastHash = r.Hash
	return r, nil
}
