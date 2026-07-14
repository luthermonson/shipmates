// Package fleetidentity owns M7 Fleet enrollment and credential lifecycle.
// It deliberately contains no transport, listener, projection, or control API.
package fleetidentity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"sort"
	"sync"
	"time"
)

const (
	ObserveCapability       = "fleet.observe.v1"
	SteerTurnCapability     = "fleet.steer.turn.v1"
	InterruptTurnCapability = "fleet.interrupt.turn.v1"
	maxArtifacts            = 256
	maxShips                = 4096
	maxObservers            = 4096
	maxOperators            = 4096
	maxTxnResults           = 256
)

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{15,95}$`)

type Clock interface{ Now() time.Time }
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type Code string

const (
	InvalidInput  Code = "invalid_input"
	Unauthorized  Code = "unauthorized"
	Expired       Code = "expired"
	AlreadyExists Code = "already_exists"
	NotFound      Code = "not_found"
	Conflict      Code = "conflict"
	LimitExceeded Code = "limit_exceeded"
)

type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) }
func fail(c Code) error        { return &Error{Code: c} }

type SecretCredential struct {
	CredentialID string
	Secret       string
}
type EnrollmentArtifact struct {
	ArtifactID string
	Secret     string
	FleetID    string
	ExpiresAt  time.Time
}
type EnrollmentResult struct {
	FleetID    string
	ShipID     string
	Credential SecretCredential
}
type ShipPrincipal struct{ FleetID, ShipID, CredentialID string }
type ObserverPrincipal struct {
	FleetID, CredentialID, Capability string
	ShipIDs                           []string
}
type OperatorPrincipal struct {
	FleetID, SubjectID, CredentialID, Capability string
	CredentialGeneration                         uint64
	ShipIDs                                      []string
}
type OperatorCredentialRecord struct {
	CredentialID, SubjectID, FleetID, Capability string
	CredentialGeneration                         uint64
	ShipIDs                                      []string
	IssuedAt, ExpiresAt                          time.Time
	Revoked                                      bool
}
type IssuedOperatorCredential struct {
	Record OperatorCredentialRecord
	Secret string
}

type credential struct {
	id       string
	verifier [32]byte
	revoked  bool
}
type rotation struct {
	pending credential
	expires time.Time
	proved  bool
}
type ship struct {
	id         string
	revoked    bool
	current    credential
	rotation   *rotation
	generation uint64
}

// ShipProof returns the standard HMAC-SHA256 challenge proof used by the M7
// observation tunnel. The credential verifier is deliberately domain separated
// before it is used as a MAC key, so plaintext credentials never cross the
// application handshake.
func ShipProof(secret string, transcript []byte) []byte {
	v := verifier(secret)
	m := hmac.New(sha256.New, v[:])
	m.Write([]byte("shipmates/fleet-tunnel-proof/v1\x00"))
	m.Write(transcript)
	return m.Sum(nil)
}

func proofMatches(v [32]byte, transcript, proof []byte) bool {
	m := hmac.New(sha256.New, v[:])
	m.Write([]byte("shipmates/fleet-tunnel-proof/v1\x00"))
	m.Write(transcript)
	return hmac.Equal(m.Sum(nil), proof)
}

// AuthenticateShipProof authenticates a challenge-bound proof without
// receiving or retaining the plaintext credential in the tunnel server.
func (r *Registry) AuthenticateShipProof(credentialID string, transcript, proof []byte) (ShipPrincipal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return ShipPrincipal{}, err
	}
	before = r.durableLocked()
	if len(transcript) == 0 || len(transcript) > 4096 || len(proof) != sha256.Size {
		return ShipPrincipal{}, fail(Unauthorized)
	}
	for _, s := range r.ships {
		if s.revoked {
			continue
		}
		if s.current.id == credentialID && !s.current.revoked && proofMatches(s.current.verifier, transcript, proof) {
			return ShipPrincipal{r.fleetID, s.id, credentialID}, nil
		}
		if s.rotation != nil && s.rotation.pending.id == credentialID && !s.rotation.pending.revoked && proofMatches(s.rotation.pending.verifier, transcript, proof) {
			r.rotationProvedLocked(s)
			if err := r.commitLocked(before); err != nil {
				return ShipPrincipal{}, err
			}
			return ShipPrincipal{r.fleetID, s.id, credentialID}, nil
		}
	}
	return ShipPrincipal{}, fail(Unauthorized)
}

// AllocateConnectionGeneration durably orders successful handshakes for a
// ship. Exhaustion fails closed and never wraps.
func (r *Registry) AllocateConnectionGeneration(p ShipPrincipal) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return 0, err
	}
	before = r.durableLocked()
	s := r.ships[p.ShipID]
	if p.FleetID != r.fleetID || s == nil || s.revoked || !credentialActive(s, p.CredentialID) {
		return 0, fail(Unauthorized)
	}
	if s.generation == ^uint64(0) {
		return 0, fail(LimitExceeded)
	}
	s.generation++
	g := s.generation
	if err := r.commitLocked(before); err != nil {
		return 0, err
	}
	return g, nil
}

// ConnectionGeneration reports the durable current authenticated connection
// generation for reverse-channel binding. It conveys no credential material.
func (r *Registry) ConnectionGeneration(shipID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.ships[shipID]; s != nil {
		return s.generation
	}
	return 0
}

func credentialActive(s *ship, id string) bool {
	return s.current.id == id && !s.current.revoked || s.rotation != nil && s.rotation.pending.id == id && !s.rotation.pending.revoked
}

// ShipCredentialActive lets an established tunnel observe rotation/revocation
// before accepting each subsequent frame.
func (r *Registry) ShipCredentialActive(p ShipPrincipal) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return false, err
	}
	s := r.ships[p.ShipID]
	return p.FleetID == r.fleetID && s != nil && !s.revoked && credentialActive(s, p.CredentialID), nil
}

type artifact struct {
	verifier [32]byte
	expires  time.Time
	used     bool
	txnID    string
	result   EnrollmentResult
}
type observer struct {
	credential
	ships map[string]struct{}
}
type operatorCredential struct {
	credential
	subject, fleetID, capability string
	generation                   uint64
	ships                        map[string]struct{}
	issued, expires              time.Time
	revokeAt                     time.Time
	previousID                   string
}

// Registry is the Fleet-owned, concurrency-safe identity state machine. A
// persistence adapter can snapshot it later; no online connection state lives
// here. Returned secrets are available only on their issuing call.
type Registry struct {
	mu        sync.Mutex
	fleetID   string
	clock     Clock
	random    io.Reader
	artifacts map[string]*artifact
	ships     map[string]*ship
	observers map[string]*observer
	operators map[string]*operatorCredential
	txnOrder  []string
	storeDir  string
}

func NewRegistry(fleetID string, clock Clock, random io.Reader) (*Registry, error) {
	if !opaqueID.MatchString(fleetID) {
		return nil, fail(InvalidInput)
	}
	if clock == nil {
		clock = wallClock{}
	}
	if random == nil {
		random = rand.Reader
	}
	return &Registry{fleetID: fleetID, clock: clock, random: random, artifacts: map[string]*artifact{}, ships: map[string]*ship{}, observers: map[string]*observer{}, operators: map[string]*operatorCredential{}}, nil
}

// OpenRegistry opens (or creates) the Fleet-owned persistent authority store.
// Every authority mutation is committed by atomic replacement before it is
// reported successful. The directory and file are restricted to the owner.
func OpenRegistry(dir, fleetID string, clock Clock, random io.Reader) (*Registry, error) {
	r, err := NewRegistry(fleetID, clock, random)
	if err != nil {
		return nil, err
	}
	r.storeDir = dir
	if err = r.loadAuthority(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) randomValue(prefix string, bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := io.ReadFull(r.random, b); err != nil {
		return "", fail(Conflict)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}
func verifier(secret string) [32]byte {
	return sha256.Sum256([]byte("shipmates/fleet-credential/v1\x00" + secret))
}
func verifies(secret string, want [32]byte) bool {
	got := verifier(secret)
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
func validTTL(ttl time.Duration) bool { return ttl >= time.Minute && ttl <= 24*time.Hour }

func (r *Registry) CreateEnrollment(ttl time.Duration) (EnrollmentArtifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return EnrollmentArtifact{}, err
	}
	before = r.durableLocked()
	if !validTTL(ttl) || len(r.artifacts) >= maxArtifacts {
		return EnrollmentArtifact{}, fail(InvalidInput)
	}
	id, err := r.randomValue("enr_", 18)
	if err != nil {
		return EnrollmentArtifact{}, err
	}
	secret, err := r.randomValue("ens_", 32)
	if err != nil {
		return EnrollmentArtifact{}, err
	}
	expires := r.clock.Now().Add(ttl)
	r.artifacts[id] = &artifact{verifier: verifier(secret), expires: expires}
	if err := r.commitLocked(before); err != nil {
		return EnrollmentArtifact{}, err
	}
	return EnrollmentArtifact{ArtifactID: id, Secret: secret, FleetID: r.fleetID, ExpiresAt: expires}, nil
}

// Enroll is idempotent only for the exact enrollment transaction. Replaying an
// artifact with another transaction can never allocate a second ship.
func (r *Registry) Enroll(artifactID, secret, transactionID string) (EnrollmentResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if !opaqueID.MatchString(artifactID) || !opaqueID.MatchString(transactionID) || len(secret) > 128 {
		return EnrollmentResult{}, fail(Unauthorized)
	}
	a := r.artifacts[artifactID]
	if a == nil || !verifies(secret, a.verifier) {
		return EnrollmentResult{}, fail(Unauthorized)
	}
	if a.used {
		if a.txnID == transactionID {
			return a.result, nil
		}
		return EnrollmentResult{}, fail(Unauthorized)
	}
	if !r.clock.Now().Before(a.expires) {
		return EnrollmentResult{}, fail(Expired)
	}
	if len(r.ships) >= maxShips {
		return EnrollmentResult{}, fail(LimitExceeded)
	}
	shipID, err := r.randomValue("shp_", 18)
	if err != nil {
		return EnrollmentResult{}, err
	}
	credID, err := r.randomValue("shc_", 18)
	if err != nil {
		return EnrollmentResult{}, err
	}
	credSecret, err := r.randomValue("shs_", 32)
	if err != nil {
		return EnrollmentResult{}, err
	}
	res := EnrollmentResult{FleetID: r.fleetID, ShipID: shipID, Credential: SecretCredential{CredentialID: credID, Secret: credSecret}}
	r.ships[shipID] = &ship{id: shipID, current: credential{id: credID, verifier: verifier(credSecret)}}
	a.used, a.txnID, a.result = true, transactionID, res
	r.txnOrder = append(r.txnOrder, artifactID)
	for len(r.txnOrder) > maxTxnResults {
		old := r.txnOrder[0]
		r.txnOrder = r.txnOrder[1:]
		delete(r.artifacts, old)
	}
	if err := r.commitLocked(before); err != nil {
		return EnrollmentResult{}, err
	}
	return res, nil
}

func (r *Registry) AuthenticateShip(credentialID, secret string) (ShipPrincipal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return ShipPrincipal{}, err
	}
	before = r.durableLocked()
	for _, s := range r.ships {
		if s.revoked {
			continue
		}
		if s.current.id == credentialID && !s.current.revoked && verifies(secret, s.current.verifier) {
			return ShipPrincipal{r.fleetID, s.id, credentialID}, nil
		}
		if s.rotation != nil && s.rotation.pending.id == credentialID && !s.rotation.pending.revoked && verifies(secret, s.rotation.pending.verifier) {
			r.rotationProvedLocked(s)
			if err := r.commitLocked(before); err != nil {
				return ShipPrincipal{}, err
			}
			return ShipPrincipal{r.fleetID, s.id, credentialID}, nil
		}
	}
	return ShipPrincipal{}, fail(Unauthorized)
}

func (r *Registry) IssueShipRotation(shipID string, overlap time.Duration) (SecretCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return SecretCredential{}, err
	}
	before = r.durableLocked()
	s := r.ships[shipID]
	if s == nil || s.revoked {
		return SecretCredential{}, fail(NotFound)
	}
	if s.rotation != nil || overlap < time.Minute || overlap > time.Hour {
		return SecretCredential{}, fail(Conflict)
	}
	id, err := r.randomValue("shc_", 18)
	if err != nil {
		return SecretCredential{}, err
	}
	secret, err := r.randomValue("shs_", 32)
	if err != nil {
		return SecretCredential{}, err
	}
	s.rotation = &rotation{pending: credential{id: id, verifier: verifier(secret)}, expires: r.clock.Now().Add(overlap)}
	if err := r.commitLocked(before); err != nil {
		return SecretCredential{}, err
	}
	return SecretCredential{id, secret}, nil
}
func (r *Registry) rotationProvedLocked(s *ship) {
	if s.rotation != nil {
		s.rotation.proved = true
	}
}
func (r *Registry) CommitShipRotation(shipID, credentialID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return err
	}
	before = r.durableLocked()
	s := r.ships[shipID]
	if s == nil || s.revoked {
		return fail(NotFound)
	}
	if s.rotation == nil || s.rotation.pending.id != credentialID || !s.rotation.proved {
		return fail(Conflict)
	}
	s.current.revoked = true
	s.current = s.rotation.pending
	s.rotation = nil
	return r.commitLocked(before)
}
func (r *Registry) RevokeShipCredential(shipID, credentialID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	s := r.ships[shipID]
	if s == nil {
		return fail(NotFound)
	}
	switch {
	case s.current.id == credentialID:
		s.current.revoked = true
	case s.rotation != nil && s.rotation.pending.id == credentialID:
		s.rotation.pending.revoked = true
	default:
		return fail(NotFound)
	}
	return r.commitLocked(before)
}
func (r *Registry) RevokeShip(shipID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	s := r.ships[shipID]
	if s == nil {
		return fail(NotFound)
	}
	s.revoked = true
	s.current.revoked = true
	if s.rotation != nil {
		s.rotation.pending.revoked = true
	}
	return r.commitLocked(before)
}

func (r *Registry) IssueObserver(shipIDs []string) (SecretCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if len(r.observers) >= maxObservers {
		return SecretCredential{}, fail(LimitExceeded)
	}
	allowed := map[string]struct{}{}
	for _, id := range shipIDs {
		if !opaqueID.MatchString(id) || r.ships[id] == nil {
			return SecretCredential{}, fail(InvalidInput)
		}
		allowed[id] = struct{}{}
	}
	id, err := r.randomValue("obc_", 18)
	if err != nil {
		return SecretCredential{}, err
	}
	secret, err := r.randomValue("obs_", 32)
	if err != nil {
		return SecretCredential{}, err
	}
	r.observers[id] = &observer{credential: credential{id: id, verifier: verifier(secret)}, ships: allowed}
	if err := r.commitLocked(before); err != nil {
		return SecretCredential{}, err
	}
	return SecretCredential{id, secret}, nil
}
func (r *Registry) AuthenticateObserver(credentialID, secret string) (ObserverPrincipal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.observers[credentialID]
	if o == nil || o.revoked || !verifies(secret, o.verifier) {
		return ObserverPrincipal{}, fail(Unauthorized)
	}
	ids := make([]string, 0, len(o.ships))
	for id := range o.ships {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ObserverPrincipal{r.fleetID, credentialID, ObserveCapability, ids}, nil
}
func (r *Registry) RevokeObserver(credentialID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	o := r.observers[credentialID]
	if o == nil {
		return fail(NotFound)
	}
	o.revoked = true
	return r.commitLocked(before)
}

func operatorRecord(o *operatorCredential) OperatorCredentialRecord {
	ids := make([]string, 0, len(o.ships))
	for id := range o.ships {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return OperatorCredentialRecord{o.id, o.subject, o.fleetID, o.capability, o.generation, ids, o.issued, o.expires, o.revoked}
}

// IssueOperator creates a role-separated, short-lived steer credential. Its
// secret is returned only by this call and is never retained by the registry.
func (r *Registry) IssueOperator(subjectID string, shipIDs []string, ttl time.Duration) (IssuedOperatorCredential, error) {
	return r.IssueOperatorCapability(subjectID, shipIDs, SteerTurnCapability, ttl)
}

// IssueOperatorCapability creates one capability-dark operator credential.
// Capabilities are deliberately single-valued so review and rotation cannot
// silently widen an existing grant.
func (r *Registry) IssueOperatorCapability(subjectID string, shipIDs []string, capability string, ttl time.Duration) (IssuedOperatorCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return IssuedOperatorCredential{}, err
	}
	before = r.durableLocked()
	if !opaqueID.MatchString(subjectID) || !validOperatorCapability(capability) || !validTTL(ttl) || len(shipIDs) == 0 || len(r.operators) >= maxOperators {
		return IssuedOperatorCredential{}, fail(InvalidInput)
	}
	allowed := map[string]struct{}{}
	for _, id := range shipIDs {
		if !opaqueID.MatchString(id) || r.ships[id] == nil {
			return IssuedOperatorCredential{}, fail(InvalidInput)
		}
		allowed[id] = struct{}{}
	}
	id, err := r.randomValue("opc_", 18)
	if err != nil {
		return IssuedOperatorCredential{}, err
	}
	if r.operators[id] != nil {
		return IssuedOperatorCredential{}, fail(Conflict)
	}
	secret, err := r.randomValue("ops_", 32)
	if err != nil {
		return IssuedOperatorCredential{}, err
	}
	now := r.clock.Now()
	o := &operatorCredential{credential: credential{id: id, verifier: verifier(secret)}, subject: subjectID, fleetID: r.fleetID, capability: capability, generation: 1, ships: allowed, issued: now, expires: now.Add(ttl)}
	r.operators[id] = o
	if err := r.commitLocked(before); err != nil {
		return IssuedOperatorCredential{}, err
	}
	return IssuedOperatorCredential{Record: operatorRecord(o), Secret: secret}, nil
}

// RotateOperator issues generation g+1 for the same subject and exact scope.
// The old generation remains usable only for the requested bounded overlap.
func (r *Registry) RotateOperator(credentialID string, generation uint64, overlap, ttl time.Duration) (IssuedOperatorCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return IssuedOperatorCredential{}, err
	}
	before = r.durableLocked()
	old := r.operators[credentialID]
	if old == nil || old.revoked || old.generation != generation {
		return IssuedOperatorCredential{}, fail(NotFound)
	}
	if generation == ^uint64(0) || overlap <= 0 || overlap > 5*time.Minute || !validTTL(ttl) {
		return IssuedOperatorCredential{}, fail(Conflict)
	}
	for _, o := range r.operators {
		if o.subject == old.subject && o.generation > old.generation && !o.revoked {
			return IssuedOperatorCredential{}, fail(Conflict)
		}
	}
	id, err := r.randomValue("opc_", 18)
	if err != nil {
		return IssuedOperatorCredential{}, err
	}
	if r.operators[id] != nil {
		return IssuedOperatorCredential{}, fail(Conflict)
	}
	secret, err := r.randomValue("ops_", 32)
	if err != nil {
		return IssuedOperatorCredential{}, err
	}
	now := r.clock.Now()
	old.revokeAt = now.Add(overlap)
	ships := map[string]struct{}{}
	for id := range old.ships {
		ships[id] = struct{}{}
	}
	o := &operatorCredential{credential: credential{id: id, verifier: verifier(secret)}, subject: old.subject, fleetID: old.fleetID, capability: old.capability, generation: generation + 1, ships: ships, issued: now, expires: now.Add(ttl), previousID: old.id}
	r.operators[id] = o
	if err := r.commitLocked(before); err != nil {
		return IssuedOperatorCredential{}, err
	}
	return IssuedOperatorCredential{Record: operatorRecord(o), Secret: secret}, nil
}

func (r *Registry) CommitOperatorRotation(credentialID string, generation uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	n := r.operators[credentialID]
	if n == nil || n.revoked || n.generation != generation || generation < 2 {
		return fail(NotFound)
	}
	old := r.operators[n.previousID]
	if old == nil || old.subject != n.subject || old.generation+1 != generation || old.revoked {
		return fail(Conflict)
	}
	old.revoked = true
	old.revokeAt = time.Time{}
	return r.commitLocked(before)
}

func (r *Registry) AuthenticateOperator(credentialID, secret, fleetID, shipID string) (OperatorPrincipal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return OperatorPrincipal{}, err
	}
	o := r.operators[credentialID]
	if o == nil || o.revoked || o.fleetID != fleetID || !validOperatorCapability(o.capability) || !r.clock.Now().Before(o.expires) || !verifies(secret, o.verifier) {
		return OperatorPrincipal{}, fail(Unauthorized)
	}
	if _, ok := o.ships[shipID]; !ok {
		return OperatorPrincipal{}, fail(Unauthorized)
	}
	rec := operatorRecord(o)
	return OperatorPrincipal{rec.FleetID, rec.SubjectID, rec.CredentialID, rec.Capability, rec.CredentialGeneration, rec.ShipIDs}, nil
}

// AuthenticateOperatorCredential authenticates before an action target is
// selected. Capability-specific discovery uses the returned immutable scope.
func (r *Registry) AuthenticateOperatorCredential(credentialID, secret string) (OperatorPrincipal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return OperatorPrincipal{}, err
	}
	o := r.operators[credentialID]
	if o == nil || o.revoked || !validOperatorCapability(o.capability) || !r.clock.Now().Before(o.expires) || !verifies(secret, o.verifier) {
		return OperatorPrincipal{}, fail(Unauthorized)
	}
	rec := operatorRecord(o)
	return OperatorPrincipal{rec.FleetID, rec.SubjectID, rec.CredentialID, rec.Capability, rec.CredentialGeneration, rec.ShipIDs}, nil
}

func validOperatorCapability(c string) bool {
	return c == SteerTurnCapability || c == InterruptTurnCapability
}

func (r *Registry) InspectOperator(credentialID string, generation uint64) (OperatorCredentialRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	if err := r.expireAndCommitLocked(before); err != nil {
		return OperatorCredentialRecord{}, err
	}
	o := r.operators[credentialID]
	if o == nil || o.generation != generation {
		return OperatorCredentialRecord{}, fail(NotFound)
	}
	return operatorRecord(o), nil
}
func (r *Registry) RevokeOperatorCredential(credentialID string, generation uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	o := r.operators[credentialID]
	if o == nil || o.generation != generation {
		return fail(NotFound)
	}
	o.revoked = true
	o.revokeAt = time.Time{}
	return r.commitLocked(before)
}
func (r *Registry) RevokeOperatorSubject(subjectID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.durableLocked()
	found := false
	for _, o := range r.operators {
		if o.subject == subjectID {
			o.revoked = true
			o.revokeAt = time.Time{}
			found = true
		}
	}
	if !found {
		return fail(NotFound)
	}
	return r.commitLocked(before)
}

func (r *Registry) expireLocked() bool {
	now := r.clock.Now()
	changed := false
	for id, a := range r.artifacts {
		if !a.used && !now.Before(a.expires) {
			delete(r.artifacts, id)
			changed = true
		}
	}
	for _, s := range r.ships {
		if s.rotation != nil && !now.Before(s.rotation.expires) {
			s.current.revoked = true
			s.current = s.rotation.pending
			s.rotation = nil
			changed = true
		}
	}
	for _, o := range r.operators {
		if !o.revoked && (!now.Before(o.expires) || (!o.revokeAt.IsZero() && !now.Before(o.revokeAt))) {
			o.revoked = true
			o.revokeAt = time.Time{}
			changed = true
		}
	}
	return changed
}

// expireAndCommitLocked makes clock-driven authority changes part of the same
// durability contract as explicit mutations. Callers must invoke it before
// making any later mutation and must not return an authentication result until
// it succeeds.
func (r *Registry) expireAndCommitLocked(before durableAuthority) error {
	if !r.expireLocked() {
		return nil
	}
	return r.commitLocked(before)
}

func IsCode(err error, code Code) bool { var e *Error; return errors.As(err, &e) && e.Code == code }
