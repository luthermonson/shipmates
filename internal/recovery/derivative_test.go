package recovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/voyage"
)

func derivativeFixture(t *testing.T) (*voyage.Plan, []byte, ChangeEnvelope, DerivativeProposal, time.Time) {
	t.Helper()
	plan := &voyage.Plan{Version: 1, Title: "commissioned", Objective: "bounded work", Scope: []string{"local"}, AcceptanceCriteria: []string{"pass"}, Commissioned: true, Tasks: []voyage.Task{{ID: "task-one", Persona: "backend", Summary: "inspect", Prompt: "inspect files", Models: []string{"gpt-5.6-luna"}, Efforts: []string{"low"}, RetrySafe: true}}}
	canonical, _ := json.Marshal(plan)
	now := time.Now().UTC()
	env := ChangeEnvelope{SchemaVersion: SchemaVersion, ID: "env-one", ParentPlanHash: voyage.Hash(canonical), ExpiresAt: now.Add(time.Hour), MaxOperations: 2, MaxRetries: 2, MaxSuccessors: 1, MaxAssessments: 1, AllowedCapabilities: []string{"retry.model"}, AllowedEffects: []string{"bounded_retry"}, AllowedModels: []string{"gpt-5.6-terra"}}
	proposal := DerivativeProposal{SchemaVersion: SchemaVersion, ProposalID: "proposal-one", EnvelopeID: env.ID, ParentPlanHash: env.ParentPlanHash, Action: ActionRetry, RetryCount: 1, Operations: []PatchOperation{{Op: "replace", Path: "/tasks/task-one/models", Value: json.RawMessage(`["gpt-5.6-terra"]`), Capability: "retry.model", Effect: "bounded_retry"}}, Provenance: Provenance{VoyagePlanHash: env.ParentPlanHash, TaskID: "task-one", TaskContractHash: voyage.TaskFingerprint(plan.Tasks[0]), StateHash: strings.Repeat("a", 64)}}
	return plan, canonical, env, proposal, now
}

func TestDeriveAllowsBoundedModelRetryAndReusesOnlyEquivalentClosure(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	result, err := Derive(parent, canonical, env, proposal, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.PlanHash == env.ParentPlanHash || result.Attestation.ChildPlanHash != result.PlanHash || result.InvariantHash == "" {
		t.Fatalf("invalid derivative result: %+v", result)
	}
	if len(result.ReusableTaskIDs) != 0 {
		t.Fatalf("changed task was reusable: %v", result.ReusableTaskIDs)
	}
	if result.Attestation.PolicyEngineVersion != DerivativePolicyVersion || result.Attestation.CanonicalDiff == "" {
		t.Fatalf("missing attestation: %+v", result.Attestation)
	}
}

func TestDerivativeRejectsSemanticAndAuthorityChanges(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	for _, path := range []string{"/tasks/task-one/prompt", "/title", "/approved", "/tasks/task-one/unknown"} {
		p := proposal
		p.Operations = []PatchOperation{{Op: "replace", Path: path, Value: json.RawMessage(`"bad"`), Capability: "retry.model", Effect: "bounded_retry"}}
		if _, err := Derive(parent, canonical, env, p, now, nil); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
	dup := proposal
	dup.Operations = append(append([]PatchOperation(nil), proposal.Operations...), proposal.Operations[0])
	if _, err := Derive(parent, canonical, env, dup, now, nil); err == nil {
		t.Error("duplicate patch was accepted")
	}
}

func TestDerivativeRejectsExpiryBudgetsCeilingsAndReplayConflict(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	if _, err := Derive(parent, canonical, env, proposal, now.Add(2*time.Hour), nil); err == nil {
		t.Error("expired envelope was accepted")
	}
	p := proposal
	p.RetryCount = 3
	if _, err := Derive(parent, canonical, env, p, now, nil); err == nil {
		t.Error("retry budget was exceeded")
	}
	p = proposal
	p.Operations = append([]PatchOperation(nil), proposal.Operations...)
	p.Operations[0].Capability = "authority.expand"
	if _, err := Derive(parent, canonical, env, p, now, nil); err == nil {
		t.Error("capability ceiling was exceeded")
	}
	first, err := Derive(parent, canonical, env, proposal, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := Derive(parent, canonical, env, proposal, now.Add(time.Minute), []MachineApprovalAttestation{first.Attestation})
	if err != nil || !replayed.Replayed {
		t.Fatalf("idempotent replay failed: %v %+v", err, replayed)
	}
	conflict := first.Attestation
	conflict.ChildPlanHash = strings.Repeat("f", 64)
	if _, err := Derive(parent, canonical, env, proposal, now, []MachineApprovalAttestation{conflict}); err == nil {
		t.Error("replay conflict was accepted")
	}
}

func TestDerivativeStrictDecodersRejectUnknownAndTrailingFields(t *testing.T) {
	if _, err := DecodeDerivativeProposal([]byte(`{"schema_version":1,"proposal_id":"p","extra":true}`)); err == nil {
		t.Error("unknown proposal field accepted")
	}
	if _, err := DecodeChangeEnvelope([]byte(`{"schema_version":1} {}`)); err == nil {
		t.Error("trailing envelope accepted")
	}
	if _, err := DecodeDerivativeProposal([]byte(`{"schema_version":1,"schema_version":1}`)); err == nil {
		t.Error("duplicate proposal key accepted")
	}
}

func TestDerivativeBindsProvenanceAndRejectsCeilingBypass(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	proposal.Provenance.VoyagePlanHash = strings.Repeat("b", 64)
	if _, err := Derive(parent, canonical, env, proposal, now, nil); err == nil {
		t.Fatal("unrelated provenance accepted")
	}
	proposal = func() DerivativeProposal { _, _, _, p, _ := derivativeFixture(t); return p }()
	proposal.Operations[0].Value = json.RawMessage(`["gpt-5.6-sol"]`)
	if _, err := Derive(parent, canonical, env, proposal, now, nil); err == nil {
		t.Fatal("model outside envelope accepted")
	}
}

func TestDerivativeCanonicalizesUnicodeAndRejectsNumbers(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	proposal.Operations[0].Value = json.RawMessage(`["gpt-5.6-terra"]`)
	if _, err := Derive(parent, canonical, env, proposal, now, nil); err != nil {
		t.Fatal(err)
	}
	proposal.Operations[0].Value = json.RawMessage(`[1]`)
	if _, err := Derive(parent, canonical, env, proposal, now, nil); err == nil {
		t.Fatal("numeric patch accepted")
	}
}

func TestDerivativeFrozenSuccessorCreatesOnlyTemplateTask(t *testing.T) {
	parent, canonical, env, proposal, now := derivativeFixture(t)
	env.MaxOperations = 1
	env.MaxSuccessors = 1
	env.Templates = []ContinuationTemplate{{ID: "host-evidence", Action: ActionSuccessor, Successor: &voyage.Task{ID: "verify-host", Persona: "tester", Summary: "verify host evidence", Prompt: "use existing host evidence", DependsOn: []string{"task-one"}}}}
	proposal.Action = ActionSuccessor
	proposal.RetryCount = 0
	proposal.SuccessorCount = 1
	proposal.TemplateID = "host-evidence"
	proposal.Operations = nil
	result, err := Derive(parent, canonical, env, proposal, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Tasks) != 2 || result.Plan.Tasks[1].ID != "verify-host" {
		t.Fatalf("frozen successor missing: %+v", result.Plan.Tasks)
	}
	if len(result.ReusableTaskIDs) != 1 || result.ReusableTaskIDs[0] != "task-one" {
		t.Fatalf("parent completion equivalence lost: %v", result.ReusableTaskIDs)
	}
}
