package recovery

// This file contains the pure policy boundary for bounded derivative plans.
// It produces an approval artifact but never writes a plan, state, Beads
// record, or journal entry and never activates a successor.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/voyage"
)

const DerivativePolicyVersion = "derivative-policy-v1"

var derivativeID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type DerivativeAction string

const (
	ActionRetry      DerivativeAction = "retry"
	ActionSuccessor  DerivativeAction = "successor"
	ActionAssessment DerivativeAction = "assessment"
)

func (a DerivativeAction) valid() bool {
	return a == ActionRetry || a == ActionSuccessor || a == ActionAssessment
}

type SlotType string

const (
	SlotString SlotType = "string"
	SlotModel  SlotType = "model"
	SlotEffort SlotType = "effort"
	SlotUint   SlotType = "uint"
)

type TemplateSlot struct {
	Type     SlotType `json:"type"`
	MaxBytes uint16   `json:"max_bytes"`
}

type ContinuationTemplate struct {
	ID        string                  `json:"id"`
	Action    DerivativeAction        `json:"action"`
	Slots     map[string]TemplateSlot `json:"slots,omitempty"`
	Successor *voyage.Task            `json:"successor,omitempty"`
}

// ChangeEnvelope is the Captain-approved ceiling for a derivative. Fields
// omitted from a proposal are immutable; an envelope never grants authority
// to change plan semantics or approvals.
type ChangeEnvelope struct {
	SchemaVersion       uint64                 `json:"schema_version"`
	ID                  string                 `json:"id"`
	ParentPlanHash      string                 `json:"parent_plan_hash"`
	ExpiresAt           time.Time              `json:"expires_at"`
	MaxOperations       uint8                  `json:"max_operations"`
	MaxRetries          uint8                  `json:"max_retries"`
	MaxSuccessors       uint8                  `json:"max_successors"`
	MaxAssessments      uint8                  `json:"max_assessments"`
	AllowedCapabilities []string               `json:"allowed_capabilities,omitempty"`
	AllowedEffects      []string               `json:"allowed_effects,omitempty"`
	AllowedModels       []string               `json:"allowed_models,omitempty"`
	AllowedEfforts      []string               `json:"allowed_efforts,omitempty"`
	Templates           []ContinuationTemplate `json:"templates,omitempty"`
}

type PatchOperation struct {
	Op         string          `json:"op"`
	Path       string          `json:"path"`
	Value      json.RawMessage `json:"value"`
	Capability string          `json:"capability"`
	Effect     string          `json:"effect"`
}

type DerivativeProposal struct {
	SchemaVersion   uint64            `json:"schema_version"`
	ProposalID      string            `json:"proposal_id"`
	EnvelopeID      string            `json:"envelope_id"`
	ParentPlanHash  string            `json:"parent_plan_hash"`
	Action          DerivativeAction  `json:"action"`
	RetryCount      uint8             `json:"retry_count,omitempty"`
	SuccessorCount  uint8             `json:"successor_count,omitempty"`
	AssessmentCount uint8             `json:"assessment_count,omitempty"`
	TemplateID      string            `json:"template_id,omitempty"`
	Slots           map[string]string `json:"slots,omitempty"`
	Operations      []PatchOperation  `json:"operations,omitempty"`
	Evidence        []EvidenceRef     `json:"evidence,omitempty"`
	Provenance      Provenance        `json:"provenance"`
}

type MachineApprovalAttestation struct {
	SchemaVersion       uint64           `json:"schema_version"`
	AttestationID       string           `json:"attestation_id"`
	ParentPlanHash      string           `json:"parent_plan_hash"`
	ChildPlanHash       string           `json:"child_plan_hash"`
	EnvelopeID          string           `json:"envelope_id"`
	CanonicalDiff       string           `json:"canonical_diff"`
	InvariantHash       string           `json:"invariant_hash"`
	PolicyEngineVersion string           `json:"policy_engine_version"`
	Action              DerivativeAction `json:"action"`
	Provenance          Provenance       `json:"provenance"`
	CreatedAt           time.Time        `json:"created_at"`
}

type DerivativeResult struct {
	Plan            *voyage.Plan
	CanonicalPlan   []byte
	PlanHash        string
	CanonicalDiff   string
	InvariantHash   string
	Attestation     MachineApprovalAttestation
	ReusableTaskIDs []string
	Replayed        bool
	Evidence        []EvidenceRef
}

func (a MachineApprovalAttestation) Validate() error {
	if a.SchemaVersion != SchemaVersion || !validHash(a.AttestationID) || !validHash(a.ParentPlanHash) || !validHash(a.ChildPlanHash) || !derivativeID.MatchString(a.EnvelopeID) || a.CanonicalDiff == "" || len(a.CanonicalDiff) > 16<<10 || !validHash(a.InvariantHash) || a.PolicyEngineVersion != DerivativePolicyVersion || !a.Action.valid() || a.CreatedAt.IsZero() {
		return errors.New("invalid machine approval attestation")
	}
	if err := a.Provenance.validate(); err != nil {
		return err
	}
	if err := rejectDuplicateJSON([]byte(a.CanonicalDiff)); err != nil {
		return errors.New("invalid canonical derivative diff")
	}
	return nil
}

func ValidateDerivativeArtifact(planBytes []byte, att MachineApprovalAttestation) error {
	if err := att.Validate(); err != nil {
		return err
	}
	if len(planBytes) == 0 || len(planBytes) > voyage.MaxPlanBytes || voyage.Hash(planBytes) != att.ChildPlanHash {
		return errors.New("derivative plan hash does not match attestation")
	}
	dec := json.NewDecoder(bytes.NewReader(planBytes))
	dec.DisallowUnknownFields()
	var plan voyage.Plan
	if err := dec.Decode(&plan); err != nil {
		return errors.New("invalid derivative plan artifact")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("derivative plan has trailing data")
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	return nil
}

func validHash(s string) bool { return hashPattern.MatchString(s) }

func (e ChangeEnvelope) Validate(now time.Time) error {
	if e.SchemaVersion != SchemaVersion || !derivativeID.MatchString(e.ID) || !validHash(e.ParentPlanHash) || e.ExpiresAt.IsZero() || !e.ExpiresAt.After(now) || e.MaxOperations == 0 || e.MaxOperations > 8 || e.MaxRetries > 8 || e.MaxSuccessors > 4 || e.MaxAssessments > 8 {
		return errors.New("invalid derivative change envelope")
	}
	if len(e.AllowedCapabilities) > 8 || len(e.AllowedEffects) > 8 || len(e.AllowedModels) > 8 || len(e.AllowedEfforts) > 8 || len(e.Templates) > 8 {
		return errors.New("derivative envelope exceeds bounds")
	}
	for _, s := range append(append(append([]string{}, e.AllowedCapabilities...), e.AllowedEffects...), append(e.AllowedModels, e.AllowedEfforts...)...) {
		if strings.TrimSpace(s) == "" || len(s) > 64 || strings.ContainsAny(s, "\r\n") {
			return errors.New("invalid derivative ceiling")
		}
	}
	seen := map[string]bool{}
	for _, t := range e.Templates {
		if !derivativeID.MatchString(t.ID) || seen[t.ID] || !t.Action.valid() || len(t.Slots) > 8 {
			return errors.New("invalid continuation template")
		}
		seen[t.ID] = true
		if t.Action == ActionSuccessor && t.Successor == nil {
			return errors.New("successor template has no frozen task")
		}
		if t.Successor != nil && (!derivativeID.MatchString(t.Successor.ID) || strings.TrimSpace(t.Successor.Persona) == "" || strings.TrimSpace(t.Successor.Summary) == "" || strings.TrimSpace(t.Successor.Prompt) == "") {
			return errors.New("invalid frozen successor task")
		}
		for name, slot := range t.Slots {
			if !derivativeID.MatchString(name) || slot.MaxBytes == 0 || slot.MaxBytes > 256 || (slot.Type != SlotString && slot.Type != SlotModel && slot.Type != SlotEffort && slot.Type != SlotUint) {
				return errors.New("invalid continuation template slot")
			}
		}
	}
	return nil
}

func (p DerivativeProposal) Validate(env ChangeEnvelope, now time.Time) error {
	if p.SchemaVersion != SchemaVersion || !derivativeID.MatchString(p.ProposalID) || p.EnvelopeID != env.ID || p.ParentPlanHash != env.ParentPlanHash || !p.Action.valid() || len(p.Operations) > int(env.MaxOperations) || len(p.Slots) > 8 {
		return errors.New("invalid derivative proposal")
	}
	if err := p.Provenance.validate(); err != nil {
		return err
	}
	if p.Provenance.VoyagePlanHash != env.ParentPlanHash {
		return errors.New("derivative provenance is not bound to envelope parent")
	}
	if err := env.Validate(now); err != nil {
		return err
	}
	if p.Action == ActionSuccessor && p.TemplateID == "" {
		return errors.New("successor proposal requires a continuation template")
	}
	if p.RetryCount > env.MaxRetries || p.SuccessorCount > env.MaxSuccessors || p.AssessmentCount > env.MaxAssessments {
		return errors.New("derivative proposal exceeds envelope budget")
	}
	if p.Action == ActionRetry && p.RetryCount == 0 || p.Action == ActionSuccessor && p.SuccessorCount == 0 || p.Action == ActionAssessment && p.AssessmentCount == 0 {
		return errors.New("derivative proposal action has no budget claim")
	}
	if p.TemplateID != "" {
		var template *ContinuationTemplate
		for i := range env.Templates {
			if env.Templates[i].ID == p.TemplateID {
				template = &env.Templates[i]
				break
			}
		}
		if template == nil || template.Action != p.Action {
			return errors.New("proposal template is not allowed")
		}
		if len(p.Slots) != len(template.Slots) {
			return errors.New("proposal template slots are incomplete")
		}
		for name, spec := range template.Slots {
			value, ok := p.Slots[name]
			if !ok || len(value) > int(spec.MaxBytes) {
				return errors.New("proposal template slot is invalid")
			}
			switch spec.Type {
			case SlotUint:
				if _, err := strconv.ParseUint(value, 10, 8); err != nil {
					return errors.New("proposal uint slot is invalid")
				}
			case SlotModel, SlotEffort:
				if strings.TrimSpace(value) == "" {
					return errors.New("proposal model/effort slot is invalid")
				}
			}
		}
	}
	if p.Action != ActionSuccessor && (p.TemplateID != "" || len(p.Slots) > 0) {
		return errors.New("template is only valid for successor")
	}
	if len(p.Evidence) > MaxEvidence {
		return errors.New("derivative evidence exceeds bound")
	}
	for _, evidence := range p.Evidence {
		if err := evidence.validate(); err != nil {
			return err
		}
	}
	for name, value := range p.Slots {
		if !derivativeID.MatchString(name) || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid proposal slot")
		}
	}
	for _, op := range p.Operations {
		if err := validateOperation(op, env); err != nil {
			return err
		}
	}
	return nil
}

func validateOperation(op PatchOperation, env ChangeEnvelope) error {
	if op.Op != "replace" || !strings.HasPrefix(op.Path, "/tasks/") || len(op.Value) == 0 || len(op.Value) > 4096 || strings.ContainsAny(op.Path, "\r\n") {
		return errors.New("unsupported or malformed derivative patch")
	}
	parts := strings.Split(op.Path, "/")
	if len(parts) != 4 || !derivativeID.MatchString(parts[2]) || (parts[3] != "models" && parts[3] != "efforts" && parts[3] != "retry_safe") {
		return errors.New("derivative patch path is not allowed")
	}
	if op.Capability == "" || op.Effect == "" {
		return errors.New("derivative patch requires capability and effect")
	}
	if !contains(env.AllowedCapabilities, op.Capability) || !contains(env.AllowedEffects, op.Effect) {
		return errors.New("derivative patch exceeds envelope ceiling")
	}
	if op.Capability == "retry.model" && op.Effect != "bounded_retry" || op.Capability == "retry.effort" && op.Effect != "bounded_retry" || op.Capability == "retry.safe" && op.Effect != "bounded_retry" {
		return errors.New("derivative capability/effect pair is invalid")
	}
	if strings.HasSuffix(op.Path, "/models") {
		if len(env.AllowedModels) == 0 {
			return errors.New("model patch has no envelope ceiling")
		}
		var values []string
		if err := json.Unmarshal(op.Value, &values); err != nil || len(values) == 0 {
			return errors.New("invalid model ceiling value")
		}
		for _, value := range values {
			if !contains(env.AllowedModels, value) {
				return errors.New("model patch exceeds envelope ceiling")
			}
		}
	}
	if strings.HasSuffix(op.Path, "/efforts") {
		if len(env.AllowedEfforts) == 0 {
			return errors.New("effort patch has no envelope ceiling")
		}
		var values []string
		if err := json.Unmarshal(op.Value, &values); err != nil || len(values) == 0 {
			return errors.New("invalid effort ceiling value")
		}
		for _, value := range values {
			if !contains(env.AllowedEfforts, value) {
				return errors.New("effort patch exceeds envelope ceiling")
			}
		}
	}
	return nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func Derive(parent *voyage.Plan, parentCanonical []byte, env ChangeEnvelope, proposal DerivativeProposal, now time.Time, previous []MachineApprovalAttestation) (DerivativeResult, error) {
	if parent == nil || !parent.Approved {
		return DerivativeResult{}, errors.New("parent voyage must be approved")
	}
	if err := parent.Validate(); err != nil {
		return DerivativeResult{}, err
	}
	if !bytes.Equal(bytes.TrimSpace(parentCanonical), mustCanonicalPlan(parent)) || voyage.Hash(parentCanonical) != env.ParentPlanHash {
		return DerivativeResult{}, errors.New("parent canonical bytes or hash mismatch")
	}
	if err := proposal.Validate(env, now); err != nil {
		return DerivativeResult{}, err
	}
	var parentTask *voyage.Task
	for i := range parent.Tasks {
		if parent.Tasks[i].ID == proposal.Provenance.TaskID {
			parentTask = &parent.Tasks[i]
			break
		}
	}
	if parentTask == nil || voyage.TaskFingerprint(*parentTask) != proposal.Provenance.TaskContractHash {
		return DerivativeResult{}, errors.New("derivative provenance task mismatch")
	}
	ops := append([]PatchOperation(nil), proposal.Operations...)
	for i := range ops {
		canonical, err := canonicalPatchJSON(ops[i].Value)
		if err != nil {
			return DerivativeResult{}, errors.New("derivative patch value is not canonical JSON")
		}
		ops[i].Value = canonical
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		return string(ops[i].Value) < string(ops[j].Value)
	})
	for i := 1; i < len(ops); i++ {
		if ops[i].Path == ops[i-1].Path {
			return DerivativeResult{}, errors.New("ambiguous duplicate derivative path")
		}
	}
	canonicalDiffBytes, _ := json.Marshal(ops)
	child := clonePlan(parent)
	if proposal.Action == ActionSuccessor {
		for _, template := range env.Templates {
			if template.ID == proposal.TemplateID && template.Successor != nil {
				for _, existing := range child.Tasks {
					if existing.ID == template.Successor.ID {
						return DerivativeResult{}, errors.New("successor task already exists")
					}
				}
				child.Tasks = append(child.Tasks, *template.Successor)
			}
		}
	}
	for _, op := range ops {
		if err := applyOperation(child, op); err != nil {
			return DerivativeResult{}, err
		}
	}
	if err := child.Validate(); err != nil {
		return DerivativeResult{}, err
	}
	invariant := invariantHash(parent)
	if invariantHashExisting(parent, child) != invariant {
		return DerivativeResult{}, errors.New("derivative changes immutable voyage invariant")
	}
	childCanonical := mustCanonicalPlan(child)
	childHash := voyage.Hash(childCanonical)
	attestationID := digestString(proposal.ParentPlanHash + "\x00" + proposal.EnvelopeID + "\x00" + proposal.ProposalID)
	att := MachineApprovalAttestation{SchemaVersion: SchemaVersion, AttestationID: attestationID, ParentPlanHash: proposal.ParentPlanHash, ChildPlanHash: childHash, EnvelopeID: env.ID, CanonicalDiff: string(canonicalDiffBytes), InvariantHash: invariant, PolicyEngineVersion: DerivativePolicyVersion, Action: proposal.Action, Provenance: proposal.Provenance, CreatedAt: now.UTC()}
	for _, old := range previous {
		if old.AttestationID == attestationID {
			if old.ChildPlanHash != att.ChildPlanHash || old.CanonicalDiff != att.CanonicalDiff || old.InvariantHash != att.InvariantHash {
				return DerivativeResult{}, errors.New("derivative replay conflict")
			}
			return DerivativeResult{Plan: child, CanonicalPlan: childCanonical, PlanHash: childHash, CanonicalDiff: string(canonicalDiffBytes), InvariantHash: invariant, Attestation: old, Replayed: true, ReusableTaskIDs: reusableTasks(parent, child), Evidence: append([]EvidenceRef(nil), proposal.Evidence...)}, nil
		}
	}
	return DerivativeResult{Plan: child, CanonicalPlan: childCanonical, PlanHash: childHash, CanonicalDiff: string(canonicalDiffBytes), InvariantHash: invariant, Attestation: att, ReusableTaskIDs: reusableTasks(parent, child), Evidence: append([]EvidenceRef(nil), proposal.Evidence...)}, nil
}

func DecodeChangeEnvelope(raw []byte) (ChangeEnvelope, error) {
	if len(raw) == 0 || len(raw) > MaxRequestBytes {
		return ChangeEnvelope{}, errors.New("change envelope exceeds bound")
	}
	if err := rejectDuplicateJSON(raw); err != nil {
		return ChangeEnvelope{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e ChangeEnvelope
	if err := dec.Decode(&e); err != nil {
		return ChangeEnvelope{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return ChangeEnvelope{}, errors.New("change envelope has trailing data")
	}
	return e, nil
}

func DecodeDerivativeProposal(raw []byte) (DerivativeProposal, error) {
	if len(raw) == 0 || len(raw) > MaxRequestBytes {
		return DerivativeProposal{}, errors.New("derivative proposal exceeds bound")
	}
	if err := rejectDuplicateJSON(raw); err != nil {
		return DerivativeProposal{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p DerivativeProposal
	if err := dec.Decode(&p); err != nil {
		return DerivativeProposal{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return DerivativeProposal{}, errors.New("derivative proposal has trailing data")
	}
	return p, nil
}

func applyOperation(plan *voyage.Plan, op PatchOperation) error {
	parts := strings.Split(op.Path, "/")
	var task *voyage.Task
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == parts[2] {
			task = &plan.Tasks[i]
			break
		}
	}
	if task == nil {
		return errors.New("derivative patch names unknown task")
	}
	switch parts[3] {
	case "models":
		var v []string
		if json.Unmarshal(op.Value, &v) != nil || len(v) == 0 || len(v) > 4 {
			return errors.New("invalid model patch")
		}
		task.Models = append([]string(nil), v...)
	case "efforts":
		var v []string
		if json.Unmarshal(op.Value, &v) != nil || len(v) == 0 || len(v) > 4 {
			return errors.New("invalid effort patch")
		}
		task.Efforts = append([]string(nil), v...)
	case "retry_safe":
		var v bool
		if json.Unmarshal(op.Value, &v) != nil {
			return errors.New("invalid retry patch")
		}
		task.RetrySafe = v
	}
	return nil
}

func reusableTasks(parent, child *voyage.Plan) []string {
	pl, pc, _ := voyage.TaskFingerprints(parent)
	cl, cc, _ := voyage.TaskFingerprints(child)
	out := []string{}
	for _, t := range parent.Tasks {
		if pl[t.ID] == cl[t.ID] && pc[t.ID] == cc[t.ID] {
			out = append(out, t.ID)
		}
	}
	sort.Strings(out)
	return out
}

func invariantHash(p *voyage.Plan) string {
	return digestBytes(canonicalJSON(struct {
		Version                                                              int
		Title, Objective                                                     string
		Scope, NonGoals, BlastArea, Risks, AcceptanceCriteria, OpenDecisions []string
		Approved                                                             bool
		Tasks                                                                []struct {
			ID, Persona, Summary string
			DependsOn            []string
		}
	}{p.Version, p.Title, p.Objective, p.Scope, p.NonGoals, p.BlastArea, p.Risks, p.AcceptanceCriteria, p.OpenDecisions, p.Approved, invariantTasks(p)}))
}

func invariantHashExisting(parent, child *voyage.Plan) string {
	ids := make(map[string]bool, len(parent.Tasks))
	for _, task := range parent.Tasks {
		ids[task.ID] = true
	}
	projection := *child
	projection.Tasks = make([]voyage.Task, 0, len(parent.Tasks))
	for _, task := range child.Tasks {
		if ids[task.ID] {
			projection.Tasks = append(projection.Tasks, task)
		}
	}
	return invariantHash(&projection)
}

func invariantTasks(p *voyage.Plan) []struct {
	ID, Persona, Summary string
	DependsOn            []string
} {
	out := make([]struct {
		ID, Persona, Summary string
		DependsOn            []string
	}, len(p.Tasks))
	for i, t := range p.Tasks {
		out[i] = struct {
			ID, Persona, Summary string
			DependsOn            []string
		}{t.ID, t.Persona, t.Summary, append([]string(nil), t.DependsOn...)}
	}
	return out
}

func clonePlan(p *voyage.Plan) *voyage.Plan {
	var out voyage.Plan
	_ = json.Unmarshal(mustCanonicalPlan(p), &out)
	return &out
}

// rejectDuplicateJSON walks every object before the strict schema decoder.
// encoding/json otherwise silently keeps the last duplicate key.
func rejectDuplicateJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if _, err := scanJSONValue(dec); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("JSON has trailing data")
	}
	return nil
}

func scanJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			seen := map[string]bool{}
			out := map[string]any{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return nil, errors.New("duplicate or invalid JSON object key")
				}
				seen[name] = true
				value, err := scanJSONValue(dec)
				if err != nil {
					return nil, err
				}
				out[name] = value
			}
			_, err := dec.Token()
			return out, err
		case '[':
			out := []any{}
			for dec.More() {
				value, err := scanJSONValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			_, err := dec.Token()
			return out, err
		default:
			return nil, errors.New("invalid JSON delimiter")
		}
	default:
		return v, nil
	}
}

func canonicalPatchJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := scanJSONValue(dec)
	if err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("JSON has trailing data")
	}
	if containsJSONNumber(value) {
		return nil, errors.New("numeric patch values are not allowed")
	}
	return canonicalJSON(value), nil
}

func containsJSONNumber(value any) bool {
	switch v := value.(type) {
	case json.Number:
		return true
	case []any:
		for _, item := range v {
			if containsJSONNumber(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if containsJSONNumber(item) {
				return true
			}
		}
	}
	return false
}

func mustCanonicalPlan(p *voyage.Plan) []byte { return canonicalJSON(p) }
func canonicalJSON(v any) []byte              { b, _ := json.Marshal(v); return b }
func digestBytes(b []byte) string             { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func digestString(s string) string            { return digestBytes([]byte(s)) }
