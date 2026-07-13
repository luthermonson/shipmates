package livesession

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

const (
	RemoteSteerCapability = "fleet.steer.turn.v1"
	RemoteSteerRetention  = 15 * time.Minute
	RemoteSteerMaxRecords = 256
	remoteSteerMaxTombs   = 256
)

type RemoteSteerOutcome string

const (
	RemoteSteerAccepted      RemoteSteerOutcome = "accepted"
	RemoteSteerRefused       RemoteSteerOutcome = "refused"
	RemoteSteerIndeterminate RemoteSteerOutcome = "indeterminate"
)

type RemoteSteerRetryDisposition string

const (
	RemoteSteerReplaySameOperation RemoteSteerRetryDisposition = "replay_same_operation"
	RemoteSteerFreshObservation    RemoteSteerRetryDisposition = "fresh_observation_required"
	RemoteSteerNoRetry             RemoteSteerRetryDisposition = "none"
)

// RemoteSteerOperator is authentication output, not a credential. The
// coordinator deliberately has no credential verifier or secret-bearing type.
type RemoteSteerOperator struct {
	SubjectID, CredentialID, FleetID string
	CredentialGeneration             uint64
	Capability                       string
}

// RemoteSteerTarget is minted and retained by the ship. Private M3 identifiers
// occur only here and in the coordinator's in-memory operation record.
type RemoteSteerTarget struct {
	Reference, FleetID, ShipID, Persona  string
	FleetEpoch, ConnectionGeneration     uint64
	SessionID, Backend, ThreadID, TurnID string
	ExpiresAt                            time.Time
}

// RemoteSteerRequest is the closed, already-validated turn_steer.v1 input.
type RemoteSteerRequest struct {
	ProtocolVersion                           uint64
	OperationID, OperationNonce               string
	FleetID, ShipID, Persona, TargetReference string
	FleetEpoch, ConnectionGeneration          uint64
	Message                                   string
	MessageSHA256                             string
	MessageUTF8Bytes                          uint64
}

type RemoteSteerResult struct {
	SchemaVersion    uint64                      `json:"schema_version"`
	OperationID      string                      `json:"operation_id"`
	Outcome          RemoteSteerOutcome          `json:"outcome"`
	ReasonCode       string                      `json:"reason_code"`
	RetryDisposition RemoteSteerRetryDisposition `json:"retry_disposition"`
}

// RemoteSteerAuditCandidate is a closed, sanitized ship-side audit value.
// It intentionally cannot carry the message, nonce, target, or raw M3 tuple.
type RemoteSteerAuditCandidate struct {
	SchemaVersion                                          uint64
	Kind, OperationID, SubjectID, CredentialID, Capability string
	CredentialGeneration, FleetEpoch, ConnectionGeneration uint64
	FleetID, ShipID, Persona, TargetHash, MessageSHA256    string
	MessageUTF8Bytes                                       uint64
	Outcome                                                RemoteSteerOutcome
	ReasonCode, Layer                                      string
	DecisionTime                                           time.Time
}

type RemoteSteerDecision struct {
	Outcome    RemoteSteerOutcome
	ReasonCode string
}

// ExactTurnSteerer is the backend-neutral M3 boundary used by deterministic
// tests. Implementations must commit at most one decision for a call.
type ExactTurnSteerer interface {
	SteerExactTurn(context.Context, string, string, string, string) RemoteSteerDecision
}

type remoteSteerRecord struct {
	fingerprint             [32]byte
	request                 RemoteSteerRequest
	target                  RemoteSteerTarget
	operator                RemoteSteerOperator
	firstReceipt, expiresAt time.Time
	committing              bool
	result                  RemoteSteerResult
	audit                   RemoteSteerAuditCandidate
	done                    chan struct{}
	terminalObserved        bool
}

type remoteSteerTomb struct {
	operationID, nonce string
	expiresAt          time.Time
}

// RemoteSteerCoordinator is ship-local, ephemeral, and backend-neutral.
type RemoteSteerCoordinator struct {
	mu          sync.Mutex
	manager     *Manager
	now         func() time.Time
	maxRecords  int
	records     map[string]*remoteSteerRecord
	nonces      map[string]string
	tombs       []remoteSteerTomb
	storeDir    string
	storeFailed bool
}

func NewRemoteSteerCoordinator(manager *Manager, now func() time.Time) *RemoteSteerCoordinator {
	if now == nil {
		now = time.Now
	}
	return &RemoteSteerCoordinator{manager: manager, now: now, maxRecords: RemoteSteerMaxRecords, records: make(map[string]*remoteSteerRecord), nonces: make(map[string]string)}
}

// OpenRemoteSteerCoordinator restores the ship-local at-most-once table. Any
// operation that was committing when the process stopped is durably resolved
// to indeterminate; it is never offered to the M3 boundary again.
func OpenRemoteSteerCoordinator(manager *Manager, now func() time.Time, dir string) (*RemoteSteerCoordinator, error) {
	c := NewRemoteSteerCoordinator(manager, now)
	c.storeDir = dir
	if err := c.loadRemoteSteerStore(); err != nil {
		return nil, err
	}
	changed := false
	for _, r := range c.records {
		if r.committing {
			r.committing = false
			r.result = remoteResult(r.request.OperationID, RemoteSteerIndeterminate, "fleet_restarted")
			r.audit = remoteAudit(c.now(), r.operator, r.request, r.target, r.result)
			close(r.done)
			changed = true
		}
	}
	if changed {
		if err := c.persistLocked(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *RemoteSteerCoordinator) Steer(ctx context.Context, op RemoteSteerOperator, req RemoteSteerRequest, target RemoteSteerTarget, adapter ExactTurnSteerer) (RemoteSteerResult, RemoteSteerAuditCandidate) {
	now := c.now()
	fp := remoteSteerFingerprint(op, req, target)
	c.mu.Lock()
	c.expireLocked(now)
	if old := c.records[req.OperationID]; old != nil {
		if subtle.ConstantTimeCompare(old.fingerprint[:], fp[:]) != 1 {
			c.mu.Unlock()
			return remoteResult(req.OperationID, RemoteSteerRefused, "operation_conflict"), RemoteSteerAuditCandidate{}
		}
		if old.committing {
			done := old.done
			c.mu.Unlock()
			select {
			case <-done:
				c.mu.Lock()
				r, a := old.result, old.audit
				c.mu.Unlock()
				return r, a
			case <-ctx.Done():
				return remoteResult(req.OperationID, RemoteSteerIndeterminate, "internal_uncertain"), RemoteSteerAuditCandidate{}
			}
		}
		r, a := old.result, old.audit
		c.mu.Unlock()
		return r, a
	}
	if prior := c.nonces[req.OperationNonce]; prior != "" {
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "operation_conflict"), RemoteSteerAuditCandidate{}
	}
	for _, tomb := range c.tombs {
		if tomb.operationID == req.OperationID || tomb.nonce == req.OperationNonce {
			c.mu.Unlock()
			return remoteResult(req.OperationID, RemoteSteerRefused, "dedup_expired"), RemoteSteerAuditCandidate{}
		}
	}
	if c.storeFailed {
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "shutdown"), RemoteSteerAuditCandidate{}
	}
	if len(c.records) >= c.maxRecords {
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "busy"), RemoteSteerAuditCandidate{}
	}
	if c.manager == nil || adapter == nil {
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "invalid_request"), RemoteSteerAuditCandidate{}
	}
	s, err := c.manager.Session(req.Persona)
	if err != nil {
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "stale_target"), RemoteSteerAuditCandidate{}
	}
	s.mu.Lock()
	reason := validateRemoteSteerLocked(now, s, op, req, target)
	if reason != "" {
		s.mu.Unlock()
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, reason), RemoteSteerAuditCandidate{}
	}
	rec := &remoteSteerRecord{fingerprint: fp, request: req, target: target, operator: op, firstReceipt: now, expiresAt: now.Add(RemoteSteerRetention), committing: true, done: make(chan struct{})}
	c.records[req.OperationID], c.nonces[req.OperationNonce] = rec, req.OperationID
	s.state = Steering
	if err := c.persistLocked(); err != nil {
		delete(c.records, req.OperationID)
		delete(c.nonces, req.OperationNonce)
		s.state = Working
		s.mu.Unlock()
		c.mu.Unlock()
		return remoteResult(req.OperationID, RemoteSteerRefused, "shutdown"), RemoteSteerAuditCandidate{}
	}
	s.mu.Unlock()
	c.mu.Unlock()

	decision := adapter.SteerExactTurn(ctx, target.SessionID, target.ThreadID, target.TurnID, req.Message)
	result := normalizeRemoteDecision(req.OperationID, decision)
	c.mu.Lock()
	s.mu.Lock()
	if s.state == Steering && s.sessionID == target.SessionID && s.threadID == target.ThreadID && s.turnID == target.TurnID {
		s.state = Working
	}
	rec.result = result
	rec.audit = remoteAudit(c.now(), op, req, target, result)
	rec.committing = false
	if err := c.persistLocked(); err != nil {
		rec.result = remoteResult(req.OperationID, RemoteSteerIndeterminate, "internal_uncertain")
		rec.audit = remoteAudit(c.now(), op, req, target, rec.result)
		// The terminal result was not durably established. Keep the in-memory
		// replay indeterminate; the durable pre-execution committing record makes
		// a restart resolve to fleet_restarted, never to the earlier decision.
	}
	close(rec.done)
	result = rec.result
	s.mu.Unlock()
	c.mu.Unlock()
	return result, rec.audit
}

func validateRemoteSteerLocked(now time.Time, s *Session, op RemoteSteerOperator, req RemoteSteerRequest, t RemoteSteerTarget) string {
	if req.ProtocolVersion != 1 || req.OperationID == "" || req.OperationNonce == "" || op.SubjectID == "" || op.CredentialID == "" || op.CredentialGeneration == 0 || op.Capability != RemoteSteerCapability {
		return "invalid_request"
	}
	if req.FleetID != op.FleetID || req.FleetID != t.FleetID || req.FleetEpoch != t.FleetEpoch || req.ShipID != t.ShipID || req.ConnectionGeneration == 0 || req.ConnectionGeneration != t.ConnectionGeneration || req.Persona != t.Persona || req.TargetReference == "" || req.TargetReference != t.Reference {
		return "stale_target"
	}
	if !now.Before(t.ExpiresAt) {
		return "target_expired"
	}
	if s.persona != t.Persona || s.sessionID != t.SessionID || s.snapshotLocked().Backend != t.Backend || s.threadID != t.ThreadID || s.turnID != t.TurnID {
		return "stale_target"
	}
	if s.approval != nil {
		return "approval_pending"
	}
	if s.shutting {
		return "shutdown"
	}
	if s.state == Steering || s.state == Interrupting || s.turnStarting {
		return "busy"
	}
	if s.state != Working {
		return "not_steerable"
	}
	digest := sha256.Sum256([]byte(req.Message))
	if req.MessageUTF8Bytes != uint64(len([]byte(req.Message))) || req.MessageSHA256 != hex.EncodeToString(digest[:]) {
		return "invalid_request"
	}
	return ""
}

func normalizeRemoteDecision(id string, d RemoteSteerDecision) RemoteSteerResult {
	switch d.Outcome {
	case RemoteSteerAccepted:
		return remoteResult(id, d.Outcome, "accepted")
	case RemoteSteerRefused:
		if d.ReasonCode == "" {
			d.ReasonCode = "not_steerable"
		}
		return remoteResult(id, d.Outcome, d.ReasonCode)
	default:
		if d.ReasonCode == "" {
			d.ReasonCode = "internal_uncertain"
		}
		return remoteResult(id, RemoteSteerIndeterminate, d.ReasonCode)
	}
}

func remoteResult(id string, outcome RemoteSteerOutcome, reason string) RemoteSteerResult {
	retry := RemoteSteerNoRetry
	if outcome == RemoteSteerIndeterminate {
		retry = RemoteSteerReplaySameOperation
	}
	if reason == "stale_target" || reason == "target_expired" || reason == "dedup_expired" {
		retry = RemoteSteerFreshObservation
	}
	return RemoteSteerResult{1, id, outcome, reason, retry}
}

func (c *RemoteSteerCoordinator) expireLocked(now time.Time) {
	changed := false
	for id, r := range c.records {
		if r.committing || now.Before(r.expiresAt) {
			continue
		}
		if s, err := c.manager.Session(r.target.Persona); err == nil {
			s.mu.Lock()
			active := s.sessionID == r.target.SessionID && s.threadID == r.target.ThreadID && s.turnID == r.target.TurnID && (s.state == Working || s.state == Steering || s.state == Interrupting)
			s.mu.Unlock()
			if active {
				r.expiresAt = now.Add(RemoteSteerRetention)
				continue
			}
		}
		if !r.terminalObserved {
			r.terminalObserved = true
			r.expiresAt = now.Add(RemoteSteerRetention)
			continue
		}
		delete(c.records, id)
		delete(c.nonces, r.request.OperationNonce)
		c.tombs = append(c.tombs, remoteSteerTomb{id, r.request.OperationNonce, now.Add(RemoteSteerRetention)})
		changed = true
	}
	n := 0
	for _, t := range c.tombs {
		if now.Before(t.expiresAt) {
			c.tombs[n] = t
			n++
		}
	}
	c.tombs = c.tombs[:n]
	if len(c.tombs) > remoteSteerMaxTombs {
		c.tombs = c.tombs[len(c.tombs)-remoteSteerMaxTombs:]
		changed = true
	}
	if changed {
		if c.persistLocked() != nil {
			c.storeFailed = true
		}
	}
}

func remoteSteerFingerprint(op RemoteSteerOperator, req RemoteSteerRequest, t RemoteSteerTarget) [32]byte {
	h := sha256.New()
	h.Write([]byte("shipmates.remote-steer.operation.v1"))
	var b [8]byte
	for _, v := range []string{op.SubjectID, op.CredentialID, op.FleetID, op.Capability, req.OperationID, req.OperationNonce, req.FleetID, req.ShipID, req.Persona, req.TargetReference, req.MessageSHA256, req.Message, t.SessionID, t.Backend, t.ThreadID, t.TurnID} {
		binary.BigEndian.PutUint64(b[:], uint64(len(v)))
		h.Write(b[:])
		h.Write([]byte(v))
	}
	for _, v := range []uint64{op.CredentialGeneration, req.ProtocolVersion, req.FleetEpoch, req.ConnectionGeneration, req.MessageUTF8Bytes, t.FleetEpoch, t.ConnectionGeneration} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func remoteAudit(now time.Time, op RemoteSteerOperator, req RemoteSteerRequest, t RemoteSteerTarget, r RemoteSteerResult) RemoteSteerAuditCandidate {
	h := sha256.Sum256([]byte(t.Reference))
	return RemoteSteerAuditCandidate{1, "remote_steer." + string(r.Outcome), req.OperationID, op.SubjectID, op.CredentialID, op.Capability, op.CredentialGeneration, req.FleetEpoch, req.ConnectionGeneration, req.FleetID, req.ShipID, req.Persona, hex.EncodeToString(h[:]), req.MessageSHA256, req.MessageUTF8Bytes, r.Outcome, r.ReasonCode, "ship", now}
}
