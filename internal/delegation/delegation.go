// Package delegation implements the ship-local, non-transport portion of
// Fleet Commander M1. It validates one signed offer, reserves one assessment,
// and persists only bounded redacted lifecycle/provenance records.
package delegation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"golang.org/x/sys/unix"
)

const (
	EnvelopeSchema    = "fleet.delegation-envelope.v1"
	RecordSchema      = "fleet.delegation-record.v1"
	ProtocolVersion   = 1
	EnvelopeMaxBytes  = 12 << 10
	RecordMaxBytes    = 16 << 10
	JournalMaxBytes   = 4 << 20
	JournalMaxRecords = 256
	digestDomain      = `shipmates/fleet-commander/m1/envelope-digest\0`
	signatureDomain   = `shipmates/fleet-commander/m1/envelope-signature\0`
	provenanceDomain  = `shipmates/fleet-commander/m1/decision-digest\0`
	maxReferences     = 16
)

type Code string

const (
	CodeInvalid            Code = "invalid"
	CodeRejected           Code = "rejected"
	CodeOptInDisabled      Code = "opt_in_disabled"
	CodeProvenanceMismatch Code = "provenance_mismatch"
	CodeExpired            Code = "expired"
	CodeIssuerRevoked      Code = "issuer_revoked"
	CodeRevoked            Code = "revoked"
	CodeAccepted           Code = "accepted"
	CodeDuplicate          Code = "duplicate"
	CodeConflict           Code = "id_conflict"
	CodeRestart            Code = "restart_after_assessment"
	CodeResponseInvalid    Code = "response_invalid"
	CodeRevokedAfterStart  Code = "revoked_after_start"
	CodeAdvised            Code = "advised"
)

type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) }
func fail(c Code) error        { return &Error{Code: c} }
func IsCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type Reference struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

type Envelope struct {
	Schema             string      `json:"schema"`
	DelegationID       string      `json:"delegation_id"`
	IssuerKeyID        string      `json:"issuer_key_id"`
	FleetID            string      `json:"fleet_id"`
	ShipID             string      `json:"ship_id"`
	VoyagePlanHash     string      `json:"voyage_plan_hash"`
	TaskContractHash   string      `json:"task_contract_hash"`
	StateHash          string      `json:"state_hash"`
	TaskID             string      `json:"task_id"`
	BlockerFingerprint string      `json:"blocker_fingerprint"`
	Mode               string      `json:"mode"`
	AssessmentBudget   uint8       `json:"assessment_budget"`
	ResponseSchema     string      `json:"response_schema"`
	References         []Reference `json:"references"`
	IssuedAt           time.Time   `json:"issued_at"`
	ExpiresAt          time.Time   `json:"expires_at"`
	Signature          string      `json:"signature"`
}

type Issuer struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	Revoked   bool
}

type Policy struct {
	Enabled         bool
	FleetID         string
	ProtocolVersion uint8
	MaxOfferLife    time.Duration
	PolicyVersion   uint8
	Issuers         []Issuer
}

func PolicyFromConfig(c recovery.CommanderDelegationConfig) (Policy, error) {
	p := Policy{Enabled: c.Enabled, FleetID: c.FleetID, ProtocolVersion: c.ProtocolVersion, MaxOfferLife: time.Duration(c.MaxOfferSeconds) * time.Second, PolicyVersion: 1}
	for _, in := range c.PermittedIssuers {
		key, err := base64.RawURLEncoding.DecodeString(in.PublicKey)
		if err != nil {
			return Policy{}, fail(CodeInvalid)
		}
		p.Issuers = append(p.Issuers, Issuer{KeyID: in.KeyID, PublicKey: append(ed25519.PublicKey(nil), key...), Revoked: in.Revoked})
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func (p Policy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if !validOpaque(p.FleetID) || p.ProtocolVersion != ProtocolVersion || p.PolicyVersion == 0 || p.MaxOfferLife <= 0 || p.MaxOfferLife > 10*time.Minute || len(p.Issuers) == 0 || len(p.Issuers) > 32 {
		return fail(CodeInvalid)
	}
	seen := map[string]bool{}
	for _, issuer := range p.Issuers {
		if !validOpaque(issuer.KeyID) || len(issuer.PublicKey) != ed25519.PublicKeySize || seen[issuer.KeyID] {
			return fail(CodeInvalid)
		}
		seen[issuer.KeyID] = true
	}
	return nil
}

type LocalCase struct {
	FleetID                string
	ShipID                 string
	VoyagePlanHash         string
	TaskContractHash       string
	StateHash              string
	TaskID                 string
	BlockerFingerprint     string
	Request                recovery.RequestV1
	SkipperArtifactDigest  string
	SkipperArtifactVersion uint8
}

func (c LocalCase) Validate() error {
	if !validOpaque(c.FleetID) || !validOpaque(c.ShipID) || !validDigest(c.VoyagePlanHash) || !validDigest(c.TaskContractHash) || !validDigest(c.StateHash) || !validTask(c.TaskID) || !validDigest(c.BlockerFingerprint) || !validDigest(c.SkipperArtifactDigest) || c.SkipperArtifactVersion == 0 || c.SkipperArtifactVersion > 8 {
		return fail(CodeProvenanceMismatch)
	}
	if err := c.Request.Validate(); err != nil || c.Request.Provenance.VoyagePlanHash != c.VoyagePlanHash || c.Request.Provenance.TaskContractHash != c.TaskContractHash || c.Request.Provenance.StateHash != c.StateHash || c.Request.Provenance.TaskID != c.TaskID {
		return fail(CodeProvenanceMismatch)
	}
	fingerprint, err := recovery.Fingerprint(c.Request)
	if err != nil || fingerprint != c.BlockerFingerprint {
		return fail(CodeProvenanceMismatch)
	}
	return nil
}

type Assessment interface {
	Assess(context.Context, recovery.RequestV1) (recovery.ResponseV1, error)
}

type Validator func(recovery.RequestV1, recovery.ResponseV1) error

// Outcome contains only the fixed M1 result vocabulary and a redacted
// decision. It intentionally has no raw advisory, prompt, path, or ID tuple.
type Outcome struct {
	ReceiptResult Code
	ReceiptReason Code
	Decision      *Decision
}

type Decision struct {
	Result           string
	ReasonCode       Code
	AdvisoryDecision recovery.Decision
	ProvenanceDigest string
	SailState        string
}

type Processor struct {
	mu         sync.Mutex
	policy     Policy
	journal    *Journal
	assessment Assessment
	validator  Validator
	revoked    func(string) bool
	now        func() time.Time
}

func Open(root, planHash string, policy Policy, assessment Assessment) (*Processor, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return &Processor{policy: policy, assessment: assessment, now: func() time.Time { return time.Now().UTC() }}, nil
	}
	path, err := project.DelegationJournalPathAt(root, planHash)
	if err != nil {
		return nil, err
	}
	j, err := OpenJournal(path, planHash)
	if err != nil {
		return nil, err
	}
	return &Processor{policy: policy, journal: j, assessment: assessment, validator: recovery.ValidateAdvisory, now: func() time.Time { return time.Now().UTC() }}, nil
}

type Hooks struct {
	Validator Validator
	Revoked   func(string) bool
	Now       func() time.Time
}

// openWithHooks is the testable composition seam. Hooks are local policy
// dependencies only; they cannot add authority or change the envelope.
func openWithHooks(root, planHash string, policy Policy, assessment Assessment, hooks Hooks) (*Processor, error) {
	p, err := Open(root, planHash, policy, assessment)
	if err != nil {
		return nil, err
	}
	if hooks.Validator != nil {
		p.validator = hooks.Validator
	}
	if hooks.Revoked != nil {
		p.revoked = hooks.Revoked
	}
	if hooks.Now != nil {
		p.now = hooks.Now
	}
	return p, nil
}

// Process is the complete local M1 boundary. The raw envelope is the only
// externally supplied value; LocalCase is read from already-approved local
// state and never serialized into a response.
// AcceptAndAssess is the sole exported state-changing delegation operation.
// It validates exact local provenance, durably reserves one assessment, runs
// the injected local adviser once, and commits only redacted terminal state.
func (p *Processor) AcceptAndAssess(ctx context.Context, raw []byte, local LocalCase) (Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.policy.Enabled {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: CodeOptInDisabled}, fail(CodeOptInDisabled)
	}
	if err := local.Validate(); err != nil {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: CodeProvenanceMismatch}, err
	}
	release, err := p.journal.acquire()
	if err != nil {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: CodeInvalid}, err
	}
	defer release()
	envelope, digest, err := DecodeAndVerify(raw, p.policy, p.now())
	if err != nil {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: codeOf(err)}, err
	}
	if err := envelope.bind(local); err != nil {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: CodeProvenanceMismatch}, err
	}
	if prior, ok := p.journal.Lookup(envelope.DelegationID); ok {
		if prior.EnvelopeDigest != digest {
			return Outcome{ReceiptResult: CodeRejected, ReceiptReason: CodeConflict}, fail(CodeConflict)
		}
		out, priorErr := prior.outcome(), prior.err()
		if prior.Lifecycle == lifecycleAdvised || prior.Lifecycle == lifecycleRejected || prior.Lifecycle == lifecycleExpired || prior.Lifecycle == lifecycleRevoked {
			out.ReceiptResult, out.ReceiptReason = CodeDuplicate, CodeDuplicate
		}
		return out, priorErr
	}
	if err := p.journal.reserve(envelope, digest, p.now()); err != nil {
		return Outcome{ReceiptResult: CodeRejected, ReceiptReason: codeOf(err)}, err
	}
	if p.issuerRevoked(envelope.IssuerKeyID) {
		decision, finalErr := p.journal.finalize(envelope, digest, "revoked", CodeRevoked, "none", recovery.Decision(""), "not_evaluated", local, "")
		if finalErr != nil {
			return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, finalErr
		}
		return decision.outcome(), fail(CodeIssuerRevoked)
	}
	if p.assessment == nil {
		return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, errors.New("assessment unavailable")
	}
	response, assessErr := p.assessment.Assess(ctx, local.Request)
	if assessErr != nil {
		// The durable assessment_started record is intentionally retained. A
		// restart must return indeterminate rather than run a second turn.
		return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, assessErr
	}
	if p.issuerRevoked(envelope.IssuerKeyID) {
		decision, finalErr := p.journal.finalize(envelope, digest, "revoked", CodeRevokedAfterStart, "none", recovery.Decision(""), "not_evaluated", local, "")
		if finalErr != nil {
			return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, finalErr
		}
		return decision.outcome(), fail(CodeRevokedAfterStart)
	}
	validator := p.validator
	if validator == nil {
		validator = recovery.ValidateAdvisory
	}
	if err := validator(local.Request, response); err != nil || response.Fingerprint != local.BlockerFingerprint {
		decision, finalErr := p.journal.finalize(envelope, digest, "rejected", CodeResponseInvalid, "none", recovery.Decision(""), "not_evaluated", local, "")
		if finalErr != nil {
			return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, finalErr
		}
		return decision.outcome(), fail(CodeResponseInvalid)
	}
	decision, finalErr := p.journal.finalize(envelope, digest, "advised", CodeAdvised, "advised", response.Decision, "locally_accepted_under_existing_policy", local, response.Decision)
	if finalErr != nil {
		return Outcome{ReceiptResult: CodeAccepted, ReceiptReason: CodeAccepted}, finalErr
	}
	return decision.outcome(), nil
}

func (p *Processor) issuerRevoked(keyID string) bool {
	if p.revoked != nil {
		return p.revoked(keyID)
	}
	for _, issuer := range p.policy.Issuers {
		if issuer.KeyID == keyID {
			return issuer.Revoked
		}
	}
	return true
}

// Lookup is the read-only M2 provenance seam used by the ship mailbox. It
// returns only the durable redacted lifecycle record; it never rehydrates or
// invokes an assessment.
func (p *Processor) Lookup(id string) (Record, bool) {
	if p == nil || p.journal == nil || id == "" {
		return Record{}, false
	}
	return p.journal.Lookup(id)
}

func (e Envelope) bind(c LocalCase) error {
	if e.FleetID != c.FleetID || e.ShipID != c.ShipID || e.VoyagePlanHash != c.VoyagePlanHash || e.TaskContractHash != c.TaskContractHash || e.StateHash != c.StateHash || e.TaskID != c.TaskID || e.BlockerFingerprint != c.BlockerFingerprint || e.Mode != "read_only_recovery_assessment" || e.AssessmentBudget != 1 || e.ResponseSchema != "recovery.response.v1" {
		return fail(CodeProvenanceMismatch)
	}
	return nil
}

func codeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInvalid
}

func DecodeAndVerify(raw []byte, policy Policy, now time.Time) (Envelope, string, error) {
	if err := policy.Validate(); err != nil {
		return Envelope{}, "", fail(CodeInvalid)
	}
	if len(raw) == 0 || len(raw) > EnvelopeMaxBytes {
		return Envelope{}, "", fail(CodeInvalid)
	}
	value, err := parseJSON(raw)
	if err != nil {
		return Envelope{}, "", fail(CodeInvalid)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return Envelope{}, "", fail(CodeInvalid)
	}
	if !closedEnvelopeObject(obj) {
		return Envelope{}, "", fail(CodeInvalid)
	}
	unsigned := cloneObject(obj)
	sigValue, ok := unsigned["signature"].(string)
	if !ok {
		return Envelope{}, "", fail(CodeInvalid)
	}
	delete(unsigned, "signature")
	canonical, err := canonicalJSON(unsigned)
	if err != nil {
		return Envelope{}, "", fail(CodeInvalid)
	}
	var envelope Envelope
	b, _ := json.Marshal(obj)
	if err := json.Unmarshal(b, &envelope); err != nil || !envelope.validate(policy) || envelope.Signature != sigValue {
		return Envelope{}, "", fail(CodeInvalid)
	}
	if !now.Before(envelope.ExpiresAt) {
		return Envelope{}, "", fail(CodeExpired)
	}
	digest := digestWith(digestDomain, canonical)
	issuer, ok := policyIssuer(policy, envelope.IssuerKeyID)
	if !ok {
		return Envelope{}, "", fail(CodeInvalid)
	}
	if issuer.Revoked {
		return Envelope{}, "", fail(CodeIssuerRevoked)
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.PublicKey, append([]byte(signatureDomain), canonical...), signature) {
		return Envelope{}, "", fail(CodeInvalid)
	}
	return envelope, digest, nil
}

// closedEnvelopeObject enforces the frozen schema before an object can reach
// either canonicalization or typed decoding. json.Unmarshal alone would ignore
// an unknown signed field, which would turn a closed delegation into an
// extensible command carrier.
func closedEnvelopeObject(obj map[string]any) bool {
	if len(obj) != 17 {
		return false
	}
	for key := range obj {
		switch key {
		case "schema", "delegation_id", "issuer_key_id", "fleet_id", "ship_id",
			"voyage_plan_hash", "task_contract_hash", "state_hash", "task_id",
			"blocker_fingerprint", "mode", "assessment_budget", "response_schema",
			"references", "issued_at", "expires_at", "signature":
		default:
			return false
		}
	}
	return true
}

func policyIssuer(policy Policy, keyID string) (Issuer, bool) {
	for _, issuer := range policy.Issuers {
		if issuer.KeyID == keyID {
			return issuer, true
		}
	}
	return Issuer{}, false
}

func (e Envelope) validate(policy Policy) bool {
	if e.Schema != EnvelopeSchema || !validOpaque(e.DelegationID) || !validOpaque(e.IssuerKeyID) || !validOpaque(e.FleetID) || !validOpaque(e.ShipID) || !validDigest(e.VoyagePlanHash) || !validDigest(e.TaskContractHash) || !validDigest(e.StateHash) || !validTask(e.TaskID) || !validDigest(e.BlockerFingerprint) || e.Mode != "read_only_recovery_assessment" || e.AssessmentBudget != 1 || e.ResponseSchema != "recovery.response.v1" || len(e.References) > maxReferences || !e.IssuedAt.Before(e.ExpiresAt) || e.ExpiresAt.Sub(e.IssuedAt) > 10*time.Minute || e.ExpiresAt.Sub(e.IssuedAt) > policy.MaxOfferLife || !sameMillis(e.IssuedAt) || !sameMillis(e.ExpiresAt) || len(e.Signature) != 86 {
		return false
	}
	if e.FleetID != policy.FleetID {
		return false
	}
	for _, ref := range e.References {
		if (ref.Kind != "recovery_evidence_ref" && ref.Kind != "evidence_certificate_ref") || !validDigest(ref.Digest) {
			return false
		}
	}
	return true
}

func sameMillis(t time.Time) bool {
	return t.UTC().Format("2006-01-02T15:04:05.000Z") == t.UTC().Format(time.RFC3339Nano)
}

func validOpaque(s string) bool {
	if len(s) < 16 || len(s) > 96 || !isAlphaNumeric(rune(s[0])) {
		return false
	}
	for _, c := range s[1:] {
		if !isAlphaNumeric(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
func validTask(s string) bool {
	if len(s) == 0 || len(s) > 96 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, c := range s[1:] {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
func isAlphaNumeric(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func digestWith(domain string, canonical []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// CanonicalUnsigned returns the frozen JCS-compatible unsigned envelope.
func CanonicalUnsigned(raw []byte) ([]byte, error) {
	value, err := parseJSON(raw)
	if err != nil {
		return nil, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("envelope must be an object")
	}
	if _, ok := obj["signature"]; !ok {
		return nil, errors.New("envelope signature missing")
	}
	delete(obj, "signature")
	return canonicalJSON(obj)
}

func EnvelopeDigest(raw []byte) (string, error) {
	b, err := CanonicalUnsigned(raw)
	if err != nil {
		return "", err
	}
	return digestWith(digestDomain, b), nil
}

// parseJSON rejects duplicate object keys recursively and trailing values.
// The M1 value domain contains only strings, booleans, null, arrays, objects,
// and bounded integers, so rejecting non-integer numbers is intentional.
func parseJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			out := map[string]any{}
			for dec.More() {
				key, ok := mustStringToken(dec)
				if !ok || outKey(out, key) {
					return nil, errors.New("duplicate or invalid JSON key")
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				out[key] = v
			}
			_, err = dec.Token()
			return out, err
		case '[':
			var out []any
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err = dec.Token()
			return out, err
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		if !isInteger(string(t)) {
			return nil, errors.New("non-integer JSON number")
		}
		return t, nil
	case string:
		if !utf8.ValidString(t) {
			return nil, errors.New("invalid UTF-8")
		}
		return t, nil
	case bool, nil:
		return t, nil
	default:
		return nil, errors.New("unsupported JSON value")
	}
}

func mustStringToken(dec *json.Decoder) (string, bool) {
	t, err := dec.Token()
	s, ok := t.(string)
	return s, err == nil && ok && utf8.ValidString(s)
}
func outKey(m map[string]any, key string) bool { _, ok := m[key]; return ok }
func isInteger(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" || s[0] == '-' {
		if len(s) < 2 || s[1] == '0' {
			return false
		}
		s = s[1:]
	}
	if s[0] < '1' || s[0] > '9' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func cloneObject(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func canonicalJSON(v any) ([]byte, error) {
	var b []byte
	if err := appendCanonical(&b, v); err != nil {
		return nil, err
	}
	return b, nil
}

func appendCanonical(dst *[]byte, v any) error {
	switch x := v.(type) {
	case nil:
		*dst = append(*dst, "null"...)
	case bool:
		if x {
			*dst = append(*dst, "true"...)
		} else {
			*dst = append(*dst, "false"...)
		}
	case string:
		q, _ := json.Marshal(x)
		*dst = append(*dst, q...)
	case json.Number:
		if !isInteger(string(x)) {
			return errors.New("non-canonical number")
		}
		*dst = append(*dst, string(x)...)
	case []any:
		*dst = append(*dst, '[')
		for i, item := range x {
			if i > 0 {
				*dst = append(*dst, ',')
			}
			if err := appendCanonical(dst, item); err != nil {
				return err
			}
		}
		*dst = append(*dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		*dst = append(*dst, '{')
		for i, key := range keys {
			if i > 0 {
				*dst = append(*dst, ',')
			}
			q, _ := json.Marshal(key)
			*dst = append(*dst, q...)
			*dst = append(*dst, ':')
			if err := appendCanonical(dst, x[key]); err != nil {
				return err
			}
		}
		*dst = append(*dst, '}')
	default:
		return fmt.Errorf("unsupported canonical value %T", v)
	}
	return nil
}

type lifecycle string

const (
	lifecycleAccepted      lifecycle = "accepted"
	lifecycleStarted       lifecycle = "assessment_started"
	lifecycleAdvised       lifecycle = "advised"
	lifecycleRejected      lifecycle = "rejected"
	lifecycleExpired       lifecycle = "expired"
	lifecycleRevoked       lifecycle = "revoked"
	lifecycleIndeterminate lifecycle = "indeterminate"
)

type Record struct {
	Schema             string            `json:"schema"`
	RecordID           string            `json:"record_id"`
	Sequence           uint64            `json:"sequence"`
	DelegationID       string            `json:"delegation_id"`
	EnvelopeDigest     string            `json:"envelope_digest"`
	VoyagePlanHash     string            `json:"voyage_plan_hash"`
	TaskContractHash   string            `json:"task_contract_hash"`
	StateHash          string            `json:"state_hash"`
	BlockerFingerprint string            `json:"blocker_fingerprint"`
	PolicyVersion      uint8             `json:"local_policy_version"`
	SkipperDigest      string            `json:"skipper_artifact_digest"`
	SkipperVersion     uint8             `json:"skipper_artifact_version"`
	EffectiveModel     string            `json:"effective_model"`
	RecoverySchema     string            `json:"recovery_schema,omitempty"`
	References         []Reference       `json:"references"`
	CreatedAt          time.Time         `json:"created_at"`
	Lifecycle          lifecycle         `json:"lifecycle"`
	ReasonCode         Code              `json:"reason_code"`
	Result             string            `json:"result"`
	AdvisoryDecision   recovery.Decision `json:"advisory_decision,omitempty"`
	ProvenanceDigest   string            `json:"provenance_digest,omitempty"`
	SailState          string            `json:"sail_state"`
}

// StartWitnessDigest identifies the immutable M2 assessment_started record.
// It is an observation only: callers cannot create or advance M2 lifecycle
// state with it.
func StartWitnessDigest(r Record) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Journal struct {
	mu       sync.Mutex
	path     string
	lockPath string
	planHash string
	next     uint64
	count    int
	byID     map[string]Record
}

func OpenJournal(path, planHash string) (*Journal, error) {
	if !validDigest(planHash) || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != planHash+".jsonl" {
		return nil, fail(CodeInvalid)
	}
	if err := ensureParents(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := openJournalFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > JournalMaxBytes {
		return nil, errors.New("invalid delegation journal")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	j := &Journal{path: path, lockPath: path + ".lock", planHash: planHash, byID: map[string]Record{}}
	s := bufio.NewScanner(io.LimitReader(f, JournalMaxBytes+1))
	s.Buffer(make([]byte, 1024), RecordMaxBytes+1)
	for s.Scan() {
		if len(s.Bytes()) > RecordMaxBytes {
			return nil, errors.New("delegation record exceeds bound")
		}
		var r Record
		if err := decodeRecord(s.Bytes(), &r); err != nil || r.Sequence == 0 || r.RecordID == "" {
			return nil, errors.New("corrupt delegation journal")
		}
		prior, exists := j.byID[r.DelegationID]
		validTransition := !exists && r.Lifecycle == lifecycleAccepted || exists && prior.Lifecycle == lifecycleAccepted && r.Lifecycle == lifecycleStarted || exists && prior.Lifecycle == lifecycleStarted && (r.Lifecycle == lifecycleAdvised || r.Lifecycle == lifecycleRejected || r.Lifecycle == lifecycleExpired || r.Lifecycle == lifecycleRevoked || r.Lifecycle == lifecycleIndeterminate)
		if r.Sequence != j.next+1 || j.count >= JournalMaxRecords || !validTransition {
			return nil, errors.New("invalid delegation journal sequence")
		}
		j.next, j.count = r.Sequence, j.count+1
		j.byID[r.DelegationID] = r
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Journal) Lookup(id string) (Record, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.byID[id]
	return r, ok
}
func (r Record) outcome() Outcome {
	o := Outcome{}
	switch r.Lifecycle {
	case lifecycleAdvised:
		o.ReceiptResult, o.ReceiptReason = CodeAccepted, CodeAccepted
	case lifecycleRejected:
		o.ReceiptResult, o.ReceiptReason = CodeRejected, CodeInvalid
	case lifecycleExpired:
		o.ReceiptResult, o.ReceiptReason = CodeExpired, CodeExpired
	case lifecycleRevoked:
		o.ReceiptResult, o.ReceiptReason = CodeRevoked, r.ReasonCode
	default:
		o.ReceiptResult, o.ReceiptReason = CodeAccepted, CodeAccepted
	}
	if r.Lifecycle == lifecycleAdvised || r.Lifecycle == lifecycleRejected || r.Lifecycle == lifecycleExpired || r.Lifecycle == lifecycleRevoked || r.Lifecycle == lifecycleIndeterminate {
		o.Decision = &Decision{Result: r.Result, ReasonCode: r.ReasonCode, AdvisoryDecision: r.AdvisoryDecision, ProvenanceDigest: r.ProvenanceDigest, SailState: r.SailState}
	}
	if r.Lifecycle == lifecycleStarted {
		o.Decision = &Decision{Result: "none", ReasonCode: CodeRestart, SailState: "not_evaluated"}
	}
	return o
}
func (r Record) err() error {
	switch r.Lifecycle {
	case lifecycleAdvised:
		return nil
	case lifecycleRejected:
		return fail(r.ReasonCode)
	case lifecycleExpired:
		return fail(CodeExpired)
	case lifecycleRevoked:
		return fail(r.ReasonCode)
	case lifecycleIndeterminate:
		return fail(CodeRestart)
	case lifecycleStarted:
		return fail(CodeRestart)
	}
	return nil
}

func (j *Journal) reserve(e Envelope, digest string, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	// This is the durable accepted -> assessment_started transition. Recheck
	// here rather than relying on DecodeAndVerify's earlier clock read so an
	// offer cannot be accepted and then start after its signed expiry.
	if !now.Before(e.ExpiresAt) {
		return fail(CodeExpired)
	}
	if existing, ok := j.byID[e.DelegationID]; ok {
		if existing.EnvelopeDigest != digest {
			return fail(CodeConflict)
		}
		return nil
	}
	if j.count+2 > JournalMaxRecords {
		return errors.New("delegation journal backpressure")
	}
	base := Record{Schema: RecordSchema, DelegationID: e.DelegationID, EnvelopeDigest: digest, VoyagePlanHash: e.VoyagePlanHash, TaskContractHash: e.TaskContractHash, StateHash: e.StateHash, BlockerFingerprint: e.BlockerFingerprint, CreatedAt: now.UTC(), PolicyVersion: 1, EffectiveModel: "gpt-5.6-sol", Lifecycle: lifecycleAccepted, ReasonCode: CodeAccepted, Result: "none", SailState: "not_evaluated"}
	started := base
	started.Lifecycle = lifecycleStarted
	base.RecordID, started.RecordID = recordID(e.DelegationID, digest, "accepted"), recordID(e.DelegationID, digest, "started")
	base.Sequence, started.Sequence = j.next+1, j.next+2
	if err := j.appendLocked([]Record{base, started}); err != nil {
		return err
	}
	j.byID[e.DelegationID], j.next, j.count = started, started.Sequence, j.count+2
	return nil
}

func (j *Journal) finalize(e Envelope, digest, life string, reason Code, result string, advisory recovery.Decision, sailState string, local LocalCase, _ recovery.Decision) (Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := j.byID[e.DelegationID]
	r.RecordID, r.Sequence = recordID(e.DelegationID, digest, life), j.next+1
	r.CreatedAt, r.Lifecycle, r.ReasonCode, r.Result, r.AdvisoryDecision, r.SailState = time.Now().UTC(), lifecycle(life), reason, result, advisory, sailState
	r.PolicyVersion, r.SkipperDigest, r.SkipperVersion, r.EffectiveModel = 1, local.SkipperArtifactDigest, local.SkipperArtifactVersion, "gpt-5.6-sol"
	r.Schema, r.RecoverySchema = "fleet.delegation-decision.v1", "recovery.response.v1"
	r.References = append([]Reference{}, e.References...)
	// Every terminal result, including fail-safe rejection, expiry, revocation,
	// and indeterminacy, is an append-only redacted decision provenance record.
	// The digest is therefore never optional merely because Sol produced no
	// advisory.
	r.ProvenanceDigest = provenanceDigest(r)
	if err := j.appendLocked([]Record{r}); err != nil {
		return r, err
	}
	j.byID[e.DelegationID], j.next, j.count = r, r.Sequence, j.count+1
	return r, nil
}

func (j *Journal) acquire() (func(), error) {
	f, err := openLockFile(j.lockPath)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Each Processor has an in-memory index. Refresh it only after taking the
	// cross-process lock, otherwise two Processors opened before the first
	// reservation can both observe an empty index and start separate Sol turns.
	fresh, err := OpenJournal(j.path, j.planHash)
	if err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		return nil, err
	}
	j.mu.Lock()
	j.next, j.count, j.byID = fresh.next, fresh.count, fresh.byID
	j.mu.Unlock()
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

func recordID(id, digest, stage string) string {
	return "rec_" + digestWith(`shipmates/fleet-commander/m1/record\0`, []byte(id+"\x00"+stage))
}
func provenanceDigest(r Record) string {
	projection := struct {
		Schema             string            `json:"schema"`
		RecordID           string            `json:"record_id"`
		DelegationID       string            `json:"delegation_id"`
		EnvelopeDigest     string            `json:"envelope_digest"`
		VoyagePlanHash     string            `json:"voyage_plan_hash"`
		TaskContractHash   string            `json:"task_contract_hash"`
		StateHash          string            `json:"state_hash"`
		BlockerFingerprint string            `json:"blocker_fingerprint"`
		PolicyVersion      uint8             `json:"local_policy_version"`
		SkipperDigest      string            `json:"skipper_artifact_digest"`
		SkipperVersion     uint8             `json:"skipper_artifact_version"`
		EffectiveModel     string            `json:"effective_model"`
		RecoverySchema     string            `json:"recovery_schema"`
		CreatedAt          time.Time         `json:"created_at"`
		Lifecycle          lifecycle         `json:"lifecycle"`
		ReasonCode         Code              `json:"reason_code"`
		References         []Reference       `json:"references"`
		Result             string            `json:"result"`
		AdvisoryDecision   recovery.Decision `json:"advisory_decision,omitempty"`
		SailState          string            `json:"sail_state"`
	}{r.Schema, r.RecordID, r.DelegationID, r.EnvelopeDigest, r.VoyagePlanHash, r.TaskContractHash, r.StateHash, r.BlockerFingerprint, r.PolicyVersion, r.SkipperDigest, r.SkipperVersion, r.EffectiveModel, r.RecoverySchema, r.CreatedAt, r.Lifecycle, r.ReasonCode, r.References, r.Result, r.AdvisoryDecision, r.SailState}
	b, _ := json.Marshal(projection)
	value, _ := parseJSON(b)
	canonical, _ := canonicalJSON(value)
	return digestWith(provenanceDomain, canonical)
}

func (j *Journal) appendLocked(records []Record) error {
	f, err := openJournalFile(j.path)
	if err != nil {
		return err
	}
	defer f.Close()
	var b bytes.Buffer
	for i := range records {
		records[i].Sequence = j.next + uint64(i) + 1
		raw, err := json.Marshal(records[i])
		if err != nil || len(raw) > RecordMaxBytes {
			return errors.New("delegation record exceeds bound")
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if int64(b.Len())+fileSize(f) > JournalMaxBytes {
		return errors.New("delegation journal exceeds bound")
	}
	if _, err := f.Write(b.Bytes()); err != nil || f.Sync() != nil {
		return errors.New("delegation journal commit failed")
	}
	return nil
}
func fileSize(f *os.File) int64 {
	info, _ := f.Stat()
	if info == nil {
		return 0
	}
	return info.Size()
}

func decodeRecord(raw []byte, out *Record) error {
	if _, err := parseJSON(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing record")
	}
	if (out.Schema != RecordSchema && out.Schema != "fleet.delegation-decision.v1") || out.Sequence == 0 || !validOpaque(out.RecordID) || !validOpaque(out.DelegationID) || !validDigest(out.EnvelopeDigest) || !validDigest(out.VoyagePlanHash) || !validDigest(out.TaskContractHash) || !validDigest(out.StateHash) || !validDigest(out.BlockerFingerprint) || out.EffectiveModel != "gpt-5.6-sol" || out.CreatedAt.IsZero() {
		return errors.New("invalid delegation record")
	}
	if out.Schema == "fleet.delegation-decision.v1" && (!validDigest(out.SkipperDigest) || out.SkipperVersion == 0 || out.SkipperVersion > 8 || out.RecoverySchema != "recovery.response.v1" || len(out.References) > maxReferences || (out.SailState != "not_evaluated" && out.SailState != "advisory_rejected" && out.SailState != "locally_accepted_under_existing_policy")) {
		return errors.New("invalid delegation decision")
	}
	return nil
}

func ensureParents(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("unsafe delegation parent")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The project root and system ancestors may intentionally be searchable;
	// the private boundary is the .shipmates/delegations pair created here.
	for _, current := range []string{filepath.Dir(dir), dir} {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return errors.New("unsafe delegation parent")
		}
	}
	return nil
}

func openJournalFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open delegation journal failed")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return nil, errors.New("unsafe delegation journal")
	}
	return f, nil
}

func openLockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open delegation lock failed")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		_ = f.Close()
		return nil, errors.New("unsafe delegation lock")
	}
	return f, nil
}
