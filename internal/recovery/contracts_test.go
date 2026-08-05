package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func provenance() Provenance {
	return Provenance{VoyagePlanHash: strings.Repeat("a", 64), TaskID: "task-one", TaskContractHash: strings.Repeat("b", 64), StateHash: strings.Repeat("c", 64)}
}

func evidence() EvidenceRef {
	return EvidenceRef{Source: "test", DetailCode: "bounded_failure", Digest: strings.Repeat("d", 64)}
}

func request() RequestV1 {
	return RequestV1{SchemaVersion: SchemaVersion, Provenance: provenance(), Reason: ReasonManagedEnvironmentMismatch, Attempt: 1, TierCount: 2, Evidence: []EvidenceRef{evidence()}}
}

func TestClassificationDefaultsToAgentActionable(t *testing.T) {
	got, err := Classify(request())
	if err != nil || got.HumanOnly || got.Reason != ReasonManagedEnvironmentMismatch {
		t.Fatalf("classification=%+v err=%v", got, err)
	}
	resp, changed, err := Assess(request(), "")
	if err != nil || !changed || resp.Decision != DecisionContinue || resp.Reason != ReasonManagedEnvironmentMismatch {
		t.Fatalf("assessment=%+v changed=%v err=%v", resp, changed, err)
	}
}

func TestHumanOnlyRequiresStructuredEvidence(t *testing.T) {
	r := request()
	r.Reason = ReasonPhysicalAccessUnavailable
	r.HumanOnly = &HumanOnlyProof{Kind: ProofPhysicalAccess, Verified: true, ObservedAt: time.Now().UTC(), Evidence: []EvidenceRef{evidence()}}
	got, err := Classify(r)
	if err != nil || !got.HumanOnly || got.Reason != ReasonPhysicalAccessUnavailable {
		t.Fatalf("classification=%+v err=%v", got, err)
	}
	resp, _, err := Assess(r, "")
	if err != nil || resp.Decision != DecisionStop {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	r.HumanOnly.Verified = false
	if _, err := Classify(r); err == nil {
		t.Fatal("unverified human-only claim accepted")
	}
}

func TestCaptainDecisionIsAmendmentOnlyAndFingerprintIsRedacted(t *testing.T) {
	r := request()
	r.Reason = ReasonCaptainDecisionRequired
	r.HumanOnly = &HumanOnlyProof{Kind: ProofCaptainDecision, Verified: true, ObservedAt: time.Now().UTC(), Evidence: []EvidenceRef{evidence()}}
	resp, _, err := Assess(r, "")
	if err != nil || resp.Decision != DecisionAmendmentRequired {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if strings.Contains(string(mustJSON(resp)), "approve") || strings.Contains(string(mustJSON(resp)), "complete") || strings.Contains(string(mustJSON(resp)), "prompt") {
		t.Fatalf("authority-forbidden response fields: %s", mustJSON(resp))
	}
	fp, err := Fingerprint(r)
	if err != nil || len(fp) != 64 || strings.Contains(fp, "secret") {
		t.Fatalf("fingerprint=%q err=%v", fp, err)
	}
	r.Evidence[0].DetailCode = "different_failure"
	fp2, _ := Fingerprint(r)
	if fp == fp2 {
		t.Fatal("materially changed evidence did not change fingerprint")
	}
	r.HumanOnly.Evidence[0].DetailCode = "new_proof"
	if fp3, _ := Fingerprint(r); fp2 == fp3 {
		t.Fatal("changed human-only evidence did not change fingerprint")
	}
}

func TestResponseCannotPromoteRoutineFailure(t *testing.T) {
	req := request()
	fp, _ := Fingerprint(req)
	resp := ResponseV1{SchemaVersion: SchemaVersion, Decision: DecisionStop, Reason: req.Reason, Fingerprint: fp}
	if err := resp.ValidateFor(req); err == nil {
		t.Fatal("routine failure was promoted to human-only stop")
	}
}

func TestDecodeIsClosedAndBounded(t *testing.T) {
	b, _ := json.Marshal(request())
	if _, err := DecodeRequest(append(b, []byte("{}")...)); err == nil {
		t.Fatal("trailing request accepted")
	}
	unknown := strings.Replace(string(b), `"reason":"managed_environment_mismatch"`, `"reason":"managed_environment_mismatch","authority":"captain"`, 1)
	if _, err := DecodeRequest([]byte(unknown)); err == nil {
		t.Fatal("authority-forbidden field accepted")
	}
	if _, err := DecodeRequest(make([]byte, MaxRequestBytes+1)); err == nil {
		t.Fatal("oversized request accepted")
	}
}

func TestResponseRejectsAuthorityActionsAndConfigBounds(t *testing.T) {
	fp, _ := Fingerprint(request())
	if err := (ResponseV1{SchemaVersion: SchemaVersion, Decision: DecisionContinue, Reason: ReasonOrdinaryFailure, Fingerprint: fp, ActionHint: "approve the task"}).Validate(); err == nil {
		t.Fatal("authority action hint accepted")
	}
	c := Config{AutoCaptain: true, ContinuationEnvelope: &ContinuationEnvelope{Enabled: true, MaxAttempts: 9, MaxAssessments: 1, ExpiresAt: time.Now().Add(time.Hour)}}
	if err := c.Validate(); err == nil {
		t.Fatal("unbounded continuation envelope accepted")
	}
}

func TestJournalIsAppendOnlyBoundedAndRestoresSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := Fingerprint(request())
	h := RecordHeader{SchemaVersion: SchemaVersion, Kind: RecordBlocker, RecordID: "record-1", CreatedAt: time.Now().UTC(), Provenance: provenance(), Fingerprint: fp}
	r := BlockerRecord{RecordHeader: h, Reason: request().Reason, Evidence: []EvidenceRef{evidence()}}
	if err := j.Append(r); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(r); err == nil {
		t.Fatal("duplicate record accepted")
	}
	j2, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	h.RecordID = "record-2"
	if err := j2.Append(BlockerRecord{RecordHeader: h, Reason: request().Reason, Evidence: []EvidenceRef{evidence()}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "\n") != 2 || !strings.Contains(string(b), `"sequence":2`) {
		t.Fatalf("journal=%s", b)
	}
}

func TestJournalRejectsMalformedAndSymlinkRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte(`{"kind":"blocker","record_id":"x","sequence":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(path); err == nil {
		t.Fatal("malformed journal accepted")
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(link); err == nil {
		t.Fatal("symlink journal accepted")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
