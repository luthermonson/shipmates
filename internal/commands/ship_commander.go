package commands

// This file is the private production composition boundary for M3. It is
// deliberately kept in commands so the existing production ship path owns
// configuration and the pinned adviser; no public Commander API is exposed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/delegation"
	"github.com/luthermonson/shipmates/internal/delegationmailbox"
	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
)

// makeCommanderStep binds one connection-generation-owned, serialized step
// factory to the authenticated identity already loaded by the tunnel.
func makeCommanderStep(root string, cfg project.Config, id fleetidentity.ShipState) (func(context.Context, fleettunnel.Channel, uint64) (fleettunnel.CommanderStep, error), error) {
	if !cfg.Recovery.AutoCaptain || !cfg.Recovery.CommanderDelegation.Enabled {
		return nil, nil
	}
	if id.FleetID != cfg.Recovery.CommanderDelegation.FleetID {
		return nil, errors.New("commander fleet identity mismatch")
	}
	policy, err := delegation.PolicyFromConfig(cfg.Recovery.CommanderDelegation)
	if err != nil {
		return nil, err
	}
	adviser, err := NewPinnedSkipperAdviserAt(root, nil)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, ch fleettunnel.Channel, generation uint64) (fleettunnel.CommanderStep, error) {
		local, planHash, err := trustedCommanderCase(root, id.FleetID, id.ShipID)
		if err != nil {
			return nil, err
		}
		digest, version, err := adviser.PinForAuthority()
		if err != nil {
			return nil, err
		}
		local.SkipperArtifactDigest, local.SkipperArtifactVersion = digest, version
		proc, err := delegation.Open(root, planHash, policy, adviser)
		if err != nil {
			return nil, err
		}
		mailbox, err := delegationmailbox.Open(root, id.FleetID, id.ShipID)
		if err != nil {
			return nil, err
		}
		fleetAck, shipAck := mailbox.Cursors()
		adapter := &productionM2Adapter{processor: proc, local: local, root: root}
		caps := codexapp.DetectExecutionCapabilities(cfg.Recovery.CommanderDelegation.PreExecHelper)
		workerCtx, workerCancel := context.WithCancel(ctx)
		return &productionCommanderStep{ctx: workerCtx, cancel: workerCancel, ch: ch, generation: generation, id: id, mailbox: mailbox, adapter: adapter, assessmentEnabled: caps.AssessmentEnabled(), fleetAck: fleetAck, shipAck: shipAck}, nil
	}, nil
}

type productionCommanderStep struct {
	ctx               context.Context
	cancel            context.CancelFunc
	ch                fleettunnel.Channel
	generation        uint64
	id                fleetidentity.ShipState
	mailbox           *delegationmailbox.Store
	adapter           *productionM2Adapter
	assessmentEnabled bool
	fleetAck, shipAck uint64
}

var errAssessmentComplete = errors.New("assessment complete")

type assessmentKey struct {
	FleetID, ShipID, InstructionID, EnvelopeDigest string
}

type processAssessmentRegistry struct {
	mu     sync.Mutex
	active map[string]int
	items  map[assessmentKey]*assessmentWorker
}

var shipProcessAssessments = &processAssessmentRegistry{active: make(map[string]int), items: make(map[assessmentKey]*assessmentWorker)}

func (s *productionCommanderStep) startPendingAssessments() error {
	if !s.assessmentEnabled {
		return nil
	}
	pending, err := s.mailbox.PendingInstructions()
	if err != nil {
		return err
	}
	for _, message := range pending {
		if err := shipProcessAssessments.ensure(s.ctx, s.generation, s.id.FleetID, s.id.ShipID, s.mailbox, s.adapter, message); err != nil {
			return err
		}
	}
	return nil
}

func (r *processAssessmentRegistry) ensure(parent context.Context, generation uint64, fleetID, shipID string, mailbox *delegationmailbox.Store, adapter *productionM2Adapter, message fleetcommander.Message) error {
	var instruction fleetcommander.Instruction
	if fleetcommander.DecodeClosed(message.Body, &instruction) != nil {
		return errors.New("invalid pending instruction")
	}
	key := assessmentKey{fleetID, shipID, message.InstructionID, instruction.EnvelopeDigest}
	r.mu.Lock()
	if _, ok := r.items[key]; ok {
		r.mu.Unlock()
		return nil
	}
	scope := fleetID + "\x00" + shipID
	if r.active[scope] >= 1 {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	workerLease, acquired, err := mailbox.AcquireWorkerLease(generation, time.Now().UTC())
	if err != nil || !acquired {
		return err
	}
	m2Provenance, err := recovery.Fingerprint(adapter.local.Request)
	if err != nil {
		_ = mailbox.MarkWorkerOutcome(generation, workerLease.WorkerEpoch, "failed_before_start", "provenance_unavailable", time.Now().UTC())
		return err
	}
	claim, claimed, err := mailbox.ClaimInstruction(message, time.Now().UTC())
	if err != nil || !claimed {
		if err != nil {
			_ = mailbox.MarkWorkerOutcome(generation, workerLease.WorkerEpoch, "failed_before_start", "instruction_claim_failed", time.Now().UTC())
		}
		return err
	}
	lease, acquired, err := mailbox.AcquireExecution(message, m2Provenance, time.Now().UTC())
	if err != nil || !acquired {
		if err != nil {
			_ = mailbox.MarkWorkerOutcome(generation, workerLease.WorkerEpoch, "failed_before_start", "execution_claim_failed", time.Now().UTC())
		}
		return err
	}
	if parent == nil {
		parent = context.Background()
	}
	worker := newLeasedAssessmentWorker(parent, func(workerCtx context.Context, active func() bool) error {
		if !active() {
			return context.Canceled
		}
		defer lease.Release()
		err := mailbox.ProcessIf(workerCtx, adapter, active)
		// M2's durable journal, not the connection generation, is the
		// irreversible-start witness. Record that boundary only after the
		// processor has published its accepted/started lifecycle.
		var envelope delegation.Envelope
		var instruction fleetcommander.Instruction
		if fleetcommander.DecodeClosed(message.Body, &instruction) == nil && json.Unmarshal(instruction.Envelope, &envelope) == nil {
			if record, ok := adapter.processor.Lookup(envelope.DelegationID); ok && record.Lifecycle == "assessment_started" {
				if err := lease.RecordM2StartWitness(delegation.StartWitnessDigest(record), time.Now().UTC()); err != nil {
					return err
				}
				if err := mailbox.MarkInstructionState(message.InstructionID, claim.EnvelopeDigest, "m2_started_observed", time.Now().UTC()); err != nil {
					return err
				}
			}
		}
		if err != nil {
			_ = lease.Record("indeterminate", "worker_error", time.Now().UTC())
			return err
		}
		_ = lease.Record("terminal", "assessment_complete", time.Now().UTC())
		return errAssessmentComplete
	}, func() error {
		if err := mailbox.MarkWorkerStarted(generation, workerLease.WorkerEpoch, time.Now().UTC()); err != nil {
			return err
		}
		if err := lease.Record("m2_call_entered", "", time.Now().UTC()); err != nil {
			return err
		}
		return mailbox.MarkInstructionState(message.InstructionID, claim.EnvelopeDigest, "m2_call_entered", time.Now().UTC())
	}, func(state, code string) error {
		workerState := state
		if workerState == "projection_pending" {
			workerState = "indeterminate"
		}
		if err := mailbox.MarkWorkerOutcome(generation, workerLease.WorkerEpoch, workerState, code, time.Now().UTC()); err != nil {
			return err
		}
		if result := mailbox.TerminalProjectionResult(); result != "" {
			if result == "indeterminate" {
				state, code = "indeterminate", "restart_after_assessment"
			} else {
				state, code = "terminal", "assessment_terminal"
			}
		} else {
			state, code = "projection_pending", "projection_pending"
		}
		return mailbox.MarkInstructionState(message.InstructionID, claim.EnvelopeDigest, state, time.Now().UTC())
	})
	worker.generation = generation
	r.mu.Lock()
	if _, exists := r.items[key]; exists {
		r.mu.Unlock()
		return nil
	}
	r.items[key], r.active[scope] = worker, r.active[scope]+1
	r.mu.Unlock()
	worker.Start()
	go func() {
		<-worker.done
		r.mu.Lock()
		if current, ok := r.items[key]; ok && current == worker {
			delete(r.items, key)
			if r.active[scope] > 0 {
				r.active[scope]--
				if r.active[scope] == 0 {
					delete(r.active, scope)
				}
			}
		}
		r.mu.Unlock()
	}()
	return nil
}

func (r *processAssessmentRegistry) closeGeneration(generation uint64) error {
	if r == nil || generation == 0 {
		return nil
	}
	r.mu.Lock()
	workers := make([]*assessmentWorker, 0)
	for _, worker := range r.items {
		if worker != nil && worker.generation == generation {
			workers = append(workers, worker)
		}
	}
	r.mu.Unlock()
	var first error
	for _, worker := range workers {
		if err := worker.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *productionCommanderStep) Step(ctx context.Context) error {
	if s == nil || s.mailbox == nil || s.adapter == nil {
		return &fleettunnel.CommanderLocalError{Err: errors.New("commander step unavailable")}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.startPendingAssessments(); err != nil {
		return &fleettunnel.CommanderLocalError{Err: err}
	}
	lease, leased, err := s.mailbox.AcquirePublicationLease(s.generation, time.Now().UTC())
	if err != nil {
		return &fleettunnel.CommanderLocalError{Err: err}
	}
	if leased {
		if lease.State == "publication_leased" {
			if err := s.mailbox.ValidatePublicationLease(lease); err != nil {
				return &fleettunnel.CommanderLocalError{Err: err}
			}
		}
		if lease.State != "publication_leased" && lease.State != "sent_unacknowledged" {
			return &fleettunnel.CommanderLocalError{Err: errors.New("invalid publication lease state")}
		}
		next, sendErr := fleettunnel.SendCommanderEvent(ctx, s.ch, s.id.FleetID, s.id.ShipID, s.generation, s.fleetAck, s.shipAck, lease.Message)
		if sendErr != nil {
			_ = s.mailbox.DetachPublicationLease(lease)
			return sendErr
		}
		if err := s.mailbox.MarkPublicationSent(lease, time.Now().UTC()); err != nil {
			return &fleettunnel.CommanderLocalError{Err: err}
		}
		if err := s.mailbox.AcknowledgePublication(lease, time.Now().UTC()); err != nil {
			return &fleettunnel.CommanderLocalError{Err: err}
		}
		s.shipAck = next
		return nil
	}
	var localErr error
	next, pullErr := fleettunnel.PullCommander(ctx, s.ch, s.id.FleetID, s.id.ShipID, s.generation, s.fleetAck, s.shipAck, func(m fleetcommander.Message) error {
		localErr = s.mailbox.Deliver(m)
		return localErr
	})
	if pullErr != nil {
		if localErr != nil {
			return &fleettunnel.CommanderLocalError{Err: localErr}
		}
		return pullErr
	}
	s.fleetAck = next
	if err := s.startPendingAssessments(); err != nil {
		return &fleettunnel.CommanderLocalError{Err: err}
	}
	return nil
}

func (s *productionCommanderStep) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	if lease, ok := s.mailbox.PublicationLease(); ok && lease.Generation == s.generation {
		_ = s.mailbox.DetachPublicationLease(lease)
	}
	return shipProcessAssessments.closeGeneration(s.generation)
}

const assessmentShutdownBound = 2 * time.Second

// assessmentWorker owns only local durable assessment work. It intentionally
// has no Channel, connection generation cursor, or Fleet callback. The
// scheduler remains responsible for one bounded carrier exchange per Step.
type assessmentWorker struct {
	ctx          context.Context
	cancel       context.CancelFunc
	process      func(context.Context, func() bool) error
	generation   uint64
	onStart      func() error
	onOutcome    func(string, string) error
	active       atomic.Bool
	attempted    atomic.Bool
	irreversible atomic.Bool
	done         chan struct{}
	once         sync.Once
	outcome      sync.Once
	outcomeMu    sync.Mutex
	outcomeSet   bool
	outcomeErr   error
}

func newAssessmentWorker(parent context.Context, process func(context.Context, func() bool) error) *assessmentWorker {
	return newLeasedAssessmentWorker(parent, process, nil, nil)
}

func newLeasedAssessmentWorker(parent context.Context, process func(context.Context, func() bool) error, onStart func() error, onOutcome func(string, string) error) *assessmentWorker {
	ctx, cancel := context.WithCancel(parent)
	w := &assessmentWorker{ctx: ctx, cancel: cancel, process: process, onStart: onStart, onOutcome: onOutcome, done: make(chan struct{})}
	w.active.Store(true)
	return w
}

func (w *assessmentWorker) Start() {
	go func() {
		defer close(w.done)
		defer func() {
			if recover() != nil {
				w.finishOutcome("indeterminate", "worker_panic")
			}
		}()
		if !w.isActive() {
			w.finishOutcome("failed_before_start", "worker_cancelled")
			return
		}
		if w.onStart != nil {
			if err := w.onStart(); err != nil {
				w.finishOutcome("failed_before_start", "worker_lease_start_failed")
				return
			}
		}
		w.irreversible.Store(w.onStart != nil)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
				if w.process != nil {
					w.attempted.Store(true)
					if err := w.process(w.ctx, w.isActive); err != nil {
						if errors.Is(err, errAssessmentComplete) {
							w.finishOutcome("completed", "assessment_complete")
							return
						}
						w.finishOutcome("indeterminate", "worker_error")
						return
					}
				}
			}
		}
	}()
}

func (w *assessmentWorker) finishOutcome(state, code string) {
	w.outcome.Do(func() {
		var err error
		if w.onOutcome != nil {
			err = w.onOutcome(state, code)
		}
		w.outcomeMu.Lock()
		w.outcomeSet = true
		w.outcomeErr = err
		w.outcomeMu.Unlock()
	})
}

func (w *assessmentWorker) isActive() bool { return w != nil && w.active.Load() }

func (w *assessmentWorker) Close() error {
	if w == nil {
		return nil
	}
	w.once.Do(func() {
		w.active.Store(false)
		w.cancel()
	})
	t := time.NewTimer(assessmentShutdownBound)
	defer t.Stop()
	select {
	case <-w.done:
		if !w.outcomeDone() {
			state, code := "failed_before_start", "worker_cancelled"
			if w.attempted.Load() || w.irreversible.Load() {
				state, code = "indeterminate", "worker_cancelled"
			}
			w.finishOutcome(state, code)
		}
		w.outcomeMu.Lock()
		err := w.outcomeErr
		w.outcomeMu.Unlock()
		return err
	case <-t.C:
		return errors.New("commander assessment worker shutdown timeout")
	}
}

func (w *assessmentWorker) outcomeDone() bool {
	w.outcomeMu.Lock()
	done := w.outcomeSet
	w.outcomeMu.Unlock()
	return done
}

type productionM2Adapter struct {
	processor *delegation.Processor
	local     delegation.LocalCase
	root      string
}

func (a *productionM2Adapter) Accept(ctx context.Context, raw []byte) error {
	if a == nil || a.processor == nil {
		return errors.New("missing local M2 processor")
	}
	// Rebuild and compare the trusted case at the mutation boundary. A changed
	// plan/state/task therefore fails closed instead of accepting stale remote
	// work through a long-lived connection.
	current, planHash, err := trustedCommanderCase(a.root, a.local.FleetID, a.local.ShipID)
	current.SkipperArtifactDigest, current.SkipperArtifactVersion = a.local.SkipperArtifactDigest, a.local.SkipperArtifactVersion
	if err != nil || planHash != a.local.VoyagePlanHash || current.FleetID != a.local.FleetID || current.ShipID != a.local.ShipID || current.TaskID != a.local.TaskID || current.TaskContractHash != a.local.TaskContractHash || current.StateHash != a.local.StateHash || current.BlockerFingerprint != a.local.BlockerFingerprint || current.Request.Provenance != a.local.Request.Provenance || current.Request.Reason != a.local.Request.Reason || current.Request.TierCount != a.local.Request.TierCount {
		return errors.New("commander local state changed")
	}
	_, err = a.processor.AcceptAndAssess(ctx, raw, current)
	return err
}

func (a *productionM2Adapter) Lookup(id string) (delegation.Record, bool) {
	if a == nil || a.processor == nil {
		return delegation.Record{}, false
	}
	return a.processor.Lookup(id)
}

// trustedCommanderCase reads only approved voyage/state/recovery records. It
// intentionally does not inspect prompts, Beads, Fleet data, or opaque
// envelope references.
func trustedCommanderCase(root, fleetID, shipID string) (delegation.LocalCase, string, error) {
	if fleetID == "" || shipID == "" {
		return delegation.LocalCase{}, "", errors.New("missing authenticated commander identity")
	}
	planPath := filepath.Join(root, project.Dir, "voyage.json")
	plan, canonical, err := voyage.Load(planPath)
	if err != nil {
		return delegation.LocalCase{}, "", err
	}
	planHash := voyage.Hash(canonical)
	statePath := filepath.Join(root, project.Dir, "voyages", "state.json")
	rawState, err := os.ReadFile(statePath)
	if err != nil {
		return delegation.LocalCase{}, "", err
	}
	state, err := voyage.LoadStateStrict(statePath, plan, planHash)
	if err != nil {
		return delegation.LocalCase{}, "", err
	}
	stateHash := voyage.StateHash(rawState)
	var selected *voyage.Task
	var entry voyage.TaskState
	for i := range plan.Tasks {
		t := &plan.Tasks[i]
		e := state.Tasks[t.ID]
		if e.Status != voyage.Failed && e.Status != voyage.Blocked && e.Status != voyage.NeedsInput {
			continue
		}
		if selected != nil {
			return delegation.LocalCase{}, "", errors.New("ambiguous commander task state")
		}
		selected, entry = t, e
	}
	if selected == nil || entry.TaskFingerprint == "" {
		return delegation.LocalCase{}, "", errors.New("no actionable commander task state")
	}
	j, err := recovery.OpenJournal(filepath.Join(root, project.Dir, "recovery", planHash[:16]+".jsonl"))
	if err != nil {
		return delegation.LocalCase{}, "", err
	}
	var matched []recovery.BlockerRecord
	for _, blocker := range j.Blockers() {
		if blocker.Provenance.VoyagePlanHash == planHash && blocker.Provenance.TaskID == selected.ID && blocker.Provenance.TaskContractHash == entry.TaskFingerprint && blocker.Provenance.StateHash == stateHash {
			matched = append(matched, blocker)
		}
	}
	if len(matched) != 1 {
		return delegation.LocalCase{}, "", errors.New("missing or ambiguous current commander blocker")
	}
	b := matched[0]
	req := recovery.RequestV1{SchemaVersion: recovery.SchemaVersion, Provenance: b.Provenance, Reason: b.Reason, TierCount: uint8(selected.TierCount()), Evidence: append([]recovery.EvidenceRef(nil), b.Evidence...)}
	fp, err := recovery.Fingerprint(req)
	if err != nil {
		return delegation.LocalCase{}, "", err
	}
	return delegation.LocalCase{FleetID: fleetID, ShipID: shipID, VoyagePlanHash: planHash, TaskContractHash: entry.TaskFingerprint, StateHash: stateHash, TaskID: selected.ID, BlockerFingerprint: fp, Request: req}, planHash, nil
}
