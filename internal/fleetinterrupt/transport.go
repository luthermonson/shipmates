// Package fleetinterrupt implements only the closed M9 exact-turn interrupt
// transport. It deliberately has no CLI, UI, generic RPC, or other authority.
package fleetinterrupt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/livesession"
)

const (
	DeliveryDeadline       = 5 * time.Second
	TotalDeadline          = 12 * time.Second
	TargetLifetime         = 60 * time.Second
	ObservationFreshness   = 60 * time.Second
	SubjectAdmissionWindow = time.Minute
	ShipAdmissionWindow    = time.Second
	TargetAdmissionWindow  = time.Minute
	MaxFleetOperations     = 256
	MaxFleetWaiters        = 64
)

// Clock supplies time to one configured timing domain.
type Clock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// ServiceConfig preserves the frozen production timings when fields are zero.
// A shorter target lifetime is useful for deployments that require shorter
// lived exact-turn capabilities without weakening observation leases.
type ServiceConfig struct {
	Clock                  Clock
	AdmissionClock         Clock
	TargetLifetime         time.Duration
	ObservationFreshness   time.Duration
	SubjectAdmissionWindow time.Duration
	ShipAdmissionWindow    time.Duration
	TargetAdmissionWindow  time.Duration
}

func normalizeServiceConfig(c ServiceConfig) ServiceConfig {
	if c.Clock == nil {
		c.Clock = wallClock{}
	}
	if c.AdmissionClock == nil {
		c.AdmissionClock = c.Clock
	}
	if c.TargetLifetime == 0 {
		c.TargetLifetime = TargetLifetime
	}
	if c.ObservationFreshness == 0 {
		c.ObservationFreshness = ObservationFreshness
	}
	if c.SubjectAdmissionWindow == 0 {
		c.SubjectAdmissionWindow = SubjectAdmissionWindow
	}
	if c.ShipAdmissionWindow == 0 {
		c.ShipAdmissionWindow = ShipAdmissionWindow
	}
	if c.TargetAdmissionWindow == 0 {
		c.TargetAdmissionWindow = TargetAdmissionWindow
	}
	return c
}

type Authority interface {
	AuthenticateOperator(string, string, string, string) (fleetidentity.OperatorPrincipal, error)
	AuthenticateOperatorCredential(string, string) (fleetidentity.OperatorPrincipal, error)
	InspectOperator(string, uint64) (fleetidentity.OperatorCredentialRecord, error)
}
type SubmitV1 struct {
	SchemaVersion        uint64 `json:"schema_version"`
	FleetID              string `json:"fleet_id"`
	FleetEpoch           uint64 `json:"fleet_epoch"`
	ShipID               string `json:"ship_id"`
	ConnectionGeneration uint64 `json:"connection_generation"`
	Persona              string `json:"persona"`
	InterruptTargetRef   string `json:"interrupt_target_ref"`
	OperationID          string `json:"operation_id,omitempty"`
}
type DeliveryV1 struct {
	Operator fleetidentity.OperatorPrincipal    `json:"operator"`
	Request  livesession.RemoteInterruptRequest `json:"request"`
}
type Endpoint interface {
	DeliverInterrupt(context.Context, DeliveryV1) livesession.RemoteInterruptResult
}
type TargetInstallV1 struct {
	FleetID              string    `json:"fleet_id"`
	FleetEpoch           uint64    `json:"fleet_epoch"`
	ShipID               string    `json:"ship_id"`
	ConnectionGeneration uint64    `json:"connection_generation"`
	Persona              string    `json:"persona"`
	InterruptTargetRef   string    `json:"interrupt_target_ref"`
	ExpiresAt            time.Time `json:"expires_at"`
}
type TargetInstaller interface {
	InstallInterruptTarget(context.Context, TargetInstallV1) error
}
type InterruptTargetV1 struct {
	FleetID              string    `json:"fleet_id"`
	FleetEpoch           uint64    `json:"fleet_epoch"`
	ShipID               string    `json:"ship_id"`
	ConnectionGeneration uint64    `json:"connection_generation"`
	Persona              string    `json:"persona"`
	InterruptTargetRef   string    `json:"interrupt_target_ref"`
	ExpiresAt            time.Time `json:"expires_at"`
}
type TargetsV1 struct {
	SchemaVersion     uint64              `json:"schema_version"`
	ObservationEpoch  string              `json:"observation_epoch"`
	ObservationCursor uint64              `json:"observation_cursor"`
	Targets           []InterruptTargetV1 `json:"targets"`
}

// TargetDiagnostics contains aggregate, test-safe discovery branch counts.
// It deliberately contains no ship, persona, target, credential, or error data.
type TargetDiagnostics struct {
	MissingConnection  uint64
	GenerationMismatch uint64
	NotTargetInstaller uint64
	InstallFailure     uint64
}
type connection struct {
	generation uint64
	endpoint   Endpoint
}
type Service struct {
	mu                     sync.Mutex
	fleetID                string
	authority              Authority
	random                 io.Reader
	connections            map[string]connection
	now                    func() time.Time
	admissionNow           func() time.Time
	targetLifetime         time.Duration
	observationFreshness   time.Duration
	subjectAdmissionWindow time.Duration
	shipAdmissionWindow    time.Duration
	targetAdmissionWindow  time.Duration
	operations             map[string]*fleetOperation
	inflight               map[string]string
	subjectHits            map[string][]time.Time
	shipHits               map[string][]time.Time
	targetHits             map[string][]time.Time
	waiters                map[string]int
	storeDir               string
	audit                  *appendAudit
	persistFailure         func() error
	projection             *fleetobserve.Projection
	epoch                  uint64
	targetDiagnostics      TargetDiagnostics
}

func (s *Service) BindProjection(p *fleetobserve.Projection, epoch uint64) error {
	if p == nil || epoch == 0 {
		return errors.New("invalid_request")
	}
	s.mu.Lock()
	s.projection, s.epoch = p, epoch
	s.mu.Unlock()
	return nil
}

func (s *Service) Targets(ctx context.Context, p fleetidentity.OperatorPrincipal, observationEpoch string, cursor uint64) TargetsV1 {
	out := TargetsV1{SchemaVersion: 1, ObservationEpoch: observationEpoch, ObservationCursor: cursor, Targets: []InterruptTargetV1{}}
	if p.Capability != fleetidentity.InterruptTurnCapability {
		return out
	}
	s.mu.Lock()
	projection, epoch := s.projection, s.epoch
	s.mu.Unlock()
	if projection == nil {
		return out
	}
	snap := projection.Snapshot()
	out.ObservationEpoch, out.ObservationCursor = snap.FleetEpoch, snap.SnapshotCursor
	if observationEpoch != "" && (observationEpoch != snap.FleetEpoch || cursor != snap.SnapshotCursor) {
		return out
	}
	now := s.now()
	for _, sh := range snap.Ships {
		if sh.Connectivity != fleetobserve.Online || sh.ConnectionGeneration == 0 || !principalHasShip(p, sh.ShipID) || sh.LastObservedAt == nil || snap.GeneratedAt.Sub(*sh.LastObservedAt) >= s.observationFreshness {
			continue
		}
		s.mu.Lock()
		c, ok := s.connections[sh.ShipID]
		s.mu.Unlock()
		if !ok {
			s.mu.Lock()
			s.targetDiagnostics.MissingConnection++
			s.mu.Unlock()
			continue
		}
		if c.generation != sh.ConnectionGeneration {
			s.mu.Lock()
			s.targetDiagnostics.GenerationMismatch++
			s.mu.Unlock()
			continue
		}
		installer, installable := c.endpoint.(TargetInstaller)
		if !installable {
			s.mu.Lock()
			s.targetDiagnostics.NotTargetInstaller++
			s.mu.Unlock()
			continue
		}
		for _, ps := range sh.Personas {
			if ps.Session != fleetobserve.SessionWorking || ps.Turn != fleetobserve.TurnActive || ps.ApprovalWaiting {
				continue
			}
			ref, err := randomID(s.random, 32)
			if err != nil {
				return TargetsV1{SchemaVersion: 1, ObservationEpoch: snap.FleetEpoch, ObservationCursor: snap.SnapshotCursor, Targets: []InterruptTargetV1{}}
			}
			t := InterruptTargetV1{FleetID: s.fleetID, FleetEpoch: epoch, ShipID: sh.ShipID, ConnectionGeneration: sh.ConnectionGeneration, Persona: ps.Persona, InterruptTargetRef: ref, ExpiresAt: now.Add(s.targetLifetime)}
			if err := installer.InstallInterruptTarget(ctx, TargetInstallV1(t)); err != nil {
				s.mu.Lock()
				s.targetDiagnostics.InstallFailure++
				s.mu.Unlock()
				continue
			}
			out.Targets = append(out.Targets, t)
		}
	}
	return out
}

// TargetDiagnosticsForTest returns aggregate counters intended only for
// deterministic tests and failure diagnostics.
func (s *Service) TargetDiagnosticsForTest() TargetDiagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targetDiagnostics
}

type fleetOperation struct {
	fingerprint [32]byte
	principal   fleetidentity.OperatorPrincipal
	input       SubmitV1
	nonce       string
	result      livesession.RemoteInterruptResult
	done        chan struct{}
	finished    bool
	created     time.Time
}

func NewService(fleetID string, authority Authority, random io.Reader) (*Service, error) {
	return NewServiceWithConfig(fleetID, authority, random, ServiceConfig{})
}

func NewServiceWithConfig(fleetID string, authority Authority, random io.Reader, config ServiceConfig) (*Service, error) {
	if fleetID == "" || authority == nil {
		return nil, errors.New("invalid_request")
	}
	config = normalizeServiceConfig(config)
	if config.TargetLifetime <= 0 || config.ObservationFreshness <= 0 || config.SubjectAdmissionWindow <= 0 || config.ShipAdmissionWindow <= 0 || config.TargetAdmissionWindow <= 0 {
		return nil, errors.New("invalid_request")
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{fleetID: fleetID, authority: authority, random: random, now: config.Clock.Now, admissionNow: config.AdmissionClock.Now, targetLifetime: config.TargetLifetime, observationFreshness: config.ObservationFreshness, subjectAdmissionWindow: config.SubjectAdmissionWindow, shipAdmissionWindow: config.ShipAdmissionWindow, targetAdmissionWindow: config.TargetAdmissionWindow, connections: map[string]connection{}, operations: map[string]*fleetOperation{}, inflight: map[string]string{}, subjectHits: map[string][]time.Time{}, shipHits: map[string][]time.Time{}, targetHits: map[string][]time.Time{}, waiters: map[string]int{}}, nil
}
func (s *Service) Connect(shipID string, generation uint64, endpoint Endpoint) (func(), error) {
	if shipID == "" || generation == 0 || endpoint == nil {
		return nil, errors.New("invalid_request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.connections[shipID]; ok && old.generation >= generation {
		return nil, errors.New("stale_generation")
	}
	s.connections[shipID] = connection{generation, endpoint}
	return func() {
		s.mu.Lock()
		if x, ok := s.connections[shipID]; ok && x.generation == generation {
			delete(s.connections, shipID)
		}
		s.mu.Unlock()
	}, nil
}
func (s *Service) Submit(ctx context.Context, credentialID, secret string, in SubmitV1) livesession.RemoteInterruptResult {
	p, err := s.Authenticate(credentialID, secret)
	if err != nil {
		return result("", livesession.RemoteInterruptRefused, "unauthorized")
	}
	return s.SubmitAuthorized(ctx, p, in)
}

// Authenticate validates an interrupt credential without consulting any
// request-body field. HTTP callers must complete this step before decoding.
func (s *Service) Authenticate(credentialID, secret string) (fleetidentity.OperatorPrincipal, error) {
	p, err := s.authority.AuthenticateOperatorCredential(credentialID, secret)
	if err != nil || p.Capability != fleetidentity.InterruptTurnCapability {
		return fleetidentity.OperatorPrincipal{}, errors.New("unauthorized")
	}
	return p, nil
}

// SubmitAuthorized accepts only the principal returned by Authenticate and
// rechecks its complete Fleet and ship scope before request validation.
func (s *Service) SubmitAuthorized(ctx context.Context, p fleetidentity.OperatorPrincipal, in SubmitV1) livesession.RemoteInterruptResult {
	if p.Capability != fleetidentity.InterruptTurnCapability || p.FleetID != in.FleetID || !principalHasShip(p, in.ShipID) {
		return result("", livesession.RemoteInterruptRefused, "unauthorized")
	}
	if in.SchemaVersion != 1 || in.FleetID != s.fleetID || in.ShipID == "" || in.FleetEpoch == 0 || in.ConnectionGeneration == 0 || !fleetobserve.ValidPersonaReference(in.Persona) || !validTargetRef(in.InterruptTargetRef) || !validOperationID(in.OperationID) {
		return result("", livesession.RemoteInterruptRefused, "invalid_request")
	}
	opid := in.OperationID
	nonce, err := randomID(s.random, 32)
	if err != nil {
		return result("", livesession.RemoteInterruptIndeterminate, "internal_uncertain")
	}
	fp := fleetFingerprint(p, in)
	now := s.now()
	admissionNow := s.admissionNow()
	targetKey := in.ShipID + "\x00" + in.Persona + "\x00" + in.InterruptTargetRef
	s.mu.Lock()
	s.reclaimLocked(now)
	if old := s.operations[opid]; old != nil {
		if subtle.ConstantTimeCompare(old.fingerprint[:], fp[:]) != 1 {
			s.mu.Unlock()
			return result(opid, livesession.RemoteInterruptRefused, "operation_conflict")
		}
		if !old.finished {
			waitKey := p.SubjectID + "\x00" + in.ShipID + "\x00" + targetKey
			if s.waiters[waitKey] >= MaxFleetWaiters {
				s.mu.Unlock()
				return result(opid, livesession.RemoteInterruptRefused, "busy")
			}
			s.waiters[waitKey]++
			done := old.done
			s.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				s.mu.Lock()
				s.waiters[waitKey]--
				if s.waiters[waitKey] == 0 {
					delete(s.waiters, waitKey)
				}
				s.mu.Unlock()
				return result(opid, livesession.RemoteInterruptIndeterminate, "delivery_unknown")
			}
			s.mu.Lock()
			s.waiters[waitKey]--
			if s.waiters[waitKey] == 0 {
				delete(s.waiters, waitKey)
			}
			r := old.result
			s.mu.Unlock()
			return r
		}
		r := old.result
		s.mu.Unlock()
		return r
	}
	if len(s.operations) >= MaxFleetOperations || s.inflight[targetKey] != "" || !admit(&s.subjectHits, p.SubjectID, admissionNow, s.subjectAdmissionWindow, 5) || !admit(&s.shipHits, in.ShipID, admissionNow, s.shipAdmissionWindow, 2) || !admit(&s.targetHits, targetKey, admissionNow, s.targetAdmissionWindow, 2) {
		s.mu.Unlock()
		return result(opid, livesession.RemoteInterruptRefused, "busy")
	}
	rec := &fleetOperation{fingerprint: fp, principal: p, input: in, nonce: nonce, result: result(opid, livesession.RemoteInterruptAccepted, "authorized"), done: make(chan struct{}), created: now}
	s.operations[opid], s.inflight[targetKey] = rec, opid
	if s.persistLocked() != nil {
		delete(s.operations, opid)
		delete(s.inflight, targetKey)
		s.mu.Unlock()
		return result(opid, livesession.RemoteInterruptRefused, "audit_unavailable")
	}
	if s.audit != nil && s.audit.appendFleet(now, "remote_interrupt.attempted", p, in, opid, "authorized") != nil {
		rec.result = result(opid, livesession.RemoteInterruptRefused, "audit_unavailable")
		rec.finished = true
		delete(s.inflight, targetKey)
		if s.persistLocked() != nil {
			rec.result = result(opid, livesession.RemoteInterruptIndeterminate, "internal_uncertain")
			_ = s.persistLocked()
		}
		close(rec.done)
		r := rec.result
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()
	req := livesession.RemoteInterruptRequest{ProtocolVersion: 1, OperationID: opid, OperationNonce: nonce, FleetID: in.FleetID, FleetEpoch: in.FleetEpoch, ShipID: in.ShipID, ConnectionGeneration: in.ConnectionGeneration, Persona: in.Persona, TargetReference: in.InterruptTargetRef}
	s.mu.Lock()
	c, ok := s.connections[in.ShipID]
	s.mu.Unlock()
	if !ok {
		return s.finish(rec, targetKey, result(opid, livesession.RemoteInterruptRefused, "ship_offline"))
	}
	if c.generation != in.ConnectionGeneration {
		return s.finish(rec, targetKey, result(opid, livesession.RemoteInterruptRefused, "stale_generation"))
	}
	dctx, cancel := context.WithTimeout(ctx, DeliveryDeadline)
	defer cancel()
	done := make(chan livesession.RemoteInterruptResult, 1)
	go func() { done <- c.endpoint.DeliverInterrupt(dctx, DeliveryV1{p, req}) }()
	select {
	case r := <-done:
		return s.finish(rec, targetKey, r)
	case <-dctx.Done():
		return s.finish(rec, targetKey, result(opid, livesession.RemoteInterruptIndeterminate, "delivery_unknown"))
	}
}

func principalHasShip(p fleetidentity.OperatorPrincipal, shipID string) bool {
	for _, allowed := range p.ShipIDs {
		if allowed == shipID {
			return true
		}
	}
	return false
}

func (s *Service) reclaimLocked(now time.Time) {
	for id, r := range s.operations {
		if r.finished && now.Sub(r.created) >= livesession.RemoteInterruptRetention {
			delete(s.operations, id)
		}
	}
	prune := func(m map[string][]time.Time, window time.Duration) {
		for k, xs := range m {
			n := 0
			for _, t := range xs {
				if now.Sub(t) < window {
					xs[n] = t
					n++
				}
			}
			if n == 0 {
				delete(m, k)
			} else {
				m[k] = xs[:n]
			}
		}
	}
	prune(s.subjectHits, s.subjectAdmissionWindow)
	prune(s.shipHits, s.shipAdmissionWindow)
	prune(s.targetHits, s.targetAdmissionWindow)
}

func (s *Service) finish(rec *fleetOperation, key string, r livesession.RemoteInterruptResult) livesession.RemoteInterruptResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !rec.finished {
		// The journal is the publication boundary. Queries and replay waiters are
		// excluded by s.mu until the terminal decision is durable. If that commit
		// is uncertain, retain only an indeterminate result in memory; the prior
		// durable unfinished record also reopens as indeterminate, so restart can
		// never contradict a result returned here.
		rec.result, rec.finished = r, true
		delete(s.inflight, key)
		if s.persistLocked() != nil {
			rec.result = result(rec.input.OperationID, livesession.RemoteInterruptIndeterminate, "internal_uncertain")
			// Best effort makes the precise uncertainty durable. If it also fails,
			// the already-durable unfinished record reopens as fleet_restarted,
			// which has the same truthful replay-only disposition.
			_ = s.persistLocked()
		}
		if s.audit != nil {
			_ = s.audit.appendFleet(s.now(), "remote_interrupt.completed", rec.principal, rec.input, rec.input.OperationID, string(rec.result.Outcome))
		}
		close(rec.done)
	}
	return rec.result
}
func (s *Service) Query(operationID string) (livesession.RemoteInterruptResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.operations[operationID]
	if r == nil {
		return livesession.RemoteInterruptResult{}, false
	}
	return r.result, true
}
func (s *Service) QueryAuthorized(credentialID, secret, operationID string) (livesession.RemoteInterruptResult, bool) {
	s.mu.Lock()
	r := s.operations[operationID]
	s.mu.Unlock()
	if r == nil {
		return livesession.RemoteInterruptResult{}, false
	}
	p, e := s.authority.AuthenticateOperator(credentialID, secret, r.input.FleetID, r.input.ShipID)
	if e != nil || p.SubjectID != r.principal.SubjectID || p.Capability != fleetidentity.InterruptTurnCapability {
		return livesession.RemoteInterruptResult{}, false
	}
	return s.Query(operationID)
}
func admit(m *map[string][]time.Time, key string, now time.Time, window time.Duration, max int) bool {
	xs := (*m)[key]
	n := 0
	for _, t := range xs {
		if now.Sub(t) < window {
			xs[n] = t
			n++
		}
	}
	xs = xs[:n]
	if len(xs) >= max {
		(*m)[key] = xs
		return false
	}
	(*m)[key] = append(xs, now)
	return true
}
func validOperationID(v string) bool {
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32
}
func validTargetRef(v string) bool {
	b, e := base64.RawURLEncoding.DecodeString(v)
	return e == nil && len(b) == 32
}

// NewOperationID returns a caller-owned 256-bit identifier suitable for one
// interrupt submission and its exact replays.
func NewOperationID() (string, error) { return randomID(rand.Reader, 32) }
func fleetFingerprint(p fleetidentity.OperatorPrincipal, in SubmitV1) [32]byte {
	h := sha256.New()
	h.Write([]byte("shipmates.fleet-interrupt.v1\x00"))
	var b [8]byte
	for _, v := range []string{p.SubjectID, p.CredentialID, p.FleetID, p.Capability, in.FleetID, in.ShipID, in.Persona, in.InterruptTargetRef, in.OperationID} {
		binary.BigEndian.PutUint64(b[:], uint64(len(v)))
		h.Write(b[:])
		h.Write([]byte(v))
	}
	for _, v := range []uint64{p.CredentialGeneration, in.SchemaVersion, in.FleetEpoch, in.ConnectionGeneration} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
func randomID(r io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func result(id string, outcome livesession.RemoteInterruptOutcome, reason string) livesession.RemoteInterruptResult {
	retry := livesession.RemoteInterruptNoRetry
	if outcome == livesession.RemoteInterruptAccepted || outcome == livesession.RemoteInterruptIndeterminate {
		retry = livesession.RemoteInterruptReplaySameOperation
	}
	if reason == "stale_generation" || reason == "stale_target" || reason == "target_expired" {
		retry = livesession.RemoteInterruptFreshObservation
	}
	return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: id, Outcome: outcome, ReasonCode: reason, RetryDisposition: retry}
}

type ShipEndpoint struct {
	mu                sync.Mutex
	fleetID, shipID   string
	epoch, generation uint64
	authority         Authority
	trusted           bool
	now               func() time.Time
	coordinator       *livesession.RemoteInterruptCoordinator
	manager           *livesession.Manager
	targets           map[string]livesession.RemoteInterruptTarget
}

func NewShipEndpoint(fleetID, shipID string, epoch, generation uint64, authority Authority, now func() time.Time, c *livesession.RemoteInterruptCoordinator, m *livesession.Manager) (*ShipEndpoint, error) {
	if fleetID == "" || shipID == "" || epoch == 0 || generation == 0 || authority == nil || c == nil || m == nil {
		return nil, errors.New("invalid_request")
	}
	if now == nil {
		now = time.Now
	}
	return &ShipEndpoint{fleetID: fleetID, shipID: shipID, epoch: epoch, generation: generation, authority: authority, now: now, coordinator: c, manager: m, targets: map[string]livesession.RemoteInterruptTarget{}}, nil
}
func (s *ShipEndpoint) InstallCurrentTarget(persona, reference string) error {
	if persona == "" || reference == "" || len(reference) > 128 {
		return errors.New("stale_target")
	}
	session, err := s.manager.Session(persona)
	if err != nil {
		return errors.New("stale_target")
	}
	snap := session.Snapshot()
	if snap.State != livesession.Working && snap.State != livesession.Steering {
		return errors.New("stale_target")
	}
	t := livesession.RemoteInterruptTarget{Reference: reference, FleetID: s.fleetID, FleetEpoch: s.epoch, ShipID: s.shipID, ConnectionGeneration: s.generation, Persona: persona, SessionID: snap.SessionID, Backend: snap.Backend, ThreadID: snap.ThreadID, TurnID: snap.TurnID, ExpiresAt: s.now().Add(TargetLifetime)}
	s.mu.Lock()
	clear(s.targets)
	s.targets[reference] = t
	s.mu.Unlock()
	return nil
}
func (s *ShipEndpoint) InvalidateTargets() { s.mu.Lock(); clear(s.targets); s.mu.Unlock() }
func (s *ShipEndpoint) DeliverInterrupt(ctx context.Context, d DeliveryV1) livesession.RemoteInterruptResult {
	r := d.Request
	if r.FleetID != s.fleetID || r.ShipID != s.shipID || r.FleetEpoch != s.epoch || r.ConnectionGeneration != s.generation || d.Operator.Capability != fleetidentity.InterruptTurnCapability {
		return result(r.OperationID, livesession.RemoteInterruptRefused, "stale_generation")
	}
	rec, err := s.authority.InspectOperator(d.Operator.CredentialID, d.Operator.CredentialGeneration)
	if err != nil || rec.Revoked || rec.Capability != fleetidentity.InterruptTurnCapability || rec.SubjectID != d.Operator.SubjectID || !s.now().Before(rec.ExpiresAt) || !contains(rec.ShipIDs, s.shipID) {
		return result(r.OperationID, livesession.RemoteInterruptRefused, "credential_revoked")
	}
	s.mu.Lock()
	target, ok := s.targets[r.TargetReference]
	s.mu.Unlock()
	if !ok {
		return result(r.OperationID, livesession.RemoteInterruptRefused, "stale_target")
	}
	op := livesession.RemoteInterruptOperator{SubjectID: d.Operator.SubjectID, CredentialID: d.Operator.CredentialID, FleetID: d.Operator.FleetID, CredentialGeneration: d.Operator.CredentialGeneration, Capability: d.Operator.Capability}
	out, _ := s.coordinator.Interrupt(ctx, op, r, target, s.manager, s.manager)
	return out
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
