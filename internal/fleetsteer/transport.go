// Package fleetsteer implements the closed M8 operator-to-one-ship steering
// transport. It is not an RPC framework and exposes no other remote action.
package fleetsteer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
)

const (
	DeliveryDeadline = 5 * time.Second
	TotalDeadline    = 12 * time.Second
	TargetLifetime   = 60 * time.Second
)

type Clock interface{ Now() time.Time }
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type Authority interface {
	AuthenticateOperator(credentialID, secret, fleetID, shipID string) (fleetidentity.OperatorPrincipal, error)
	InspectOperator(credentialID string, generation uint64) (fleetidentity.OperatorCredentialRecord, error)
}

type SubmitV1 struct {
	SchemaVersion        uint64 `json:"schema_version"`
	FleetID              string `json:"fleet_id"`
	FleetEpoch           uint64 `json:"fleet_epoch"`
	ShipID               string `json:"ship_id"`
	ConnectionGeneration uint64 `json:"connection_generation"`
	Persona              string `json:"persona"`
	SteerTargetRef       string `json:"steer_target_ref"`
	Message              string `json:"message"`
}

type DeliveryV1 struct {
	Operator fleetidentity.OperatorPrincipal
	Request  livesession.RemoteSteerRequest
}
type deliveryV1 = DeliveryV1

type Endpoint interface {
	Deliver(context.Context, DeliveryV1) livesession.RemoteSteerResult
}

// SteerTargetV1 is the Fleet-visible exact-turn projection. The ship retains
// the private tuple behind Reference and resolves it again before delivery.
type SteerTargetV1 struct {
	FleetID              string    `json:"fleet_id"`
	FleetEpoch           uint64    `json:"fleet_epoch"`
	ShipID               string    `json:"ship_id"`
	ConnectionGeneration uint64    `json:"connection_generation"`
	Persona              string    `json:"persona"`
	SteerTargetRef       string    `json:"steer_target_ref"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type SteerTargetsV1 struct {
	SchemaVersion     uint64          `json:"schema_version"`
	ObservationEpoch  string          `json:"observation_epoch"`
	ObservationCursor uint64          `json:"observation_cursor"`
	Targets           []SteerTargetV1 `json:"targets"`
}

// TargetProvider is implemented by the authenticated reverse endpoint. It
// exposes only the already-installed opaque target projection.
type TargetProvider interface {
	PublicTargets(context.Context) ([]SteerTargetV1, error)
}

type connection struct {
	generation uint64
	endpoint   Endpoint
}

type Service struct {
	mu          sync.Mutex
	fleetID     string
	authority   Authority
	clock       Clock
	random      io.Reader
	connections map[string]connection
	changed     chan struct{}
	audit       AuditSink
	projection  *fleetobserve.Projection
	epoch       uint64
}

// AuditRecordV1 is a closed redacted projection. It intentionally has no
// message, nonce, target reference, credential secret, or private turn tuple.
type AuditRecordV1 struct {
	SchemaVersion                uint64                         `json:"schema_version"`
	EventKind                    string                         `json:"event_kind"`
	OperationID                  string                         `json:"operation_id"`
	ActorSubjectID               string                         `json:"actor_subject_id"`
	OperatorCredentialID         string                         `json:"operator_credential_id"`
	OperatorCredentialGeneration uint64                         `json:"operator_credential_generation"`
	Capability                   string                         `json:"capability"`
	FleetID                      string                         `json:"fleet_id"`
	ShipID                       string                         `json:"ship_id"`
	Persona                      string                         `json:"persona"`
	MessageSHA256                string                         `json:"message_sha256"`
	FleetEpoch                   uint64                         `json:"fleet_epoch"`
	ConnectionGeneration         uint64                         `json:"connection_generation"`
	MessageUTF8Bytes             uint64                         `json:"message_utf8_bytes"`
	Outcome                      livesession.RemoteSteerOutcome `json:"outcome,omitempty"`
	ReasonCode                   string                         `json:"reason_code,omitempty"`
	Layer                        string                         `json:"layer"`
}
type AuditSink interface{ AppendRemoteSteer(AuditRecordV1) error }

func (s *Service) SetAuditSink(a AuditSink) { s.mu.Lock(); s.audit = a; s.mu.Unlock() }

func (s *Service) BindProjection(p *fleetobserve.Projection, epoch uint64) error {
	if p == nil || epoch == 0 {
		return errors.New("invalid_request")
	}
	s.mu.Lock()
	s.projection, s.epoch = p, epoch
	s.mu.Unlock()
	return nil
}

// Targets returns only fresh, ship-installed opaque targets for the exact
// authorized capability. Authentication and capability checks happen before
// consulting the projection or any connection.
func (s *Service) Targets(ctx context.Context, p fleetidentity.OperatorPrincipal, observationEpoch string, cursor uint64) SteerTargetsV1 {
	out := SteerTargetsV1{SchemaVersion: 1, ObservationEpoch: observationEpoch, ObservationCursor: cursor, Targets: []SteerTargetV1{}}
	if p.Capability != livesession.RemoteSteerCapability {
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
	for _, sh := range snap.Ships {
		if sh.Connectivity != fleetobserve.Online || sh.ConnectionGeneration == 0 || !principalHasShip(p, sh.ShipID) || sh.LastObservedAt == nil || snap.GeneratedAt.Sub(*sh.LastObservedAt) >= fleetinterrupt.ObservationFreshness {
			continue
		}
		s.mu.Lock()
		c, ok := s.connections[sh.ShipID]
		s.mu.Unlock()
		if !ok || c.generation != sh.ConnectionGeneration {
			continue
		}
		provider, ok := c.endpoint.(TargetProvider)
		if !ok {
			continue
		}
		targets, err := provider.PublicTargets(ctx)
		if err != nil || len(targets) > 256 {
			continue
		}
		now := s.clock.Now()
		for _, target := range targets {
			if target.FleetID == s.fleetID && target.FleetEpoch == epoch && target.ShipID == sh.ShipID && target.ConnectionGeneration == sh.ConnectionGeneration && target.SteerTargetRef != "" && target.ExpiresAt.After(now) && !target.ExpiresAt.After(now.Add(TargetLifetime)) {
				out.Targets = append(out.Targets, target)
			}
		}
	}
	return out
}

func NewService(fleetID string, authority Authority, clock Clock, random io.Reader) (*Service, error) {
	if fleetID == "" || authority == nil {
		return nil, errors.New("invalid_request")
	}
	if clock == nil {
		clock = wallClock{}
	}
	if random == nil {
		random = rand.Reader
	}
	return &Service{fleetID: fleetID, authority: authority, clock: clock, random: random, connections: map[string]connection{}, changed: make(chan struct{})}, nil
}

func (s *Service) signalConnectionChangeLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// WaitConnected waits for the exact authenticated reverse-tunnel generation.
// It is edge-safe: callers inspect state and subscribe under the same lock.
func (s *Service) WaitConnected(ctx context.Context, shipID string, generation uint64) error {
	if shipID == "" || generation == 0 {
		return errors.New("invalid_request")
	}
	for {
		s.mu.Lock()
		c, ok := s.connections[shipID]
		changed := s.changed
		s.mu.Unlock()
		if ok && c.generation == generation {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Connect installs exactly one current authenticated ship generation.
func (s *Service) Connect(shipID string, generation uint64, endpoint Endpoint) (func(), error) {
	if shipID == "" || generation == 0 || endpoint == nil {
		return nil, errors.New("invalid_request")
	}
	s.mu.Lock()
	if old, ok := s.connections[shipID]; ok && old.generation >= generation {
		s.mu.Unlock()
		return nil, errors.New("stale_generation")
	}
	s.connections[shipID] = connection{generation, endpoint}
	s.signalConnectionChangeLocked()
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if c, ok := s.connections[shipID]; ok && c.generation == generation {
			delete(s.connections, shipID)
			s.signalConnectionChangeLocked()
		}
		s.mu.Unlock()
	}, nil
}

func (s *Service) Submit(ctx context.Context, credentialID, secret string, in SubmitV1) livesession.RemoteSteerResult {
	if in.SchemaVersion != 1 {
		return result("", "refused", "unsupported_version")
	}
	if in.FleetID != s.fleetID || in.ShipID == "" {
		return result("", "refused", "wrong_scope")
	}
	principal, err := s.authority.AuthenticateOperator(credentialID, secret, in.FleetID, in.ShipID)
	if err != nil || principal.Capability != livesession.RemoteSteerCapability {
		return result("", "refused", "unauthorized")
	}
	if !validSubmit(in) {
		return result("", "refused", "invalid_request")
	}
	opid, err1 := randomID(s.random, 16)
	nonce, err2 := randomID(s.random, 32)
	if err1 != nil || err2 != nil {
		return result("", "indeterminate", "internal_uncertain")
	}
	digest := sha256.Sum256([]byte(in.Message))
	req := livesession.RemoteSteerRequest{ProtocolVersion: 1, OperationID: opid, OperationNonce: nonce, FleetID: in.FleetID, ShipID: in.ShipID, Persona: in.Persona, TargetReference: in.SteerTargetRef, FleetEpoch: in.FleetEpoch, ConnectionGeneration: in.ConnectionGeneration, Message: in.Message, MessageSHA256: hex.EncodeToString(digest[:]), MessageUTF8Bytes: uint64(len([]byte(in.Message)))}
	s.mu.Lock()
	c, ok := s.connections[in.ShipID]
	audit := s.audit
	s.mu.Unlock()
	base := AuditRecordV1{SchemaVersion: 1, EventKind: "remote_steer.attempted", OperationID: opid, ActorSubjectID: principal.SubjectID, OperatorCredentialID: principal.CredentialID, OperatorCredentialGeneration: principal.CredentialGeneration, Capability: principal.Capability, FleetID: in.FleetID, ShipID: in.ShipID, Persona: in.Persona, MessageSHA256: req.MessageSHA256, FleetEpoch: in.FleetEpoch, ConnectionGeneration: in.ConnectionGeneration, MessageUTF8Bytes: req.MessageUTF8Bytes, Layer: "fleet"}
	if audit != nil && audit.AppendRemoteSteer(base) != nil {
		return result(opid, "refused", "shutdown")
	}
	if !ok {
		return s.audited(audit, base, result(opid, "refused", "ship_offline"))
	}
	if c.generation != in.ConnectionGeneration {
		return s.audited(audit, base, result(opid, "refused", "stale_generation"))
	}
	dctx, cancel := context.WithTimeout(ctx, DeliveryDeadline)
	defer cancel()
	done := make(chan livesession.RemoteSteerResult, 1)
	go func() { done <- c.endpoint.Deliver(dctx, DeliveryV1{principal, req}) }()
	select {
	case r := <-done:
		return s.audited(audit, base, r)
	case <-dctx.Done():
		return s.audited(audit, base, result(opid, "indeterminate", "delivery_unknown"))
	}
}

func (s *Service) audited(a AuditSink, rec AuditRecordV1, r livesession.RemoteSteerResult) livesession.RemoteSteerResult {
	if a != nil {
		rec.EventKind = "remote_steer." + string(r.Outcome)
		rec.Outcome, rec.ReasonCode = r.Outcome, r.ReasonCode
		_ = a.AppendRemoteSteer(rec)
	}
	return r
}

func validSubmit(in SubmitV1) bool {
	if in.FleetEpoch == 0 || in.ConnectionGeneration == 0 || in.SteerTargetRef == "" || len(in.SteerTargetRef) > 128 || project.ValidatePersonaName(in.Persona) != nil || !utf8.ValidString(in.Message) || len([]byte(in.Message)) == 0 || len([]byte(in.Message)) > 4096 {
		return false
	}
	for _, r := range in.Message {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\t') || r == '\u202a' || r == '\u202b' || r == '\u202c' || r == '\u202d' || r == '\u202e' || (r >= '\u2066' && r <= '\u2069') {
			return false
		}
	}
	return strings.TrimSpace(in.Message) != ""
}

func principalHasShip(p fleetidentity.OperatorPrincipal, shipID string) bool {
	for _, id := range p.ShipIDs {
		if id == shipID {
			return true
		}
	}
	return false
}

func randomID(r io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func result(id, outcome, reason string) livesession.RemoteSteerResult {
	r := livesession.RemoteSteerNoRetry
	if outcome == "indeterminate" {
		r = livesession.RemoteSteerReplaySameOperation
	}
	if reason == "stale_generation" || reason == "stale_target" || reason == "target_expired" {
		r = livesession.RemoteSteerFreshObservation
	}
	return livesession.RemoteSteerResult{SchemaVersion: 1, OperationID: id, Outcome: livesession.RemoteSteerOutcome(outcome), ReasonCode: reason, RetryDisposition: r}
}

// ShipEndpoint retains private targets and independently revalidates current
// operator generation/revocation immediately before entering the coordinator.
type ShipEndpoint struct {
	mu                      sync.Mutex
	fleetID, shipID         string
	fleetEpoch, generation  uint64
	authority               Authority
	trustedFleet            bool
	steerClock              Clock
	interruptClock          Clock
	coordinator             *livesession.RemoteSteerCoordinator
	manager                 *livesession.Manager
	localControl            *LocalControl
	targets                 map[string]livesession.RemoteSteerTarget
	interruptTargets        map[string]livesession.RemoteInterruptTarget
	interruptPersonaAliases map[string]string
	interruptPublicPersonas map[string]string
	interruptRandom         io.Reader
}

type ShipEndpointConfig struct {
	SteerClock     Clock
	InterruptClock Clock
}

func (c ShipEndpointConfig) clocks() (Clock, Clock) {
	steer, interrupt := c.SteerClock, c.InterruptClock
	if steer == nil {
		steer = wallClock{}
	}
	if interrupt == nil {
		interrupt = wallClock{}
	}
	return steer, interrupt
}

func NewProductionShipEndpoint(fleetID, shipID string, epoch, generation uint64, config ShipEndpointConfig, control *LocalControl) (*ShipEndpoint, error) {
	if fleetID == "" || shipID == "" || epoch == 0 || generation == 0 || control == nil {
		return nil, errors.New("invalid_request")
	}
	steerClock, interruptClock := config.clocks()
	return &ShipEndpoint{fleetID: fleetID, shipID: shipID, fleetEpoch: epoch, generation: generation, authority: authenticatedFleetAuthority{}, trustedFleet: true, steerClock: steerClock, interruptClock: interruptClock, localControl: control, targets: map[string]livesession.RemoteSteerTarget{}, interruptTargets: map[string]livesession.RemoteInterruptTarget{}, interruptPersonaAliases: map[string]string{}, interruptPublicPersonas: map[string]string{}, interruptRandom: rand.Reader}, nil
}

// NewAuthenticatedShipEndpoint is for the ship side of the mutually
// authenticated production tunnel. Operator authentication is performed by
// Fleet immediately before the closed delivery is sent; the ship still owns
// target and exact-turn validation.
func NewAuthenticatedShipEndpoint(fleetID, shipID string, epoch, generation uint64, clock Clock, coordinator *livesession.RemoteSteerCoordinator, manager *livesession.Manager) (*ShipEndpoint, error) {
	s, err := NewShipEndpoint(fleetID, shipID, epoch, generation, authenticatedFleetAuthority{}, clock, coordinator, manager)
	if err == nil {
		s.trustedFleet = true
	}
	return s, err
}

type authenticatedFleetAuthority struct{}

func (authenticatedFleetAuthority) AuthenticateOperator(string, string, string, string) (fleetidentity.OperatorPrincipal, error) {
	return fleetidentity.OperatorPrincipal{}, errors.New("unsupported")
}
func (authenticatedFleetAuthority) InspectOperator(string, uint64) (fleetidentity.OperatorCredentialRecord, error) {
	return fleetidentity.OperatorCredentialRecord{}, errors.New("unsupported")
}

func NewShipEndpoint(fleetID, shipID string, epoch, generation uint64, authority Authority, clock Clock, coordinator *livesession.RemoteSteerCoordinator, manager *livesession.Manager) (*ShipEndpoint, error) {
	if fleetID == "" || shipID == "" || epoch == 0 || generation == 0 || authority == nil || coordinator == nil || manager == nil {
		return nil, errors.New("invalid_request")
	}
	if clock == nil {
		clock = wallClock{}
	}
	return &ShipEndpoint{fleetID: fleetID, shipID: shipID, fleetEpoch: epoch, generation: generation, authority: authority, steerClock: clock, interruptClock: clock, coordinator: coordinator, manager: manager, targets: map[string]livesession.RemoteSteerTarget{}, interruptTargets: map[string]livesession.RemoteInterruptTarget{}, interruptPersonaAliases: map[string]string{}, interruptPublicPersonas: map[string]string{}, interruptRandom: rand.Reader}, nil
}

func (s *ShipEndpoint) InstallTarget(t livesession.RemoteSteerTarget) error {
	now := s.steerClock.Now()
	if t.FleetID != s.fleetID || t.ShipID != s.shipID || t.FleetEpoch != s.fleetEpoch || t.ConnectionGeneration != s.generation || t.Reference == "" || !now.Before(t.ExpiresAt) || t.ExpiresAt.After(now.Add(TargetLifetime)) {
		return errors.New("stale_target")
	}
	interruptRef, err := randomID(s.interruptRandom, 32)
	if err != nil {
		s.InvalidateTargets()
		return errors.New("interrupt_target_unavailable")
	}
	s.mu.Lock()
	clear(s.targets)
	s.targets[t.Reference] = t
	clear(s.interruptTargets)
	s.interruptTargets[interruptRef] = livesession.RemoteInterruptTarget{Reference: interruptRef, FleetID: t.FleetID, FleetEpoch: t.FleetEpoch, ShipID: t.ShipID, ConnectionGeneration: t.ConnectionGeneration, Persona: t.Persona, SessionID: t.SessionID, Backend: t.Backend, ThreadID: t.ThreadID, TurnID: t.TurnID, ExpiresAt: t.ExpiresAt}
	clear(s.interruptPersonaAliases)
	clear(s.interruptPublicPersonas)
	s.mu.Unlock()
	return nil
}

// InstallTargetsAndInterruptPersonaAliases validates and commits the complete
// private exact-turn target projection and its Fleet-visible persona aliases as
// one transaction. Fleet interrupt installation can therefore never observe a
// target projection without the aliases belonging to that same projection.
func (s *ShipEndpoint) InstallTargetsAndInterruptPersonaAliases(targets []livesession.RemoteSteerTarget, aliases map[string]string) error {
	s.mu.Lock()
	now := s.steerClock.Now()
	next := make(map[string]livesession.RemoteSteerTarget, len(targets))
	personas := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if t.FleetID != s.fleetID || t.ShipID != s.shipID || t.FleetEpoch != s.fleetEpoch || t.ConnectionGeneration != s.generation || t.Reference == "" || !now.Before(t.ExpiresAt) || t.ExpiresAt.After(now.Add(TargetLifetime)) {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("stale_target")
		}
		if _, exists := next[t.Reference]; exists {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("stale_target")
		}
		next[t.Reference] = t
		personas[t.Persona] = struct{}{}
	}
	nextAliases := make(map[string]string, len(aliases))
	aliasedPersonas := make(map[string]struct{}, len(aliases))
	for opaque, private := range aliases {
		if !fleetobserve.ValidPersonaReference(opaque) || project.ValidatePersonaName(private) != nil {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("stale_target")
		}
		if _, ok := personas[private]; !ok {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("stale_target")
		}
		if _, duplicate := aliasedPersonas[private]; duplicate {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("stale_target")
		}
		nextAliases[opaque] = private
		aliasedPersonas[private] = struct{}{}
	}
	if len(aliasedPersonas) != len(personas) {
		s.invalidateTargetsLocked()
		s.mu.Unlock()
		return errors.New("stale_target")
	}
	// Interrupt authority is never derived from, equal to, or published with an
	// M7/M8 steering reference. Mint one independent 256-bit capability for
	// each exact live tuple installed on this ship generation.
	interrupts := make(map[string]livesession.RemoteInterruptTarget, len(next))
	for _, t := range next {
		ref, err := randomID(s.interruptRandom, 32)
		if err != nil {
			s.invalidateTargetsLocked()
			s.mu.Unlock()
			return errors.New("interrupt_target_unavailable")
		}
		interrupts[ref] = livesession.RemoteInterruptTarget{Reference: ref, FleetID: t.FleetID, FleetEpoch: t.FleetEpoch, ShipID: t.ShipID, ConnectionGeneration: t.ConnectionGeneration, Persona: t.Persona, SessionID: t.SessionID, Backend: t.Backend, ThreadID: t.ThreadID, TurnID: t.TurnID, ExpiresAt: t.ExpiresAt}
	}
	s.targets = next
	s.interruptTargets = interrupts
	s.interruptPersonaAliases = nextAliases
	s.interruptPublicPersonas = map[string]string{}
	s.mu.Unlock()
	return nil
}

// InstallCurrentTarget derives the private tuple exclusively from the local
// live-session manager. Callers provide only the public opaque reference; they
// cannot inject session, backend, thread, or turn identifiers.
func (s *ShipEndpoint) InstallCurrentTarget(persona, reference string) error {
	if project.ValidatePersonaName(persona) != nil || reference == "" || len(reference) > 128 {
		s.InvalidateTargets()
		return errors.New("stale_target")
	}
	session, err := s.manager.Session(persona)
	if err != nil {
		s.InvalidateTargets()
		return errors.New("stale_target")
	}
	snap := session.Snapshot()
	if snap.State != livesession.Working || snap.SessionID == "" || snap.ThreadID == "" || snap.TurnID == "" {
		s.InvalidateTargets()
		return errors.New("stale_target")
	}
	return s.InstallTarget(livesession.RemoteSteerTarget{Reference: reference, FleetID: s.fleetID, FleetEpoch: s.fleetEpoch, ShipID: s.shipID, ConnectionGeneration: s.generation, Persona: persona, SessionID: snap.SessionID, Backend: snap.Backend, ThreadID: snap.ThreadID, TurnID: snap.TurnID, ExpiresAt: s.steerClock.Now().Add(TargetLifetime)})
}

// CurrentTargetReference is the opaque, exact-turn reference used by the
// production ship runtime and exposed observation/UI layers.
func CurrentTargetReference(persona string, snap livesession.Snapshot) string {
	h := sha256.New()
	_, _ = h.Write([]byte("shipmates/fleet-steer-target/v1\x00"))
	for _, v := range []string{persona, snap.SessionID, snap.ThreadID, snap.TurnID} {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	return "trg_" + hex.EncodeToString(h.Sum(nil)[:16])
}

// InvalidateTargets is called on any local turn/session/approval/generation
// transition. A later steer can never fall forward to the current turn.
func (s *ShipEndpoint) InvalidateTargets() {
	s.mu.Lock()
	s.invalidateTargetsLocked()
	s.mu.Unlock()
}

func (s *ShipEndpoint) PublicTargets(ctx context.Context) ([]SteerTargetV1, error) {
	if ctx == nil {
		return nil, errors.New("invalid_request")
	}
	now := s.steerClock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SteerTargetV1, 0, len(s.targets))
	for _, t := range s.targets {
		if !now.Before(t.ExpiresAt) {
			continue
		}
		out = append(out, SteerTargetV1{FleetID: t.FleetID, FleetEpoch: t.FleetEpoch, ShipID: t.ShipID, ConnectionGeneration: t.ConnectionGeneration, Persona: t.Persona, SteerTargetRef: t.Reference, ExpiresAt: t.ExpiresAt})
	}
	return out, nil
}

func (s *ShipEndpoint) invalidateTargetsLocked() {
	clear(s.targets)
	clear(s.interruptTargets)
	clear(s.interruptPersonaAliases)
	clear(s.interruptPublicPersonas)
}

func (s *ShipEndpoint) Deliver(ctx context.Context, d DeliveryV1) livesession.RemoteSteerResult {
	r := d.Request
	if r.FleetID != s.fleetID || r.ShipID != s.shipID || r.FleetEpoch != s.fleetEpoch || r.ConnectionGeneration != s.generation {
		return result(r.OperationID, "refused", "stale_generation")
	}
	rec, err := s.authority.InspectOperator(d.Operator.CredentialID, d.Operator.CredentialGeneration)
	if !s.trustedFleet && (err != nil || rec.Revoked || rec.FleetID != s.fleetID || rec.SubjectID != d.Operator.SubjectID || rec.Capability != livesession.RemoteSteerCapability || !s.steerClock.Now().Before(rec.ExpiresAt) || !contains(rec.ShipIDs, s.shipID)) {
		return result(r.OperationID, "refused", "credential_revoked")
	}
	s.mu.Lock()
	target, ok := s.targets[r.TargetReference]
	s.mu.Unlock()
	if !ok {
		return result(r.OperationID, "refused", "stale_target")
	}
	op := livesession.RemoteSteerOperator{SubjectID: d.Operator.SubjectID, CredentialID: d.Operator.CredentialID, FleetID: d.Operator.FleetID, CredentialGeneration: d.Operator.CredentialGeneration, Capability: d.Operator.Capability}
	if s.localControl != nil {
		return s.localControl.Steer(ctx, op, r, target)
	}
	res, _ := s.coordinator.Steer(ctx, op, r, target, s.manager)
	return res
}

// DeliverInterrupt implements the distinct closed M9 reverse operation. It
// reuses only the exact private target tuple already installed by local
// discovery; it does not widen steering authority.
func (s *ShipEndpoint) DeliverInterrupt(ctx context.Context, d fleetinterrupt.DeliveryV1) livesession.RemoteInterruptResult {
	r := d.Request
	if r.FleetID != s.fleetID || r.ShipID != s.shipID || r.FleetEpoch != s.fleetEpoch || r.ConnectionGeneration != s.generation || d.Operator.Capability != fleetidentity.InterruptTurnCapability {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: r.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "stale_generation", RetryDisposition: livesession.RemoteInterruptFreshObservation}
	}
	s.mu.Lock()
	t, ok := s.interruptTargets[r.TargetReference]
	publicPersona := s.interruptPublicPersonas[r.TargetReference]
	s.mu.Unlock()
	if !ok || publicPersona == "" || publicPersona != r.Persona {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: r.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "stale_target", RetryDisposition: livesession.RemoteInterruptFreshObservation}
	}
	if !s.interruptClock.Now().Before(t.ExpiresAt) {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: r.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "target_expired", RetryDisposition: livesession.RemoteInterruptFreshObservation}
	}
	target := t
	// Fleet authenticates and compares the opaque reference. Only after that
	// exact public-target check does the authoritative ship-side table resolve
	// it to the private persona used by the local coordinator and manager.
	r.Persona = target.Persona
	op := livesession.RemoteInterruptOperator{SubjectID: d.Operator.SubjectID, CredentialID: d.Operator.CredentialID, FleetID: d.Operator.FleetID, CredentialGeneration: d.Operator.CredentialGeneration, Capability: d.Operator.Capability}
	if s.localControl != nil {
		return s.localControl.Interrupt(ctx, op, r, target)
	}
	return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: r.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "shutdown", RetryDisposition: livesession.RemoteInterruptNoRetry}
}

func (s *ShipEndpoint) InstallInterruptTarget(_ context.Context, in fleetinterrupt.TargetInstallV1) error {
	if in.FleetID != s.fleetID || in.FleetEpoch != s.fleetEpoch || in.ShipID != s.shipID || in.ConnectionGeneration != s.generation || !fleetobserve.ValidPersonaReference(in.Persona) || !s.interruptClock.Now().Before(in.ExpiresAt) {
		return errors.New("stale_target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	privatePersona, ok := s.interruptPersonaAliases[in.Persona]
	if !ok {
		return errors.New("stale_target")
	}
	for _, t := range s.targets {
		if t.Persona == privatePersona {
			clear(s.interruptTargets)
			clear(s.interruptPublicPersonas)
			s.interruptTargets[in.InterruptTargetRef] = livesession.RemoteInterruptTarget{Reference: in.InterruptTargetRef, FleetID: t.FleetID, FleetEpoch: t.FleetEpoch, ShipID: t.ShipID, ConnectionGeneration: t.ConnectionGeneration, Persona: t.Persona, SessionID: t.SessionID, Backend: t.Backend, ThreadID: t.ThreadID, TurnID: t.TurnID, ExpiresAt: in.ExpiresAt}
			s.interruptPublicPersonas[in.InterruptTargetRef] = in.Persona
			return nil
		}
	}
	return errors.New("stale_target")
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
