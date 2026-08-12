package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompletionIntegrationHumanBoundariesAndAppendOnlyEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := provenance()
	base := request()
	base.Reason = ReasonUncertainty
	fp, err := Fingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	// False needs-input remains agent-actionable and is safely continued.
	if response, changed, err := Assess(base, ""); err != nil || !changed || response.Decision != DecisionContinue {
		t.Fatalf("false needs-input assessment=%+v changed=%v err=%v", response, changed, err)
	}
	// Independently supplied external/CI evidence is append-only and bounded.
	header := RecordHeader{SchemaVersion: SchemaVersion, Kind: RecordAttestation, RecordID: "ci-pass", CreatedAt: time.Now().UTC(), Provenance: plan, Fingerprint: fp}
	if err := j.Append(AttestationRecord{RecordHeader: header, Attester: "ci", Evidence: []EvidenceRef{evidence()}}); err != nil {
		t.Fatal(err)
	}
	proofs := []struct {
		name   string
		reason ReasonCode
		kind   ProofKind
		want   Decision
	}{
		{"physical-access", ReasonPhysicalAccessUnavailable, ProofPhysicalAccess, DecisionStop},
		{"human-credential", ReasonHumanCredentialRequired, ProofHumanCredential, DecisionStop},
		{"captain-decision", ReasonCaptainDecisionRequired, ProofCaptainDecision, DecisionAmendmentRequired},
	}
	for _, tc := range proofs {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			r.Reason = tc.reason
			r.HumanOnly = &HumanOnlyProof{Kind: tc.kind, Verified: true, ObservedAt: time.Now().UTC(), Evidence: []EvidenceRef{evidence()}}
			response, _, err := Assess(r, "")
			if err != nil || response.Decision != tc.want {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if tc.want == DecisionAmendmentRequired && response.Decision == DecisionAmendmentRequired && strings.Contains(strings.ToLower(response.ActionHint), "approve") {
				t.Fatal("amendment advisory smuggled approval authority")
			}
		})
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(AttestationRecord{RecordHeader: RecordHeader{SchemaVersion: SchemaVersion, Kind: RecordAttestation, RecordID: "ci-pass-2", CreatedAt: time.Now().UTC(), Provenance: plan, Fingerprint: fp}, Attester: "ci", Evidence: []EvidenceRef{evidence()}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || strings.Count(string(after), "\n") != 2 {
		t.Fatalf("journal was not append-only: before=%d after=%d", len(before), len(after))
	}
}
