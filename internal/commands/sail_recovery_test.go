package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
)

func TestPrepareCodexTransportHomeCopiesOnlyBoundedAuthentication(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CODEX_HOME", source)
	want := []byte(`{"tokens":{"access_token":"test-only"}}`)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	isolated, cleanup, err := prepareCodexTransportHome()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := os.ReadFile(filepath.Join(isolated, "auth.json"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("isolated auth = %q, %v", got, err)
	}
	entries, err := os.ReadDir(isolated)
	if err != nil || len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("isolated transport files = %v, %v", entries, err)
	}
}

func TestPrepareCodexTransportHomeRejectsSymlinkedAuthentication(t *testing.T) {
	source := t.TempDir()
	t.Setenv("CODEX_HOME", source)
	target := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(target, []byte(`{"token":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareCodexTransportHome(); err == nil {
		t.Fatal("symlinked authentication was accepted")
	}
}

func TestRealAutoCaptainDispatch(t *testing.T) {
	if os.Getenv("SHIPMATES_REAL_AUTOCAPTAIN_TEST") != "1" {
		t.Skip("set SHIPMATES_REAL_AUTOCAPTAIN_TEST=1 inside a delegated Linux scope")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := pinnedSkipperSnapshotAt(repo, "skipper")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	agents := filepath.Join(root, ".codex", "agents")
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "skipper.toml"), snapshot.Raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{filepath.Join(".shipmates", "policy.yaml"), filepath.Join(".shipmates", "policies", "skipper.yaml")} {
		body, readErr := os.ReadFile(filepath.Join(repo, relative))
		if readErr != nil {
			t.Fatal(readErr)
		}
		destination := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var output bytes.Buffer
	err = dispatchPinnedSkipperAt(ctx, root, snapshot, `{"schema_version":1,"reason":"uncertainty","evidence":[]}`, project.PersonaConfig{Model: "gpt-5.6-sol", Effort: "low"}, &output, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) == "" {
		t.Fatal("Auto-Captain returned no advisory")
	}
}

func TestSailRecoveryDispatchIsStableAcrossRetryAttempts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(_ context.Context, _ *skipperSnapshot, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		calls.Add(1)
		var req recovery.RequestV1
		if err := json.Unmarshal([]byte(prompt), &req); err != nil {
			return err
		}
		fp, err := recovery.Fingerprint(req)
		if err != nil {
			return err
		}
		_, err = io.WriteString(stdout, string(mustJSON(recovery.ResponseV1{SchemaVersion: recovery.SchemaVersion, Decision: recovery.DecisionContinue, Reason: req.Reason, Fingerprint: fp})))
		return err
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("base"), nil
	}}
	task := voyage.Task{ID: "task-one", Persona: "crew", RetrySafe: true, Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{Status: voyage.Failed, Attempt: 0, TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	first, advised, err := r.advise(context.Background(), plan, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", task, entry, recovery.ReasonExhaustedTiers, "same bounded failure")
	if err != nil || !advised || first.Decision != recovery.DecisionContinue {
		t.Fatalf("first advice: %#v %v %v", first, advised, err)
	}
	entry.Attempt = 1
	second, advised, err := r.advise(context.Background(), plan, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", task, entry, recovery.ReasonExhaustedTiers, "same bounded failure")
	if err != nil || advised || second.Decision != "" {
		t.Fatalf("repeated advice: %#v %v %v", second, advised, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("Sol calls = %d, want 1", calls.Load())
	}
}

func TestValidateSolBindingRejectsRequestDigestSubstitution(t *testing.T) {
	request := recovery.RequestV1{
		SchemaVersion: recovery.SchemaVersion,
		Provenance:    recovery.Provenance{VoyagePlanHash: strings.Repeat("a", 64), TaskID: "task-one", TaskContractHash: strings.Repeat("b", 64), StateHash: strings.Repeat("c", 64)},
		Reason:        recovery.ReasonUncertainty, TierCount: 1,
		Evidence: []recovery.EvidenceRef{{Source: "sail", DetailCode: "uncertainty", Digest: strings.Repeat("d", 64)}},
	}
	snapshot := testSnapshot("binding")
	fingerprint, err := recovery.Fingerprint(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	binding := solAdvisoryBinding{RequestDigest: digestBytes(raw), BlockerFingerprint: fingerprint, SkipperDigest: snapshot.Digest, Model: "gpt-5.6-sol", ResponseSchemaDigest: digest(solResponseSchema), Deadline: time.Now().Add(time.Minute)}
	binding.RequestDigest = strings.Repeat("e", 64)
	if err := validateSolBinding(request, snapshot, binding); err == nil {
		t.Fatal("accepted substituted request digest")
	}
}

func TestSailRecoveryJournalDoesNotPersistFailureDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(_ context.Context, _ *skipperSnapshot, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		var req recovery.RequestV1
		if err := json.Unmarshal([]byte(prompt), &req); err != nil {
			return err
		}
		fp, _ := recovery.Fingerprint(req)
		_, err := io.WriteString(stdout, string(mustJSON(recovery.ResponseV1{SchemaVersion: recovery.SchemaVersion, Decision: recovery.DecisionContinue, Reason: req.Reason, Fingerprint: fp})))
		return err
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("base"), nil
	}}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{{ID: "task-one", Persona: "crew", Summary: "work", Prompt: "work"}}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	secret := "SECRET_TOKEN /private/path"
	if _, _, err := r.advise(context.Background(), plan, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", plan.Tasks[0], entry, recovery.ReasonUncertainty, secret); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("raw failure detail persisted")
	}
}

func TestSailRecoveryAssessmentPersistsPinnedSkipperProvenance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(_ context.Context, _ *skipperSnapshot, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		var req recovery.RequestV1
		if err := json.Unmarshal([]byte(prompt), &req); err != nil {
			return err
		}
		fp, _ := recovery.Fingerprint(req)
		_, err := io.WriteString(stdout, string(mustJSON(recovery.ResponseV1{SchemaVersion: recovery.SchemaVersion, Decision: recovery.DecisionContinue, Reason: req.Reason, Fingerprint: fp})))
		return err
	}
	digest := project.SHA([]byte("immutable"))
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("immutable"), nil
	}}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "hostile evidence"); err != nil || !ok {
		t.Fatalf("advise: %v %v", err, ok)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, digest) || !strings.Contains(text, `"persona_version":1`) || !strings.Contains(text, `"effective_model":"gpt-5.6-sol"`) {
		t.Fatalf("missing pinned provenance: %s", text)
	}
}

func TestSailRecoveryFakeSolUsesSkipperOverrideAndCompletesAfterRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	var calls atomic.Int32
	sailRecoveryDispatcher = func(_ context.Context, snapshot *skipperSnapshot, prompt string, cfg project.PersonaConfig, stdout, _ io.Writer) error {
		calls.Add(1)
		if snapshot.Name != "skipper" || snapshot.Digest != project.SHA([]byte("immutable")) || cfg.Model != "gpt-5.6-sol" {
			t.Fatalf("dispatch lost immutable Skipper/model contract: snapshot=%+v model=%q", snapshot, cfg.Model)
		}
		var req recovery.RequestV1
		if err := json.Unmarshal([]byte(prompt), &req); err != nil {
			return err
		}
		fp, err := recovery.Fingerprint(req)
		if err != nil {
			return err
		}
		_, err = io.WriteString(stdout, string(mustJSON(recovery.ResponseV1{SchemaVersion: recovery.SchemaVersion, Decision: recovery.DecisionContinue, Reason: req.Reason, Fingerprint: fp})))
		return err
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("immutable"), nil
	}}
	task := voyage.Task{ID: "recoverable", Persona: "crew", RetrySafe: true, Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{Status: voyage.Failed, TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	response, advised, err := r.advise(context.Background(), plan, strings.Repeat("b", 64), task, entry, recovery.ReasonExhaustedTiers, "ordinary tier exhausted")
	if err != nil || !advised || response.Decision != recovery.DecisionContinue {
		t.Fatalf("recovery advice=%+v advised=%v err=%v", response, advised, err)
	}
	// Sail remains authoritative: a continue assessment permits the next
	// normal task attempt, whose fake-Codex result is successful.
	entry.Status, entry.Summary = voyage.Completed, "fake Codex completed after bounded recovery"
	if entry.Status != voyage.Completed || calls.Load() != 1 {
		t.Fatalf("completion=%+v Sol calls=%d", entry, calls.Load())
	}
	// A restarted Sail process sees the assessment and does not call Sol again.
	restarted, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	r.journal = restarted
	if _, advised, err := r.advise(context.Background(), plan, strings.Repeat("b", 64), task, entry, recovery.ReasonExhaustedTiers, "ordinary tier exhausted"); err != nil || advised {
		t.Fatalf("restart dedup advised=%v err=%v", advised, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("restart duplicated Sol call: %d", calls.Load())
	}
}

func TestSailRecoveryRejectsMalformedAdvisory(t *testing.T) {
	j, err := recovery.OpenJournal(filepath.Join(t.TempDir(), "recovery.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(context.Context, *skipperSnapshot, string, project.PersonaConfig, io.Writer, io.Writer) error {
		return io.ErrUnexpectedEOF
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("x"), nil
	}}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "x"); err == nil || ok {
		t.Fatalf("malformed advisory accepted: %v %v", err, ok)
	}
}

func TestSailRecoveryTimeoutFallsBackWithoutAssessment(t *testing.T) {
	j, err := recovery.OpenJournal(filepath.Join(t.TempDir(), "recovery.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(ctx context.Context, _ *skipperSnapshot, _ string, _ project.PersonaConfig, _ io.Writer, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	r := &sailRecovery{
		journal: j, skipper: "skipper",
		cfg:               project.Config{Recovery: recovery.Config{AutoCaptain: true}},
		assessmentTimeout: 20 * time.Millisecond,
		capture: func() (*skipperSnapshot, error) {
			return testSnapshot("immutable"), nil
		},
	}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	started := time.Now()
	response, advised, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "needs Captain input")
	if err != nil || advised || response.Decision != "" {
		t.Fatalf("timeout fallback: response=%+v advised=%v err=%v", response, advised, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("timed-out advisory did not return promptly")
	}
	if j.Count(recovery.RecordBlocker) != 1 || j.Count(recovery.RecordAssessment) != 0 || j.Count(recovery.RecordAdvisoryOutcome) != 1 {
		t.Fatalf("journal blocker=%d assessment=%d outcome=%d", j.Count(recovery.RecordBlocker), j.Count(recovery.RecordAssessment), j.Count(recovery.RecordAdvisoryOutcome))
	}
}

func TestSailRecoveryTimeoutConsumesFingerprintAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	var calls atomic.Int32
	sailRecoveryDispatcher = func(ctx context.Context, _ *skipperSnapshot, _ string, _ project.PersonaConfig, _ io.Writer, _ io.Writer) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, assessmentTimeout: 10 * time.Millisecond, capture: func() (*skipperSnapshot, error) { return testSnapshot("restart-safe"), nil }}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "same"); err != nil || ok {
		t.Fatalf("first timeout ok=%v err=%v", ok, err)
	}
	reopened, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	r.journal = reopened
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "same"); err != nil || ok {
		t.Fatalf("restart replay ok=%v err=%v", ok, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("timeout redispatched after restart: %d", calls.Load())
	}
}

func TestSailRecoveryInvalidResponsePersistsRedactedOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(context.Context, *skipperSnapshot, string, project.PersonaConfig, io.Writer, io.Writer) error {
		return nil
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) { return testSnapshot("invalid-response"), nil }}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "hostile SECRET"); err == nil || ok {
		t.Fatalf("invalid response accepted: ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hostile SECRET") || strings.Contains(string(data), "SECRET") {
		t.Fatal("raw advisory input persisted")
	}
	if j.Count(recovery.RecordAdvisoryOutcome) != 1 {
		t.Fatal("invalid response outcome missing")
	}
}

func TestSailRecoveryManagedChildFailureFallsBackWithoutAssessment(t *testing.T) {
	j, err := recovery.OpenJournal(filepath.Join(t.TempDir(), "recovery.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(context.Context, *skipperSnapshot, string, project.PersonaConfig, io.Writer, io.Writer) error {
		return &managedSessionFailure{state: "failed", code: "transport_closed"}
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		return testSnapshot("immutable"), nil
	}}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	response, advised, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "needs Captain input")
	if err != nil || advised || response.Decision != "" {
		t.Fatalf("managed failure fallback: response=%+v advised=%v err=%v", response, advised, err)
	}
	if j.Count(recovery.RecordBlocker) != 1 || j.Count(recovery.RecordAssessment) != 0 {
		t.Fatalf("journal blocker=%d assessment=%d", j.Count(recovery.RecordBlocker), j.Count(recovery.RecordAssessment))
	}
}

func TestSailRecoveryRejectsChangedPinnedArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true}}, capture: func() (*skipperSnapshot, error) {
		s := testSnapshot("immutable")
		s.Raw = []byte("changed")
		return s, nil
	}}
	task := voyage.Task{ID: "task-one", Persona: "crew", Models: []string{"m"}, Efforts: []string{"low"}}
	entry := voyage.TaskState{TaskFingerprint: voyage.Hash([]byte("contract"))}
	plan := &voyage.Plan{Version: 1, Approved: true, Tasks: []voyage.Task{task}}
	if _, ok, err := r.advise(context.Background(), plan, strings.Repeat("a", 64), task, entry, recovery.ReasonUncertainty, "detail"); err == nil || ok {
		t.Fatalf("changed snapshot accepted: %v %v", err, ok)
	}
	if j.Count(recovery.RecordAssessment) != 0 {
		t.Fatal("changed snapshot produced assessment")
	}
}

func TestSailRecoveryMachineApprovesBoundedDerivative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.jsonl")
	j, err := recovery.OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{Version: 1, Title: "approved", Objective: "bounded", Approved: true, Tasks: []voyage.Task{{ID: "task-one", Persona: "crew", Summary: "work", Prompt: "work", Models: []string{"m"}, Efforts: []string{"low"}, RetrySafe: true}}}
	canonical, _ := json.Marshal(plan)
	parentHash := voyage.Hash(canonical)
	env := recovery.ChangeEnvelope{SchemaVersion: recovery.SchemaVersion, ID: "env-one", ParentPlanHash: parentHash, ExpiresAt: time.Now().Add(time.Hour), MaxOperations: 1, MaxRetries: 1, AllowedCapabilities: []string{"retry.model"}, AllowedEffects: []string{"bounded_retry"}, AllowedModels: []string{"m2"}}
	old := sailRecoveryDispatcher
	defer func() { sailRecoveryDispatcher = old }()
	sailRecoveryDispatcher = func(_ context.Context, _ *skipperSnapshot, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		var req recovery.RequestV1
		if err := json.Unmarshal([]byte(prompt), &req); err != nil {
			return err
		}
		fp, _ := recovery.Fingerprint(req)
		proposal := recovery.DerivativeProposal{SchemaVersion: recovery.SchemaVersion, ProposalID: "proposal-one", EnvelopeID: env.ID, ParentPlanHash: parentHash, Action: recovery.ActionRetry, RetryCount: 1, Operations: []recovery.PatchOperation{{Op: "replace", Path: "/tasks/task-one/models", Value: json.RawMessage(`["m2"]`), Capability: "retry.model", Effect: "bounded_retry"}}, Provenance: req.Provenance}
		out := recovery.ResponseV1{SchemaVersion: recovery.SchemaVersion, Decision: recovery.DecisionContinue, Reason: req.Reason, Fingerprint: fp, Derivative: &proposal}
		_, err := io.WriteString(stdout, string(mustJSON(out)))
		return err
	}
	r := &sailRecovery{journal: j, skipper: "skipper", cfg: project.Config{Recovery: recovery.Config{AutoCaptain: true, DerivativeEnvelope: &env}}, capture: func() (*skipperSnapshot, error) { return testSnapshot("immutable"), nil }}
	task := plan.Tasks[0]
	entry := voyage.TaskState{Status: voyage.Failed, TaskFingerprint: voyage.TaskFingerprint(task)}
	response, ok, err := r.advise(context.Background(), plan, parentHash, task, entry, recovery.ReasonExhaustedTiers, "host evidence is available")
	if err != nil || !ok || response.Derivative == nil {
		t.Fatalf("derivative advice: %+v %v %v", response, ok, err)
	}
	result := r.takeDerivative(response.Fingerprint)
	if result == nil || result.Plan.Tasks[0].Models[0] != "m2" || result.PlanHash == parentHash {
		t.Fatalf("derivative was not activated: %+v", result)
	}
}

func testSnapshot(instructions string) *skipperSnapshot {
	raw := []byte(instructions)
	return &skipperSnapshot{Name: "skipper", Instructions: instructions, Raw: raw, Digest: project.SHA(raw), Version: 1}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
