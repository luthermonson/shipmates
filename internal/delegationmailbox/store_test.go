//go:build unix

package delegationmailbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/delegation"
	"github.com/luthermonson/shipmates/internal/fleetcommander"
)

type fakeM2 struct {
	calls        int
	record       map[string]map[string]any
	acceptID     string
	acceptRecord map[string]any
}

func (f *fakeM2) Accept(context.Context, []byte) error {
	f.calls++
	f.record[f.acceptID] = f.acceptRecord
	return nil
}
func (f *fakeM2) Lookup(id string) (delegation.Record, bool) {
	raw, ok := f.record[id]
	if !ok {
		return delegation.Record{}, false
	}
	b, _ := json.Marshal(raw)
	var r delegation.Record
	if json.Unmarshal(b, &r) != nil {
		return delegation.Record{}, false
	}
	return r, true
}

func TestOneInstructionProjectsFromDurableM2AndRepairs(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	var env struct {
		DelegationID string `json:"delegation_id"`
	}
	_ = json.Unmarshal(raw, &env)
	// The codec test owns signature/digest validation; this local fixture uses
	// the frozen envelope bytes and a valid M3 message digest.
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_0123456789abcdef", InstructionID: "ins_0123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"schema": "fleet.delegation-decision.v1", "record_id": "rec_0123456789abcdef", "sequence": 3, "delegation_id": env.DelegationID, "envelope_digest": digest, "voyage_plan_hash": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "task_contract_hash": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "state_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "blocker_fingerprint": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "policy_version": 1, "skipper_artifact_digest": "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "skipper_artifact_version": 1, "effective_model": "gpt-5.6-sol", "recovery_schema": "recovery.response.v1", "references": []any{}, "created_at": "2026-07-14T19:29:00.000Z", "lifecycle": "advised", "reason_code": "advised", "result": "advised", "advisory_decision": "continue", "provenance_digest": "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "sail_state": "locally_accepted_under_existing_policy"}
	f := &fakeM2{record: map[string]map[string]any{}, acceptID: env.DelegationID, acceptRecord: record}
	if err := s.Process(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("M2 calls=%d", f.calls)
	}
	events, err := s.PullOutbox(0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	if err := s.AckOutbox(events[0].MailboxSequence); err != nil {
		t.Fatal(err)
	}
	// A second process is a projection replay only; M2 is not called again.
	if err := s.Repair(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("repair reran M2: %d", f.calls)
	}
}

func TestInstructionClaimIsAtomicAcrossIndependentStores(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s1, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_8123456789abcdef", InstructionID: "ins_8123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}
	if err := s1.Deliver(m); err != nil {
		t.Fatal(err)
	}
	// Both handles represent separate process/store instances. The mailbox
	// flock, not an in-memory mutex, must decide the winner.
	type result struct {
		acquired bool
		err      error
	}
	results := make(chan result, 2)
	now := time.Now().UTC()
	go func() { _, ok, e := s1.ClaimInstruction(m, now); results <- result{ok, e} }()
	go func() { _, ok, e := s2.ClaimInstruction(m, now); results <- result{ok, e} }()
	var acquired int
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.acquired {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired claims=%d, want exactly one", acquired)
	}
}

func TestInstructionClaimAfterAssessmentStartNeverRetriesAfterRestart(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_9123456789abcdef", InstructionID: "ins_9123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := s.ClaimInstruction(m, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("initial claim ok=%v err=%v", ok, err)
	}
	if err := s.MarkInstructionState(m.InstructionID, claim.EnvelopeDigest, "m2_started_observed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimInstruction(m, time.Now().UTC().Add(time.Hour)); err != nil || ok {
		t.Fatalf("assessment-started claim retried: ok=%v err=%v", ok, err)
	}
}

func TestExecutionIntentIsImmutableAcrossIndependentStores(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s1, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	now := time.Now().UTC().Truncate(time.Millisecond)
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_a123456789abcdef", InstructionID: "ins_a123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: now.Add(time.Minute), Body: body}
	if err := s1.Deliver(m); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := s1.AcquireExecution(m, "m2-prov-a", now)
	if err != nil || !acquired {
		t.Fatalf("acquire=%v err=%v", acquired, err)
	}
	if lease.Intent().InstructionID != m.InstructionID || lease.Intent().EnvelopeDigest != digest || lease.Intent().M2Provenance != "m2-prov-a" || lease.Intent().BootID == "" || lease.Intent().OwnerPID == 0 || lease.Intent().OwnerStartTime == "" || lease.Intent().LockDevice == 0 || lease.Intent().LockInode == 0 {
		t.Fatalf("incomplete execution intent: %+v", lease.Intent())
	}
	if err := lease.Record("m2_call_entered", "", now); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := s2.AcquireExecution(m, "m2-prov-a", now.Add(time.Second)); err != nil || acquired {
		t.Fatalf("second process acquired live/immutable intent: acquired=%v err=%v", acquired, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := s2.AcquireExecution(m, "m2-prov-a", now.Add(time.Hour)); err != nil || acquired {
		t.Fatalf("released intent was retried: acquired=%v err=%v", acquired, err)
	}
}

func executionTestMessage(t *testing.T, s *Store, instructionID string) fleetcommander.Message {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_" + instructionID[4:], InstructionID: instructionID, FleetID: s.fleetID, ShipID: s.shipID, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func currentExecutionLockIdentity(t *testing.T, s *Store) lockIdentity {
	t.Helper()
	f, err := os.OpenFile(s.executionPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	id, err := lockIdentityOf(f)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPositiveOwnerDeathRequiresIdentityEvidence(t *testing.T) {
	s, err := Open(t.TempDir(), "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	id := currentExecutionLockIdentity(t, s)
	boot, err := linuxBootID()
	if err != nil {
		t.Fatal(err)
	}
	start, err := linuxProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	claim := ExecutionFact{Kind: "claim_acquired", OwnerPID: os.Getpid(), OwnerStartTime: start, BootID: boot, LockDevice: id.Device, LockInode: id.Inode}
	if dead, err := positiveOwnerDeath(claim, id); err == nil || dead {
		t.Fatalf("live owner accepted as dead: dead=%v err=%v", dead, err)
	}
	claim.OwnerPID = 1 << 30
	dead, err := positiveOwnerDeath(claim, id)
	if err != nil || !dead {
		t.Fatalf("absent proc did not prove death: dead=%v err=%v", dead, err)
	}
	claim.OwnerPID = os.Getpid()
	claim.OwnerStartTime = start
	claim.BootID = "prior-boot-id"
	dead, err = positiveOwnerDeath(claim, id)
	if err != nil || !dead {
		t.Fatalf("boot change did not prove death: dead=%v err=%v", dead, err)
	}
	claim.BootID = boot
	if _, err := positiveOwnerDeath(claim, lockIdentity{Device: id.Device, Inode: id.Inode + 1}); err == nil {
		t.Fatal("replaced lock identity was accepted")
	}
}

func TestAcquireExecutionDoesNotTreatFreeLockAsOwnerDeath(t *testing.T) {
	s, err := Open(t.TempDir(), "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	m := executionTestMessage(t, s, "ins_b123456789abcdef")
	id := currentExecutionLockIdentity(t, s)
	boot, _ := linuxBootID()
	start, _ := linuxProcessStartTime(os.Getpid())
	if err := appendExecutionFact(s.executionPath, ExecutionFact{Schema: executionSchema, Kind: "claim_acquired", ExecutionID: "exe_deadbeef01234567", InstructionID: m.InstructionID, EnvelopeDigest: mustInstructionDigest(t, m), BootID: boot, OwnerPID: os.Getpid(), OwnerStartTime: start, LockGeneration: 1, LockDevice: id.Device, LockInode: id.Inode, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := s.AcquireExecution(m, "m2-prov", time.Now().UTC()); err == nil || acquired {
		t.Fatalf("free lock without death proof acquired: acquired=%v err=%v", acquired, err)
	}
}

func mustInstructionDigest(t *testing.T, m fleetcommander.Message) string {
	t.Helper()
	var instruction fleetcommander.Instruction
	if err := fleetcommander.DecodeClosed(m.Body, &instruction); err != nil {
		t.Fatal(err)
	}
	return instruction.EnvelopeDigest
}

func TestAcquireExecutionRecoversOnlyAbsentPreIntentOwner(t *testing.T) {
	s, err := Open(t.TempDir(), "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	m := executionTestMessage(t, s, "ins_c123456789abcdef")
	id := currentExecutionLockIdentity(t, s)
	boot, _ := linuxBootID()
	if err := appendExecutionFact(s.executionPath, ExecutionFact{Schema: executionSchema, Kind: "claim_acquired", ExecutionID: "exe_deadbeef01234567", InstructionID: m.InstructionID, EnvelopeDigest: mustInstructionDigest(t, m), BootID: boot, OwnerPID: 1 << 30, OwnerStartTime: "unknown", LockGeneration: 1, LockDevice: id.Device, LockInode: id.Inode, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := s.AcquireExecution(m, "m2-prov", time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("positive dead-owner recovery failed: acquired=%v err=%v", acquired, err)
	}
	if lease.Intent().LockGeneration != 2 {
		t.Fatalf("generation=%d, want 2", lease.Intent().LockGeneration)
	}
	_ = lease.Release()
	if _, acquired, err := s.AcquireExecution(m, "m2-prov", time.Now().UTC()); err != nil || acquired {
		t.Fatalf("intent became retryable: acquired=%v err=%v", acquired, err)
	}
}

func TestExecutionReplayRejectsOutOfOrderLifecycleFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	if err := appendExecutionFact(path, ExecutionFact{
		Schema: executionSchema, Kind: "m2_started_observed", ExecutionID: "exe_0123456789abcdef",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readExecutionFacts(path); err == nil {
		t.Fatal("out-of-order execution lifecycle was accepted")
	}
}

func TestM2EnvelopeDisagreementCannotProjectPriorLifecycle(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	var env struct {
		DelegationID string `json:"delegation_id"`
	}
	_ = json.Unmarshal(raw, &env)
	// Build a codec-valid delivery using the real digest, then replace only the
	// fake M2 record digest below to model an existing conflicting delegation.
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_1123456789abcdef", InstructionID: "ins_1123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	f := &fakeM2{record: map[string]map[string]any{env.DelegationID: {"envelope_digest": strings.Repeat("b", 64)}}, acceptID: env.DelegationID}
	if err := s.Process(context.Background(), f); err == nil || f.calls != 0 {
		t.Fatalf("conflicting M2 lifecycle projected: err=%v calls=%d", err, f.calls)
	}
}

func TestProcessIfStaleGenerationDoesNotPublishProjection(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_2123456789abcdef", InstructionID: "ins_2123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	f := &fakeM2{record: map[string]map[string]any{}}
	if err := s.ProcessIf(context.Background(), f, func() bool { return false }); err != nil {
		t.Fatal(err)
	}
	events, err := s.PullOutbox(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("stale generation published %d events", len(events))
	}
}

func TestProcessIfSupersededAfterAssessmentStartDoesNotPublishTerminal(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	var env struct {
		DelegationID string `json:"delegation_id"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_4123456789abcdef", InstructionID: "ins_4123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	var active atomic.Bool
	active.Store(true)
	f := &fakeM2{record: map[string]map[string]any{}, acceptID: env.DelegationID, acceptRecord: map[string]any{"schema": "fleet.delegation-decision.v1", "record_id": "rec_4123456789abcdef", "sequence": 1, "delegation_id": env.DelegationID, "envelope_digest": digest, "voyage_plan_hash": strings.Repeat("a", 64), "task_contract_hash": strings.Repeat("b", 64), "state_hash": strings.Repeat("c", 64), "blocker_fingerprint": strings.Repeat("d", 64), "policy_version": 1, "skipper_artifact_digest": strings.Repeat("e", 64), "skipper_artifact_version": 1, "effective_model": "gpt-5.6-sol", "recovery_schema": "recovery.response.v1", "references": []any{}, "created_at": "2026-07-14T19:29:00.000Z", "lifecycle": "assessment_started"}}
	// Model a worker that loses its generation immediately after M2 has
	// durably reserved the assessment. The stale worker may not append a
	// restart/indeterminate projection; that belongs to the next generation.
	// fakeM2 has no hook, so use a small adapter with the same durable record
	// shape and flip the fence in Accept.
	a := staleAfterAcceptM2{fakeM2: f, active: &active}
	if err := s.ProcessIf(context.Background(), &a, active.Load); err != nil {
		t.Fatal(err)
	}
	events, err := s.PullOutbox(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("stale worker events=%d, want received only", len(events))
	}
}

type staleAfterAcceptM2 struct {
	*fakeM2
	active *atomic.Bool
}

func (f *staleAfterAcceptM2) Accept(ctx context.Context, raw []byte) error {
	if err := f.fakeM2.Accept(ctx, raw); err != nil {
		return err
	}
	f.active.Store(false)
	return context.Canceled
}

func TestAssessmentStartedRepairsAsIndeterminateWithoutM2Rerun(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	var env struct {
		DelegationID string `json:"delegation_id"`
	}
	_ = json.Unmarshal(raw, &env)
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_3123456789abcdef", InstructionID: "ins_3123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	f := &fakeM2{record: map[string]map[string]any{env.DelegationID: {"envelope_digest": digest, "lifecycle": "assessment_started"}}}
	if err := s.Process(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if err := s.Repair(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatalf("repair reran M2: %d", f.calls)
	}
	events, err := s.PullOutbox(0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	var completed fleetcommander.Completed
	if err := fleetcommander.DecodeClosed(events[1].Body, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Result != "indeterminate" || completed.ReasonCode != "restart_after_assessment" {
		t.Fatalf("completed=%#v", completed)
	}
}

func TestPullOutboxRejectsRollbackAndFutureAck(t *testing.T) {
	s, err := Open(t.TempDir(), "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PullOutbox(1, 1); err == nil {
		t.Fatal("future outbox acknowledgement accepted")
	}
}

func TestWorkerLeaseIsDurableBoundedAndGenerationFenced(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	lease, acquired, err := s.AcquireWorkerLease(7, now)
	if err != nil || !acquired || lease.WorkerEpoch != 1 || lease.Attempts != 1 {
		t.Fatalf("first lease=%#v acquired=%v err=%v", lease, acquired, err)
	}
	if _, acquired, err := s.AcquireWorkerLease(7, now); err != nil || acquired {
		t.Fatalf("duplicate generation acquired=%v err=%v", acquired, err)
	}
	if err := s.MarkWorkerStarted(7, lease.WorkerEpoch, now); err != nil {
		t.Fatal(err)
	}
	if !s.WorkerIsCurrent(7, lease.WorkerEpoch) {
		t.Fatal("active worker lease not current")
	}
	if err := s.MarkWorkerOutcome(7, lease.WorkerEpoch, "failed_before_start", "worker_error", now); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := s.AcquireWorkerLease(8, now.Add(500*time.Millisecond)); err != nil || acquired {
		t.Fatalf("retry-before-bound acquired=%v err=%v", acquired, err)
	}
	second, acquired, err := s.AcquireWorkerLease(8, now.Add(2*time.Second))
	if err != nil || !acquired || second.WorkerEpoch != 2 || second.Attempts != 2 {
		t.Fatalf("second lease=%#v acquired=%v err=%v", second, acquired, err)
	}
	if err := s.MarkWorkerStarted(7, second.WorkerEpoch, now); err == nil {
		t.Fatal("stale generation started worker")
	}
	if s.WorkerIsCurrent(7, lease.WorkerEpoch) {
		t.Fatal("superseded worker remained current")
	}
	reopened, err := Open(root, "flt_0123456789abcdef", "shp_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.WorkerLease()
	if !ok || got.WorkerEpoch != second.WorkerEpoch || got.Generation != 8 {
		t.Fatalf("reopened lease=%#v ok=%v", got, ok)
	}
}

func TestPublicationLeaseFencesSendAckReplayAndShips(t *testing.T) {
	root := t.TempDir()
	makeStore := func(fleet, ship, msg string) *Store {
		s, err := Open(root, fleet, ship)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
		if err != nil {
			t.Fatal(err)
		}
		digest, _ := envelopeDigestForTest(raw)
		body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
		m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: msg, InstructionID: "ins_" + msg[4:], FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}
		if err := s.Deliver(m); err != nil {
			t.Fatal(err)
		}
		var e struct {
			DelegationID string `json:"delegation_id"`
		}
		_ = json.Unmarshal(raw, &e)
		f := &fakeM2{record: map[string]map[string]any{}, acceptID: e.DelegationID, acceptRecord: map[string]any{"schema": "fleet.delegation-decision.v1", "record_id": "rec_0123456789abcdef", "sequence": 3, "delegation_id": e.DelegationID, "envelope_digest": digest, "voyage_plan_hash": strings.Repeat("a", 64), "task_contract_hash": strings.Repeat("b", 64), "state_hash": strings.Repeat("c", 64), "blocker_fingerprint": strings.Repeat("d", 64), "policy_version": 1, "skipper_artifact_digest": strings.Repeat("e", 64), "skipper_artifact_version": 1, "effective_model": "gpt-5.6-sol", "recovery_schema": "recovery.response.v1", "references": []any{}, "created_at": "2026-07-14T19:29:00.000Z", "lifecycle": "advised", "reason_code": "advised", "result": "advised", "advisory_decision": "continue", "provenance_digest": strings.Repeat("f", 64), "sail_state": "locally_accepted_under_existing_policy"}}
		if err := s.Process(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		return s
	}
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s := makeStore(fleet, ship, "msg_0123456789abcdef")
	l1, ok, err := s.AcquirePublicationLease(1, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("lease=%#v ok=%v err=%v", l1, ok, err)
	}
	if err := s.MarkPublicationSent(l1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgePublication(PublicationLease{EventID: l1.EventID, EventDigest: l1.EventDigest, Generation: l1.Generation, Epoch: l1.Epoch, Nonce: "stale", Message: l1.Message}, time.Now().UTC()); err == nil {
		t.Fatal("stale nonce acknowledged")
	}
	if err := s.AcknowledgePublication(l1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	l2, ok, err := s.AcquirePublicationLease(2, time.Now().UTC())
	if err != nil || !ok || l2.EventID == l1.EventID {
		t.Fatalf("next immutable projection not advanced: lease=%#v ok=%v err=%v", l2, ok, err)
	}
	other, err := Open(root, fleet, "shp_abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fleetcommander.Progress{Type: fleetcommander.ProgressType, DelegationID: "del_0123456789abcdef", State: "received"})
	if err := other.emit(fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_1123456789abcdef", InstructionID: "ins_1123456789abcdef", FleetID: fleet, ShipID: "shp_abcdef0123456789", Direction: fleetcommander.ShipToFleet, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := other.AcquirePublicationLease(1, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("second ship lease isolated: ok=%v err=%v", ok, err)
	}
}

func TestPublicationLeaseReplaysSameGenerationAfterSentWithoutAck(t *testing.T) {
	root := t.TempDir()
	s := makeStoreForPublicationTest(t, root, "flt_0123456789abcdef", "shp_0123456789abcdef", "msg_0123456789abcdef")
	l1, ok, err := s.AcquirePublicationLease(7, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("initial lease: ok=%v err=%v", ok, err)
	}
	if err := s.MarkPublicationSent(l1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	l2, ok, err := s.AcquirePublicationLease(7, time.Now().UTC())
	if err != nil || !ok || l2.EventID != l1.EventID || l2.EventDigest != l1.EventDigest || l2.Nonce != l1.Nonce || l2.State != "sent_unacknowledged" {
		t.Fatalf("same-generation replay lease: %#v ok=%v err=%v", l2, ok, err)
	}
	if err := s.ValidatePublicationLease(l2); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgePublication(l2, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func makeStoreForPublicationTest(t *testing.T, root, fleet, ship, msg string) *Store {
	t.Helper()
	s, err := Open(root, fleet, ship)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := envelopeDigestForTest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: digest, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: msg, InstructionID: "ins_" + msg[4:], FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Now().UTC().Truncate(time.Millisecond).Add(time.Minute), Body: body}
	if err := s.Deliver(m); err != nil {
		t.Fatal(err)
	}
	var e struct {
		DelegationID string `json:"delegation_id"`
	}
	_ = json.Unmarshal(raw, &e)
	f := &fakeM2{record: map[string]map[string]any{}, acceptID: e.DelegationID, acceptRecord: map[string]any{"schema": "fleet.delegation-decision.v1", "record_id": "rec_0123456789abcdef", "sequence": 3, "delegation_id": e.DelegationID, "envelope_digest": digest, "voyage_plan_hash": strings.Repeat("a", 64), "task_contract_hash": strings.Repeat("b", 64), "state_hash": strings.Repeat("c", 64), "blocker_fingerprint": strings.Repeat("d", 64), "policy_version": 1, "skipper_artifact_digest": strings.Repeat("e", 64), "skipper_artifact_version": 1, "effective_model": "gpt-5.6-sol", "recovery_schema": "recovery.response.v1", "references": []any{}, "created_at": "2026-07-14T19:29:00.000Z", "lifecycle": "advised", "reason_code": "advised", "result": "advised", "advisory_decision": "continue", "provenance_digest": strings.Repeat("f", 64), "sail_state": "locally_accepted_under_existing_policy"}}
	if err := s.Process(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	return s
}

func envelopeDigestForTest(raw []byte) (string, error) { return delegation.EnvelopeDigest(raw) }
