package livesession

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"path/filepath"
	"sync"
	"time"
)

const (
	RemoteInterruptCapability = "fleet.interrupt.turn.v1"
	RemoteInterruptRetention  = 15 * time.Minute
	RemoteInterruptMaxRecords = 256
	remoteInterruptMaxTombs   = 256
)

type RemoteInterruptOutcome string

const (
	RemoteInterruptAccepted        RemoteInterruptOutcome = "accepted"
	RemoteInterruptInterrupted     RemoteInterruptOutcome = "interrupted"
	RemoteInterruptAlreadyTerminal RemoteInterruptOutcome = "already_terminal"
	RemoteInterruptRefused         RemoteInterruptOutcome = "refused"
	RemoteInterruptIndeterminate   RemoteInterruptOutcome = "indeterminate"
)

type RemoteInterruptRetryDisposition string

const (
	RemoteInterruptReplaySameOperation RemoteInterruptRetryDisposition = "replay_same_operation"
	RemoteInterruptFreshObservation    RemoteInterruptRetryDisposition = "fresh_observation_required"
	RemoteInterruptNoRetry             RemoteInterruptRetryDisposition = "none"
)

// RemoteInterruptOperator is the output of authentication and exact-scope
// authorization. It contains no credential secret or verifier.
type RemoteInterruptOperator struct {
	SubjectID, CredentialID, FleetID string
	CredentialGeneration             uint64
	Capability                       string
}

// RemoteInterruptTarget is an expiring ship-private target-table record.
type RemoteInterruptTarget struct {
	Reference, FleetID, ShipID, Persona  string
	FleetEpoch, ConnectionGeneration     uint64
	SessionID, Backend, ThreadID, TurnID string
	ExpiresAt                            time.Time
}

// RemoteInterruptRequest is an already strictly decoded turn_interrupt.v1
// message. Private turn identifiers are deliberately absent.
type RemoteInterruptRequest struct {
	ProtocolVersion                           uint64
	OperationID, OperationNonce               string
	FleetID, ShipID, Persona, TargetReference string
	FleetEpoch, ConnectionGeneration          uint64
}

type RemoteInterruptResult struct {
	SchemaVersion    uint64                          `json:"schema_version"`
	OperationID      string                          `json:"operation_id"`
	Outcome          RemoteInterruptOutcome          `json:"outcome"`
	ReasonCode       string                          `json:"reason_code"`
	RetryDisposition RemoteInterruptRetryDisposition `json:"retry_disposition"`
}

// RemoteInterruptAuditCandidate is a closed safe projection. In particular it
// cannot represent a nonce, raw target reference, or private turn tuple.
type RemoteInterruptAuditCandidate struct {
	SchemaVersion                                          uint64
	Kind, OperationID, SubjectID, CredentialID, Capability string
	CredentialGeneration, FleetEpoch, ConnectionGeneration uint64
	FleetID, ShipID, Persona, TargetHash                   string
	Outcome                                                RemoteInterruptOutcome
	ReasonCode, Layer                                      string
	DecisionTime                                           time.Time
	ApprovalCancelled                                      bool
}

type ApprovalCancelOutcome string

const (
	ApprovalAbsent    ApprovalCancelOutcome = "absent"
	ApprovalCancelled ApprovalCancelOutcome = "cancelled"
	ApprovalUnknown   ApprovalCancelOutcome = "unknown"
)

// PendingApprovalCanceller is the internal M6 deny-class boundary. Unknown
// means delivery cannot be proven and requires fail-closed ownership.
type PendingApprovalCanceller interface {
	CancelPendingApproval(context.Context, string, string, string) ApprovalCancelOutcome
}

type ExactTurnInterruptOutcome string

const (
	ExactTurnInterrupted     ExactTurnInterruptOutcome = "interrupted"
	ExactTurnAlreadyTerminal ExactTurnInterruptOutcome = "already_terminal"
	ExactTurnUnknown         ExactTurnInterruptOutcome = "indeterminate"
)

// ExactTurnInterrupter is the backend-neutral M3 exact-turn boundary.
type ExactTurnInterrupter interface {
	InterruptExactTurn(context.Context, string, string, string) ExactTurnInterruptOutcome
}

type RemoteInterruptBarrier func(stage string)

type remoteInterruptRecord struct {
	fingerprint             [32]byte
	request                 RemoteInterruptRequest
	target                  RemoteInterruptTarget
	operator                RemoteInterruptOperator
	firstReceipt, expiresAt time.Time
	result                  RemoteInterruptResult
	audit                   RemoteInterruptAuditCandidate
	done                    chan struct{}
	committing              bool
}

type remoteInterruptTomb struct {
	operationID, nonce string
	expiresAt          time.Time
}

// RemoteInterruptCoordinator is ephemeral and ship-local. All admission and
// local interrupt ownership is serialized with the target Session lock.
type RemoteInterruptCoordinator struct {
	mu         sync.Mutex
	manager    *Manager
	now        func() time.Time
	barrier    RemoteInterruptBarrier
	maxRecords int
	records    map[string]*remoteInterruptRecord
	nonces     map[string]string
	tombs      []remoteInterruptTomb
	shutdown   bool
	storeDir   string
	audit      RemoteInterruptAuditSink
	persist    func() error
}

type RemoteInterruptAuditSink interface {
	AppendRemoteInterrupt(RemoteInterruptAuditCandidate) error
}

// OpenRemoteInterruptCoordinator restores the separate M9 at-most-once
// journal. Unfinished operations become restart uncertainty and are never
// applied again.
func OpenRemoteInterruptCoordinator(manager *Manager, now func() time.Time, dir string, barrier RemoteInterruptBarrier) (*RemoteInterruptCoordinator, error) {
	c := NewRemoteInterruptCoordinator(manager, now, barrier)
	c.storeDir = dir
	var err error
	c.audit, err = openRemoteInterruptAuditSink(filepath.Join(dir, "audit"))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadRemoteInterruptStore(); err != nil {
		return nil, err
	}
	for _, r := range c.records {
		if r.committing {
			r.result = remoteInterruptResult(r.request.OperationID, RemoteInterruptIndeterminate, "ship_restarted")
			r.audit = remoteInterruptAudit(c.now(), r.operator, r.request, r.target, r.result, false)
			r.committing = false
			close(r.done)
		}
	}
	if err := c.persistJournalLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

func NewRemoteInterruptCoordinator(manager *Manager, now func() time.Time, barrier RemoteInterruptBarrier) *RemoteInterruptCoordinator {
	if now == nil {
		now = time.Now
	}
	if barrier == nil {
		barrier = func(string) {}
	}
	return &RemoteInterruptCoordinator{manager: manager, now: now, barrier: barrier, maxRecords: RemoteInterruptMaxRecords, records: make(map[string]*remoteInterruptRecord), nonces: make(map[string]string)}
}

func (c *RemoteInterruptCoordinator) Interrupt(ctx context.Context, op RemoteInterruptOperator, req RemoteInterruptRequest, target RemoteInterruptTarget, approvals PendingApprovalCanceller, interrupt ExactTurnInterrupter) (RemoteInterruptResult, RemoteInterruptAuditCandidate) {
	now := c.now()
	fp := remoteInterruptFingerprint(op, req, target)
	c.barrier("before_validation")
	c.mu.Lock()
	c.expireLocked(now)
	if old := c.records[req.OperationID]; old != nil {
		if subtle.ConstantTimeCompare(old.fingerprint[:], fp[:]) != 1 {
			c.mu.Unlock()
			return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "operation_conflict"), RemoteInterruptAuditCandidate{}
		}
		if old.committing {
			done := old.done
			c.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain"), RemoteInterruptAuditCandidate{}
			}
			c.mu.Lock()
		}
		r, a := old.result, old.audit
		c.mu.Unlock()
		return r, a
	}
	if prior := c.nonces[req.OperationNonce]; prior != "" {
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "operation_conflict"), RemoteInterruptAuditCandidate{}
	}
	for _, t := range c.tombs {
		if t.operationID == req.OperationID || t.nonce == req.OperationNonce {
			c.mu.Unlock()
			return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "dedup_expired"), RemoteInterruptAuditCandidate{}
		}
	}
	if c.shutdown {
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "shutdown"), RemoteInterruptAuditCandidate{}
	}
	if len(c.records) >= c.maxRecords {
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "busy"), RemoteInterruptAuditCandidate{}
	}
	if c.manager == nil || approvals == nil || interrupt == nil {
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "invalid_request"), RemoteInterruptAuditCandidate{}
	}
	received := remoteInterruptAudit(now, op, req, target, remoteInterruptResult(req.OperationID, RemoteInterruptAccepted, "received"), false)
	received.Kind, received.Outcome = "remote_interrupt.received", ""
	if c.audit != nil && c.audit.AppendRemoteInterrupt(received) != nil {
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "audit_unavailable"), RemoteInterruptAuditCandidate{}
	}
	s, err := c.manager.Session(req.Persona)
	if err != nil {
		refused := remoteInterruptAudit(now, op, req, target, remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "stale_target"), false)
		if c.audit != nil && c.audit.AppendRemoteInterrupt(refused) != nil {
			c.mu.Unlock()
			return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "audit_unavailable"), RemoteInterruptAuditCandidate{}
		}
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, "stale_target"), RemoteInterruptAuditCandidate{}
	}
	s.mu.Lock()
	if reason := validateRemoteInterruptLocked(now, s, op, req, target); reason != "" {
		if reason == "already_terminal" {
			r := remoteInterruptResult(req.OperationID, RemoteInterruptAlreadyTerminal, reason)
			a := remoteInterruptAudit(now, op, req, target, r, false)
			rec := &remoteInterruptRecord{fingerprint: fp, request: req, target: target, operator: op, firstReceipt: now, expiresAt: now.Add(RemoteInterruptRetention), result: r, audit: a, done: make(chan struct{})}
			c.records[req.OperationID], c.nonces[req.OperationNonce] = rec, req.OperationID
			if err := c.persistJournalLocked(); err != nil {
				r = remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain")
				rec.result, rec.audit = r, RemoteInterruptAuditCandidate{}
				// Retain the uncertain operation for same-process replay and make
				// that uncertainty durable when the injected/storage failure was
				// pre-commit. No definitive audit is eligible on this path.
				_ = c.persistJournalLocked()
				close(rec.done)
				s.mu.Unlock()
				c.mu.Unlock()
				return r, RemoteInterruptAuditCandidate{}
			}
			if c.audit != nil && c.audit.AppendRemoteInterrupt(a) != nil {
				r = remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain")
				rec.result, rec.audit = r, RemoteInterruptAuditCandidate{}
				if err := c.persistJournalLocked(); err != nil {
					// The definitive result is already durable. Do not return a
					// result that a reopen cannot replay.
					rec.result, rec.audit = remoteInterruptResult(req.OperationID, RemoteInterruptAlreadyTerminal, reason), a
					r = rec.result
				}
			}
			close(rec.done)
			s.mu.Unlock()
			c.mu.Unlock()
			return r, rec.audit
		}
		refused := remoteInterruptAudit(now, op, req, target, remoteInterruptResult(req.OperationID, RemoteInterruptRefused, reason), false)
		if c.audit != nil && c.audit.AppendRemoteInterrupt(refused) != nil {
			s.mu.Unlock()
			c.mu.Unlock()
			return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "audit_unavailable"), RemoteInterruptAuditCandidate{}
		}
		s.mu.Unlock()
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptRefused, reason), RemoteInterruptAuditCandidate{}
	}
	rec := &remoteInterruptRecord{fingerprint: fp, request: req, target: target, operator: op, firstReceipt: now, expiresAt: now.Add(RemoteInterruptRetention), committing: true, result: remoteInterruptResult(req.OperationID, RemoteInterruptAccepted, "accepted"), done: make(chan struct{})}
	acceptedAudit := remoteInterruptAudit(now, op, req, target, rec.result, false)
	c.records[req.OperationID], c.nonces[req.OperationNonce] = rec, req.OperationID
	if err := c.persistJournalLocked(); err != nil {
		delete(c.records, req.OperationID)
		delete(c.nonces, req.OperationNonce)
		s.state = Working
		s.mu.Unlock()
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain"), RemoteInterruptAuditCandidate{}
	}
	if c.audit != nil && c.audit.AppendRemoteInterrupt(acceptedAudit) != nil {
		delete(c.records, req.OperationID)
		delete(c.nonces, req.OperationNonce)
		_ = c.persistJournalLocked()
		s.mu.Unlock()
		c.mu.Unlock()
		return remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "audit_unavailable"), RemoteInterruptAuditCandidate{}
	}
	s.state = Interrupting
	s.mu.Unlock()
	c.mu.Unlock()
	c.barrier("accepted")

	approval := approvals.CancelPendingApproval(ctx, target.SessionID, target.ThreadID, target.TurnID)
	c.barrier("approval_closed")
	if approval == ApprovalUnknown {
		s.mu.Lock()
		if exactRemoteInterruptTurn(s, target) {
			s.state, s.shutting = Failed, true
		}
		s.mu.Unlock()
		return c.complete(rec, op, req, target, RemoteInterruptIndeterminate, "approval_cancel_unknown", false)
	}
	if approval != ApprovalAbsent && approval != ApprovalCancelled {
		return c.complete(rec, op, req, target, RemoteInterruptIndeterminate, "internal_uncertain", false)
	}

	decision := interrupt.InterruptExactTurn(ctx, target.SessionID, target.ThreadID, target.TurnID)
	c.barrier("interrupt_returned")
	s.mu.Lock()
	if exactRemoteInterruptTurn(s, target) && decision == ExactTurnAlreadyTerminal {
		s.state, s.turnID = Idle, ""
	}
	s.mu.Unlock()
	switch decision {
	case ExactTurnInterrupted:
		return c.complete(rec, op, req, target, RemoteInterruptInterrupted, "interrupted", approval == ApprovalCancelled)
	case ExactTurnAlreadyTerminal:
		return c.complete(rec, op, req, target, RemoteInterruptAlreadyTerminal, "already_terminal", approval == ApprovalCancelled)
	default:
		return c.complete(rec, op, req, target, RemoteInterruptIndeterminate, "terminal_unknown", approval == ApprovalCancelled)
	}
}

// LocalInterrupt enters the same lifecycle serialization boundary. It owns no
// remote operation record and therefore cannot join a different exact turn.
func (c *RemoteInterruptCoordinator) LocalInterrupt(ctx context.Context, persona, sessionID, threadID, turnID string, approvals PendingApprovalCanceller, interrupt ExactTurnInterrupter) ExactTurnInterruptOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shutdown || c.manager == nil || approvals == nil || interrupt == nil {
		return ExactTurnUnknown
	}
	s, err := c.manager.Session(persona)
	if err != nil {
		return ExactTurnAlreadyTerminal
	}
	s.mu.Lock()
	if s.sessionID != sessionID || s.threadID != threadID || s.turnID != turnID {
		s.mu.Unlock()
		return ExactTurnAlreadyTerminal
	}
	if s.state == Interrupting {
		s.mu.Unlock()
		return ExactTurnUnknown
	}
	if s.state != Working && s.state != Steering {
		s.mu.Unlock()
		return ExactTurnAlreadyTerminal
	}
	s.state = Interrupting
	s.mu.Unlock()
	c.mu.Unlock()
	a := approvals.CancelPendingApproval(ctx, sessionID, threadID, turnID)
	if a == ApprovalUnknown {
		s.mu.Lock()
		s.state, s.shutting = Failed, true
		s.mu.Unlock()
		c.mu.Lock()
		return ExactTurnUnknown
	}
	r := interrupt.InterruptExactTurn(ctx, sessionID, threadID, turnID)
	c.mu.Lock()
	return r
}

func validateRemoteInterruptLocked(now time.Time, s *Session, op RemoteInterruptOperator, r RemoteInterruptRequest, t RemoteInterruptTarget) string {
	if r.ProtocolVersion != 1 {
		return "unsupported_version"
	}
	if r.OperationID == "" || r.OperationNonce == "" || op.SubjectID == "" || op.CredentialID == "" || op.CredentialGeneration == 0 || op.FleetID == "" || op.Capability != RemoteInterruptCapability {
		return "invalid_request"
	}
	if r.FleetID != op.FleetID || r.FleetID != t.FleetID || r.FleetEpoch == 0 || r.FleetEpoch != t.FleetEpoch || r.ShipID == "" || r.ShipID != t.ShipID || r.ConnectionGeneration == 0 || r.ConnectionGeneration != t.ConnectionGeneration || r.Persona == "" || r.Persona != t.Persona || r.TargetReference == "" || r.TargetReference != t.Reference {
		return "stale_target"
	}
	if !now.Before(t.ExpiresAt) {
		return "target_expired"
	}
	if s.persona != t.Persona || s.sessionID != t.SessionID || s.snapshotLocked().Backend != t.Backend || s.threadID != t.ThreadID || s.turnID != t.TurnID {
		return "stale_target"
	}
	if s.shutting {
		return "shutdown"
	}
	if s.state == Idle {
		return "already_terminal"
	}
	if s.state == Interrupting {
		return "busy"
	}
	if s.state != Working && s.state != Steering {
		return "not_active"
	}
	return ""
}

func exactRemoteInterruptTurn(s *Session, t RemoteInterruptTarget) bool {
	return s.sessionID == t.SessionID && s.threadID == t.ThreadID && s.turnID == t.TurnID
}

func (c *RemoteInterruptCoordinator) complete(rec *remoteInterruptRecord, op RemoteInterruptOperator, req RemoteInterruptRequest, target RemoteInterruptTarget, outcome RemoteInterruptOutcome, reason string, cancelled bool) (RemoteInterruptResult, RemoteInterruptAuditCandidate) {
	now := c.now()
	r := remoteInterruptResult(req.OperationID, outcome, reason)
	a := remoteInterruptAudit(now, op, req, target, r, cancelled)
	c.mu.Lock()
	rec.result, rec.audit, rec.committing = r, a, false
	rec.expiresAt = now.Add(RemoteInterruptRetention)
	if err := c.persistJournalLocked(); err != nil {
		r = remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain")
		rec.result, rec.audit = r, RemoteInterruptAuditCandidate{}
		// The previously durable committing record reopens indeterminate.
		_ = c.persistJournalLocked()
	} else if c.audit != nil && c.audit.AppendRemoteInterrupt(a) != nil {
		r = remoteInterruptResult(req.OperationID, RemoteInterruptIndeterminate, "internal_uncertain")
		rec.result, rec.audit = r, RemoteInterruptAuditCandidate{}
		if err := c.persistJournalLocked(); err != nil {
			// Keep the already durable terminal result in memory and at the
			// API boundary when the fail-closed rewrite cannot be committed.
			rec.result, rec.audit = remoteInterruptResult(req.OperationID, outcome, reason), a
			r = rec.result
		}
	}
	close(rec.done)
	c.mu.Unlock()
	return r, rec.audit
}

func (c *RemoteInterruptCoordinator) persistJournalLocked() error {
	if c.persist != nil {
		return c.persist()
	}
	return c.persistLocked()
}

func remoteInterruptResult(id string, outcome RemoteInterruptOutcome, reason string) RemoteInterruptResult {
	retry := RemoteInterruptNoRetry
	if outcome == RemoteInterruptAccepted || outcome == RemoteInterruptIndeterminate {
		retry = RemoteInterruptReplaySameOperation
	}
	if reason == "stale_target" || reason == "target_expired" || reason == "dedup_expired" || reason == "not_active" {
		retry = RemoteInterruptFreshObservation
	}
	return RemoteInterruptResult{1, id, outcome, reason, retry}
}

func remoteInterruptAudit(now time.Time, op RemoteInterruptOperator, r RemoteInterruptRequest, t RemoteInterruptTarget, result RemoteInterruptResult, cancelled bool) RemoteInterruptAuditCandidate {
	h := sha256.Sum256([]byte(t.Reference))
	return RemoteInterruptAuditCandidate{SchemaVersion: 1, Kind: "remote_interrupt." + string(result.Outcome), OperationID: r.OperationID, SubjectID: op.SubjectID, CredentialID: op.CredentialID, Capability: op.Capability, CredentialGeneration: op.CredentialGeneration, FleetEpoch: r.FleetEpoch, ConnectionGeneration: r.ConnectionGeneration, FleetID: r.FleetID, ShipID: r.ShipID, Persona: r.Persona, TargetHash: stringHex(h[:]), Outcome: result.Outcome, ReasonCode: result.ReasonCode, Layer: "ship", DecisionTime: now, ApprovalCancelled: cancelled}
}

func (c *RemoteInterruptCoordinator) expireLocked(now time.Time) {
	for id, r := range c.records {
		if r.committing || now.Before(r.expiresAt) {
			continue
		}
		delete(c.records, id)
		delete(c.nonces, r.request.OperationNonce)
		c.tombs = append(c.tombs, remoteInterruptTomb{id, r.request.OperationNonce, now.Add(RemoteInterruptRetention)})
	}
	n := 0
	for _, t := range c.tombs {
		if now.Before(t.expiresAt) {
			c.tombs[n] = t
			n++
		}
	}
	c.tombs = c.tombs[:n]
	if len(c.tombs) > remoteInterruptMaxTombs {
		c.tombs = c.tombs[len(c.tombs)-remoteInterruptMaxTombs:]
	}
}

func (c *RemoteInterruptCoordinator) Shutdown() { c.mu.Lock(); c.shutdown = true; c.mu.Unlock() }

func remoteInterruptFingerprint(op RemoteInterruptOperator, r RemoteInterruptRequest, t RemoteInterruptTarget) [32]byte {
	h := sha256.New()
	h.Write([]byte("shipmates.remote-interrupt.operation.v1\x00"))
	var b [8]byte
	for _, v := range []string{op.SubjectID, op.CredentialID, op.FleetID, op.Capability, r.OperationID, r.OperationNonce, r.FleetID, r.ShipID, r.Persona, r.TargetReference, t.Reference, t.FleetID, t.ShipID, t.Persona, t.SessionID, t.Backend, t.ThreadID, t.TurnID} {
		binary.BigEndian.PutUint64(b[:], uint64(len(v)))
		h.Write(b[:])
		h.Write([]byte(v))
	}
	for _, v := range []uint64{op.CredentialGeneration, r.ProtocolVersion, r.FleetEpoch, r.ConnectionGeneration, t.FleetEpoch, t.ConnectionGeneration} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func stringHex(p []byte) string {
	const x = "0123456789abcdef"
	b := make([]byte, len(p)*2)
	for i, v := range p {
		b[2*i], b[2*i+1] = x[v>>4], x[v&15]
	}
	return string(b)
}
