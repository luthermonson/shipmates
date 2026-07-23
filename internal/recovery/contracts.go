// Package recovery defines the bounded, append-only contracts used by Sail's
// auto-captain recovery advisor. It deliberately does not dispatch Sol from
// or mutate voyage state, plans, evidence, or Beads.
package recovery

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion   = 1
	MaxRequestBytes = 32 << 10
	MaxRecordBytes  = 32 << 10
	MaxJournalBytes = 4 << 20
	MaxEvidence     = 16
	MaxText         = 2048
)

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var taskPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type ReasonCode string

const (
	ReasonOrdinaryFailure            ReasonCode = "ordinary_failure"
	ReasonUncertainty                ReasonCode = "uncertainty"
	ReasonStaleContinuity            ReasonCode = "stale_continuity"
	ReasonContradictoryEvidence      ReasonCode = "contradictory_evidence"
	ReasonExhaustedTiers             ReasonCode = "exhausted_tiers"
	ReasonManagedEnvironmentMismatch ReasonCode = "managed_environment_mismatch"
	ReasonPhysicalAccessUnavailable  ReasonCode = "physical_access_unavailable"
	ReasonHumanCredentialRequired    ReasonCode = "human_credential_required"
	ReasonCaptainDecisionRequired    ReasonCode = "captain_decision_required"
)

func (r ReasonCode) valid() bool {
	switch r {
	case ReasonOrdinaryFailure, ReasonUncertainty, ReasonStaleContinuity, ReasonContradictoryEvidence, ReasonExhaustedTiers, ReasonManagedEnvironmentMismatch, ReasonPhysicalAccessUnavailable, ReasonHumanCredentialRequired, ReasonCaptainDecisionRequired:
		return true
	default:
		return false
	}
}

type Decision string

const (
	DecisionResume            Decision = "resume"
	DecisionContinue          Decision = "continue"
	DecisionAmendmentRequired Decision = "amendment_required"
	DecisionStop              Decision = "stop"
)

func (d Decision) valid() bool {
	return d == DecisionResume || d == DecisionContinue || d == DecisionAmendmentRequired || d == DecisionStop
}

// Config is optional project configuration. Zero means disabled.
type Config struct {
	AutoCaptain          bool                      `yaml:"autoCaptain" json:"auto_captain,omitempty"`
	ContinuationEnvelope *ContinuationEnvelope     `yaml:"continuationEnvelope,omitempty" json:"continuation_envelope,omitempty"`
	DerivativeEnvelope   *ChangeEnvelope           `yaml:"derivativeEnvelope,omitempty" json:"derivative_envelope,omitempty"`
	CommanderDelegation  CommanderDelegationConfig `yaml:"commanderDelegation" json:"commander_delegation,omitempty"`
}

type ContinuationEnvelope struct {
	Enabled        bool         `yaml:"enabled" json:"enabled"`
	MaxAttempts    uint8        `yaml:"maxAttempts" json:"max_attempts"`
	MaxAssessments uint8        `yaml:"maxAssessments" json:"max_assessments"`
	ExpiresAt      time.Time    `yaml:"expiresAt" json:"expires_at"`
	AllowedReasons []ReasonCode `yaml:"allowedReasons,omitempty" json:"allowed_reasons,omitempty"`
}

func (c Config) Validate() error {
	if err := c.CommanderDelegation.Validate(); err != nil {
		return err
	}
	if c.DerivativeEnvelope != nil {
		if err := c.DerivativeEnvelope.Validate(time.Now().UTC()); err != nil {
			return err
		}
	}
	if c.ContinuationEnvelope == nil {
		return nil
	}
	e := c.ContinuationEnvelope
	if !e.Enabled {
		return nil
	}
	if e.MaxAttempts == 0 || e.MaxAttempts > 8 || e.MaxAssessments == 0 || e.MaxAssessments > 8 || e.ExpiresAt.IsZero() || !e.ExpiresAt.After(time.Now().UTC()) {
		return errors.New("invalid continuation envelope bounds")
	}
	if len(e.AllowedReasons) > 8 {
		return errors.New("continuation envelope has too many reason codes")
	}
	for _, reason := range e.AllowedReasons {
		if !reason.valid() {
			return errors.New("continuation envelope has an invalid reason code")
		}
	}
	return nil
}

type Provenance struct {
	VoyagePlanHash   string `json:"voyage_plan_hash"`
	TaskID           string `json:"task_id"`
	TaskContractHash string `json:"task_contract_hash"`
	StateHash        string `json:"state_hash"`
}

func (p Provenance) validate() error {
	if !hashPattern.MatchString(p.VoyagePlanHash) || !hashPattern.MatchString(p.TaskContractHash) || !hashPattern.MatchString(p.StateHash) || !taskPattern.MatchString(p.TaskID) {
		return errors.New("invalid recovery provenance")
	}
	return nil
}

// EvidenceRef is intentionally a redacted reference, never raw output,
// prompt, path, credential, private ID, or backend payload.
type EvidenceRef struct {
	Source     string `json:"source"`
	DetailCode string `json:"detail_code"`
	Digest     string `json:"digest"`
}

func (e EvidenceRef) validate() error {
	if len(e.Source) == 0 || len(e.Source) > 64 || len(e.DetailCode) == 0 || len(e.DetailCode) > 128 || !hashPattern.MatchString(e.Digest) || strings.ContainsAny(e.Source+e.DetailCode, "\r\n") {
		return errors.New("invalid redacted evidence reference")
	}
	return nil
}

type ProofKind string

const (
	ProofPhysicalAccess  ProofKind = "physical_access_unavailable"
	ProofHumanCredential ProofKind = "human_credential_required"
	ProofCaptainDecision ProofKind = "captain_decision_required"
)

type HumanOnlyProof struct {
	Kind       ProofKind     `json:"kind"`
	Verified   bool          `json:"verified"`
	ObservedAt time.Time     `json:"observed_at"`
	Evidence   []EvidenceRef `json:"evidence"`
}

func (p HumanOnlyProof) validate() error {
	if p.Kind != ProofPhysicalAccess && p.Kind != ProofHumanCredential && p.Kind != ProofCaptainDecision {
		return errors.New("invalid human-only proof kind")
	}
	if !p.Verified || p.ObservedAt.IsZero() || len(p.Evidence) == 0 || len(p.Evidence) > MaxEvidence {
		return errors.New("human-only proof is not evidenced")
	}
	for _, e := range p.Evidence {
		if err := e.validate(); err != nil {
			return err
		}
	}
	return nil
}

type RequestV1 struct {
	SchemaVersion uint64          `json:"schema_version"`
	Provenance    Provenance      `json:"provenance"`
	Reason        ReasonCode      `json:"reason"`
	Attempt       uint8           `json:"attempt"`
	TierCount     uint8           `json:"tier_count"`
	Evidence      []EvidenceRef   `json:"evidence,omitempty"`
	HumanOnly     *HumanOnlyProof `json:"human_only,omitempty"`
}

func (r RequestV1) Validate() error {
	if r.SchemaVersion != SchemaVersion || r.TierCount == 0 || r.Attempt >= r.TierCount || !r.Reason.valid() {
		return errors.New("invalid recovery request")
	}
	if err := r.Provenance.validate(); err != nil {
		return err
	}
	if len(r.Evidence) > MaxEvidence {
		return errors.New("recovery request has too much evidence")
	}
	for _, e := range r.Evidence {
		if err := e.validate(); err != nil {
			return err
		}
	}
	if r.HumanOnly != nil {
		if err := r.HumanOnly.validate(); err != nil {
			return err
		}
		if proofReason(r.HumanOnly.Kind) != r.Reason {
			return errors.New("human-only proof does not match recovery reason")
		}
	}
	return nil
}

type ResponseV1 struct {
	SchemaVersion  uint64              `json:"schema_version"`
	Decision       Decision            `json:"decision"`
	Reason         ReasonCode          `json:"reason"`
	Fingerprint    string              `json:"blocker_fingerprint"`
	ActionHint     string              `json:"action_hint,omitempty"`
	PersonaDigest  string              `json:"persona_digest,omitempty"`
	PersonaVersion uint64              `json:"persona_version,omitempty"`
	EffectiveModel string              `json:"effective_model,omitempty"`
	Derivative     *DerivativeProposal `json:"derivative,omitempty"`
}

func (r ResponseV1) EffectiveModelOrDefault() string {
	if r.EffectiveModel == "" {
		return "gpt-5.6-sol"
	}
	return r.EffectiveModel
}

func (r ResponseV1) Validate() error {
	if r.SchemaVersion != SchemaVersion || !r.Decision.valid() || !r.Reason.valid() || !hashPattern.MatchString(r.Fingerprint) || len(r.ActionHint) > 512 || strings.ContainsAny(r.ActionHint, "\r\n") || forbiddenActionHint(r.ActionHint) {
		return errors.New("invalid recovery response")
	}
	if r.PersonaDigest != "" && (!hashPattern.MatchString(r.PersonaDigest) || r.PersonaVersion == 0 || r.PersonaVersion > 8 || r.EffectiveModel != "gpt-5.6-sol") {
		return errors.New("invalid recovery persona provenance")
	}
	if r.Derivative != nil && r.Derivative.SchemaVersion != SchemaVersion {
		return errors.New("invalid derivative advisory")
	}
	return nil
}

// ValidateFor prevents an advisory response from promoting an agent-actionable
// blocker into a human-only stop or amendment. Sail remains authoritative.
func (r ResponseV1) ValidateFor(req RequestV1) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Reason != req.Reason {
		return errors.New("recovery response changed blocker reason")
	}
	c, err := Classify(req)
	if err != nil {
		return err
	}
	if !c.HumanOnly && (r.Decision == DecisionStop || r.Decision == DecisionAmendmentRequired) {
		return errors.New("agent-actionable blocker cannot become human-only")
	}
	if c.HumanOnly && c.Reason == ReasonCaptainDecisionRequired && r.Decision != DecisionAmendmentRequired {
		return errors.New("captain decision proof requires amendment decision")
	}
	if c.HumanOnly && c.Reason != ReasonCaptainDecisionRequired && r.Decision != DecisionStop {
		return errors.New("human-only proof requires stop decision")
	}
	return nil
}

func forbiddenActionHint(s string) bool {
	s = strings.ToLower(s)
	for _, word := range []string{"approve", "complete", "close", "reopen", "rewrite", "reassign", "revoke", "mark done"} {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}

type Classification struct {
	Reason    ReasonCode
	HumanOnly bool
}

func Classify(r RequestV1) (Classification, error) {
	if err := r.Validate(); err != nil {
		return Classification{}, err
	}
	if r.HumanOnly == nil {
		return Classification{Reason: r.Reason}, nil
	}
	if err := r.HumanOnly.validate(); err != nil {
		return Classification{}, err
	}
	var reason ReasonCode
	switch r.HumanOnly.Kind {
	case ProofPhysicalAccess:
		reason = ReasonPhysicalAccessUnavailable
	case ProofHumanCredential:
		reason = ReasonHumanCredentialRequired
	case ProofCaptainDecision:
		reason = ReasonCaptainDecisionRequired
	}
	return Classification{Reason: reason, HumanOnly: true}, nil
}

func Fingerprint(r RequestV1) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	// Only stable redacted facts enter the fingerprint. Raw evidence and human
	// text are not accepted by this schema and therefore cannot leak here.
	redacted := struct {
		Provenance Provenance
		Reason     ReasonCode
		Attempt    uint8
		TierCount  uint8
		Evidence   []EvidenceRef
		HumanOnly  *HumanOnlyProof
	}{r.Provenance, r.Reason, r.Attempt, r.TierCount, r.Evidence, r.HumanOnly}
	b, _ := json.Marshal(redacted)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func proofKind(p *HumanOnlyProof) ProofKind {
	if p == nil {
		return ""
	}
	return p.Kind
}

func proofReason(kind ProofKind) ReasonCode {
	switch kind {
	case ProofPhysicalAccess:
		return ReasonPhysicalAccessUnavailable
	case ProofHumanCredential:
		return ReasonHumanCredentialRequired
	case ProofCaptainDecision:
		return ReasonCaptainDecisionRequired
	default:
		return ""
	}
}

// Assess produces advisory output only. It never authorizes execution or
// changes approved plan/state data.
func Assess(r RequestV1, previousFingerprint string) (ResponseV1, bool, error) {
	classification, err := Classify(r)
	if err != nil {
		return ResponseV1{}, false, err
	}
	fp, err := Fingerprint(r)
	if err != nil {
		return ResponseV1{}, false, err
	}
	materiallyChanged := previousFingerprint == "" || previousFingerprint != fp
	decision := DecisionContinue
	hint := "retry within the approved bounded envelope"
	if classification.HumanOnly {
		if classification.Reason == ReasonCaptainDecisionRequired {
			decision, hint = DecisionAmendmentRequired, "Captain decision required; preserve predecessor state"
		} else {
			decision, hint = DecisionStop, "human-only external action is required"
		}
	}
	return ResponseV1{SchemaVersion: SchemaVersion, Decision: decision, Reason: classification.Reason, Fingerprint: fp, ActionHint: hint}, materiallyChanged, nil
}

type RecordKind string

const (
	RecordBlocker         RecordKind = "blocker"
	RecordAssessment      RecordKind = "assessment"
	RecordAttestation     RecordKind = "attestation"
	RecordAcceptedAction  RecordKind = "accepted_action"
	RecordAdvisoryOutcome RecordKind = "advisory_outcome"
)

const (
	AdvisoryAvailable       = "available"
	AdvisoryAuthUnavailable = "auth_unavailable"
	AdvisoryTimeout         = "timeout"
	AdvisoryInvalidResponse = "invalid_response"
	AdvisoryCleanupFailed   = "cleanup_failed"
)

type RecordHeader struct {
	SchemaVersion uint64     `json:"schema_version"`
	Kind          RecordKind `json:"kind"`
	RecordID      string     `json:"record_id"`
	Sequence      uint64     `json:"sequence"`
	CreatedAt     time.Time  `json:"created_at"`
	Provenance    Provenance `json:"provenance"`
	Fingerprint   string     `json:"blocker_fingerprint"`
}

type BlockerRecord struct {
	RecordHeader
	Reason   ReasonCode      `json:"reason"`
	Evidence []EvidenceRef   `json:"evidence"`
	Proof    *HumanOnlyProof `json:"human_only,omitempty"`
}

type AssessmentRecord struct {
	RecordHeader
	Reason                ReasonCode                  `json:"reason"`
	Decision              Decision                    `json:"decision"`
	MateriallyChanged     bool                        `json:"materially_changed"`
	PersonaDigest         string                      `json:"persona_digest,omitempty"`
	PersonaVersion        uint64                      `json:"persona_version,omitempty"`
	EffectiveModel        string                      `json:"effective_model,omitempty"`
	DerivativePlan        []byte                      `json:"derivative_plan,omitempty"`
	DerivativeAttestation *MachineApprovalAttestation `json:"derivative_attestation,omitempty"`
}

type AttestationRecord struct {
	RecordHeader
	Attester string        `json:"attester"`
	Evidence []EvidenceRef `json:"evidence"`
}

type AcceptedActionRecord struct {
	RecordHeader
	Action              string `json:"action"`
	CaptainApprovalHash string `json:"captain_approval_hash"`
}

// AdvisoryOutcomeRecord is the redacted terminal admission/transport result
// for one blocker fingerprint. It contains no prompt, response, path,
// credential, process, or provider detail.
type AdvisoryOutcomeRecord struct {
	RecordHeader
	Code                 string `json:"code"`
	RequestDigest        string `json:"request_digest"`
	ResponseSchemaDigest string `json:"response_schema_digest"`
	DurationMillis       uint64 `json:"duration_ms"`
	Dispatched           bool   `json:"dispatched"`
}

func validateHeader(h RecordHeader, kind RecordKind) error {
	if h.SchemaVersion != SchemaVersion || h.Kind != kind || h.RecordID == "" || len(h.RecordID) > 128 || strings.ContainsAny(h.RecordID, "\r\n") || h.CreatedAt.IsZero() || !hashPattern.MatchString(h.Fingerprint) {
		return errors.New("invalid recovery record header")
	}
	return h.Provenance.validate()
}

func validateEvidence(es []EvidenceRef) error {
	if len(es) == 0 || len(es) > MaxEvidence {
		return errors.New("invalid recovery record evidence")
	}
	for _, e := range es {
		if err := e.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateRecord(v any) error {
	switch r := v.(type) {
	case BlockerRecord:
		if err := validateHeader(r.RecordHeader, RecordBlocker); err != nil {
			return err
		}
		if !r.Reason.valid() || validateEvidence(r.Evidence) != nil {
			return errors.New("invalid blocker record")
		}
		if r.Proof != nil {
			return r.Proof.validate()
		}
	case AssessmentRecord:
		if err := validateHeader(r.RecordHeader, RecordAssessment); err != nil {
			return err
		}
		if !r.Reason.valid() || !r.Decision.valid() {
			return errors.New("invalid assessment record")
		}
		if r.PersonaDigest != "" && (!hashPattern.MatchString(r.PersonaDigest) || r.PersonaVersion == 0 || r.PersonaVersion > 8 || r.EffectiveModel != "gpt-5.6-sol") {
			return errors.New("invalid assessment persona provenance")
		}
		if len(r.DerivativePlan) > 16<<10 {
			return errors.New("derivative plan exceeds bound")
		}
		if len(r.DerivativePlan) > 0 || r.DerivativeAttestation != nil {
			if r.DerivativeAttestation == nil || ValidateDerivativeArtifact(r.DerivativePlan, *r.DerivativeAttestation) != nil {
				return errors.New("invalid derivative assessment artifact")
			}
		}
	case AttestationRecord:
		if err := validateHeader(r.RecordHeader, RecordAttestation); err != nil {
			return err
		}
		if len(r.Attester) == 0 || len(r.Attester) > 128 || strings.ContainsAny(r.Attester, "\r\n") || validateEvidence(r.Evidence) != nil {
			return errors.New("invalid attestation record")
		}
	case AcceptedActionRecord:
		if err := validateHeader(r.RecordHeader, RecordAcceptedAction); err != nil {
			return err
		}
		if len(r.Action) == 0 || len(r.Action) > 512 || strings.ContainsAny(r.Action, "\r\n") || !hashPattern.MatchString(r.CaptainApprovalHash) {
			return errors.New("invalid accepted action record")
		}
	case AdvisoryOutcomeRecord:
		if err := validateHeader(r.RecordHeader, RecordAdvisoryOutcome); err != nil || !validAdvisoryCode(r.Code) || !hashPattern.MatchString(r.RequestDigest) || !hashPattern.MatchString(r.ResponseSchemaDigest) || r.DurationMillis > 24*60*60*1000 {
			return errors.New("invalid advisory outcome")
		}
	default:
		return errors.New("unsupported recovery record")
	}
	return nil
}

// Journal is a bounded append-only JSONL store. It refuses symlinks, malformed
// existing records, duplicate IDs, oversized records, and replacement writes.
type Journal struct {
	mu          sync.Mutex
	path        string
	seen        map[string]struct{}
	kinds       map[RecordKind]uint64
	assessments map[string]AssessmentRecord
	next        uint64
}

func OpenJournal(path string) (*Journal, error) {
	if err := ensureJournalDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > MaxJournalBytes) {
		return nil, errors.New("recovery journal must be a bounded regular file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	j := &Journal{path: path, seen: map[string]struct{}{}, kinds: map[RecordKind]uint64{}, assessments: map[string]AssessmentRecord{}}
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, MaxJournalBytes+1))
	s.Buffer(make([]byte, 1024), MaxRecordBytes+1)
	for s.Scan() {
		if len(s.Bytes()) > MaxRecordBytes {
			return nil, errors.New("recovery journal record exceeds bound")
		}
		h, err := decodeStoredRecord(s.Bytes())
		if err != nil || h.RecordID == "" || h.Sequence == 0 {
			return nil, errors.New("malformed recovery journal record")
		}
		if _, ok := j.seen[h.RecordID]; ok {
			return nil, errors.New("duplicate recovery record")
		}
		j.seen[h.RecordID] = struct{}{}
		j.kinds[h.Kind]++
		if h.Kind == RecordAssessment {
			dec := json.NewDecoder(bytes.NewReader(s.Bytes()))
			dec.DisallowUnknownFields()
			var assessment AssessmentRecord
			if err := dec.Decode(&assessment); err != nil {
				return nil, errors.New("malformed recovery assessment")
			}
			j.assessments[h.RecordID] = assessment
		}
		if h.Sequence > j.next {
			j.next = h.Sequence
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return j, nil
}

func decodeStoredRecord(raw []byte) (RecordHeader, error) {
	var kind struct {
		Kind RecordKind `json:"kind"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&kind); err != nil {
		return RecordHeader{}, err
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var record any
	switch kind.Kind {
	case RecordBlocker:
		record = &BlockerRecord{}
	case RecordAssessment:
		record = &AssessmentRecord{}
	case RecordAttestation:
		record = &AttestationRecord{}
	case RecordAcceptedAction:
		record = &AcceptedActionRecord{}
	case RecordAdvisoryOutcome:
		record = &AdvisoryOutcomeRecord{}
	default:
		return RecordHeader{}, errors.New("unknown recovery record kind")
	}
	if err := dec.Decode(record); err != nil {
		return RecordHeader{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RecordHeader{}, errors.New("recovery journal record has trailing data")
	}
	var header RecordHeader
	switch r := record.(type) {
	case *BlockerRecord:
		header = r.RecordHeader
	case *AssessmentRecord:
		header = r.RecordHeader
	case *AttestationRecord:
		header = r.RecordHeader
	case *AcceptedActionRecord:
		header = r.RecordHeader
	case *AdvisoryOutcomeRecord:
		header = r.RecordHeader
	}
	if err := validateRecordValue(record); err != nil {
		return RecordHeader{}, err
	}
	return header, nil
}

func validateRecordValue(v any) error {
	switch r := v.(type) {
	case *BlockerRecord:
		return validateRecord(*r)
	case *AssessmentRecord:
		return validateRecord(*r)
	case *AttestationRecord:
		return validateRecord(*r)
	case *AcceptedActionRecord:
		return validateRecord(*r)
	case *AdvisoryOutcomeRecord:
		return validateRecord(*r)
	default:
		return errors.New("unsupported recovery record")
	}
}

func (j *Journal) Append(v any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := validateRecord(v); err != nil {
		return err
	}
	var header *RecordHeader
	switch r := v.(type) {
	case BlockerRecord:
		header = &r.RecordHeader
	case AssessmentRecord:
		header = &r.RecordHeader
	case AttestationRecord:
		header = &r.RecordHeader
	case AcceptedActionRecord:
		header = &r.RecordHeader
	case AdvisoryOutcomeRecord:
		header = &r.RecordHeader
	}
	if _, ok := j.seen[header.RecordID]; ok {
		return errors.New("duplicate recovery record")
	}
	if header.Sequence != 0 {
		return errors.New("recovery journal sequence is writer-owned")
	}
	header.Sequence = j.next + 1
	var record any
	switch r := v.(type) {
	case BlockerRecord:
		r.RecordHeader.Sequence = header.Sequence
		record = r
	case AssessmentRecord:
		r.RecordHeader.Sequence = header.Sequence
		record = r
	case AttestationRecord:
		r.RecordHeader.Sequence = header.Sequence
		record = r
	case AcceptedActionRecord:
		r.RecordHeader.Sequence = header.Sequence
		record = r
	case AdvisoryOutcomeRecord:
		r.RecordHeader.Sequence = header.Sequence
		record = r
	}
	b, err := json.Marshal(record)
	if err != nil || len(b) > MaxRecordBytes {
		return errors.New("recovery record exceeds bound")
	}
	if err := ensureJournalDirectory(filepath.Dir(j.path)); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, e := f.Stat(); e != nil || info.Size()+int64(len(b)+1) > MaxJournalBytes {
		return errors.New("recovery journal exceeds bound")
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	j.seen[header.RecordID] = struct{}{}
	j.kinds[header.Kind]++
	if assessment, ok := record.(AssessmentRecord); ok {
		if j.assessments == nil {
			j.assessments = map[string]AssessmentRecord{}
		}
		j.assessments[assessment.RecordID] = assessment
	}
	j.next = header.Sequence
	return nil
}

// Count returns committed records of one kind, including records restored
// after restart. It supports bounded continuation budgets.
func (j *Journal) Count(kind RecordKind) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.kinds[kind]
}

// Has reports whether an immutable record id has already been committed.
// Sail uses this to make a recovered blocker a one-shot operation across
// process restarts.
func (j *Journal) Has(recordID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.seen[recordID]
	return ok
}

func (j *Journal) Assessment(recordID string) (AssessmentRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	assessment, ok := j.assessments[recordID]
	if !ok {
		return AssessmentRecord{}, false
	}
	assessment.DerivativePlan = append([]byte(nil), assessment.DerivativePlan...)
	return assessment, true
}

func (j *Journal) AdvisoryOutcome(fingerprint string) (AdvisoryOutcomeRecord, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id := range j.seen {
		_ = id
	}
	// Outcomes are identified by their immutable blocker fingerprint. The
	// bounded journal is small, so re-read only the in-memory index is kept
	// deliberately simple through the persisted record scan helper below.
	return j.outcomeLocked(fingerprint)
}

func (j *Journal) outcomeLocked(fingerprint string) (AdvisoryOutcomeRecord, bool) {
	b, err := os.ReadFile(j.path)
	if err != nil {
		return AdvisoryOutcomeRecord{}, false
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r AdvisoryOutcomeRecord
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&r) == nil && r.Kind == RecordAdvisoryOutcome && r.Fingerprint == fingerprint {
			return r, true
		}
	}
	return AdvisoryOutcomeRecord{}, false
}

func validAdvisoryCode(code string) bool {
	return code == AdvisoryAvailable || code == AdvisoryAuthUnavailable || code == AdvisoryTimeout || code == AdvisoryInvalidResponse || code == AdvisoryCleanupFailed
}

func (j *Journal) Assessments() []AssessmentRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]AssessmentRecord, 0, len(j.assessments))
	for _, assessment := range j.assessments {
		assessment.DerivativePlan = append([]byte(nil), assessment.DerivativePlan...)
		out = append(out, assessment)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Sequence < out[k].Sequence })
	return out
}

// Blockers returns a bounded snapshot of persisted blocker provenance. It is
// intentionally read-only; callers must still revalidate current task state
// before using a blocker to construct a new request.
func (j *Journal) Blockers() []BlockerRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j == nil {
		return nil
	}
	out := make([]BlockerRecord, 0, j.kinds[RecordBlocker])
	f, err := os.Open(j.path)
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, MaxJournalBytes+1))
	s.Buffer(make([]byte, 1024), MaxRecordBytes+1)
	for s.Scan() {
		var r BlockerRecord
		dec := json.NewDecoder(bytes.NewReader(s.Bytes()))
		dec.DisallowUnknownFields()
		if dec.Decode(&r) == nil && r.Kind == RecordBlocker {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Sequence < out[k].Sequence })
	return out
}

func ensureJournalDirectory(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	current := filepath.VolumeName(abs) + string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, current), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("recovery journal directory is unsafe")
		}
	}
	return nil
}

func DecodeRequest(raw []byte) (RequestV1, error) {
	if len(raw) == 0 || len(raw) > MaxRequestBytes {
		return RequestV1{}, errors.New("recovery request exceeds bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r RequestV1
	if err := dec.Decode(&r); err != nil {
		return RequestV1{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return RequestV1{}, errors.New("recovery request has trailing data")
	}
	return r, r.Validate()
}

func DecodeResponse(raw []byte) (ResponseV1, error) {
	if len(raw) == 0 || len(raw) > MaxRequestBytes {
		return ResponseV1{}, errors.New("recovery response exceeds bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r ResponseV1
	if err := dec.Decode(&r); err != nil {
		return ResponseV1{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return ResponseV1{}, errors.New("recovery response has trailing data")
	}
	return r, r.Validate()
}
