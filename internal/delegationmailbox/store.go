//go:build unix

// Package delegationmailbox is the ship-local M3 inbox/outbox. It contains no
// transport and calls M2 only through the narrow adapter below.
package delegationmailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/luthermonson/shipmates/internal/delegation"
	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/project"
	"golang.org/x/sys/unix"
)

const (
	StateSchema               = 1
	MaxInbox                  = 16
	MaxOutbox                 = 32
	MaxWorkerPreStartAttempts = 3
)

type InstructionClaim struct {
	InstructionID  string    `json:"instruction_id"`
	EnvelopeDigest string    `json:"envelope_digest"`
	State          string    `json:"state"`
	Attempts       uint8     `json:"attempts"`
	RetryNotBefore time.Time `json:"retry_not_before,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ExecutionIntent is the immutable, fsynced identity of the one local M2
// execution. PID and start time are diagnostic liveness evidence only; the
// held execution lock is the ownership proof.
type ExecutionIntent struct {
	Schema         int       `json:"schema"`
	ExecutionID    string    `json:"execution_id"`
	InstructionID  string    `json:"instruction_id"`
	EnvelopeDigest string    `json:"envelope_digest"`
	M2Provenance   string    `json:"m2_provenance"`
	BootID         string    `json:"boot_id"`
	OwnerPID       int       `json:"owner_pid"`
	OwnerStartTime string    `json:"owner_start_time"`
	LockGeneration uint64    `json:"lock_generation"`
	LockDevice     uint64    `json:"lock_device"`
	LockInode      uint64    `json:"lock_inode"`
	CreatedAt      time.Time `json:"created_at"`
}

// ExecutionFact is separate from publication state. Facts are append-only
// and are never rewritten when a connection generation reconnects.
type ExecutionFact struct {
	Schema         int              `json:"schema"`
	Kind           string           `json:"kind"`
	ExecutionID    string           `json:"execution_id"`
	InstructionID  string           `json:"instruction_id,omitempty"`
	EnvelopeDigest string           `json:"envelope_digest,omitempty"`
	BootID         string           `json:"boot_id,omitempty"`
	OwnerPID       int              `json:"owner_pid,omitempty"`
	OwnerStartTime string           `json:"owner_start_time,omitempty"`
	LockGeneration uint64           `json:"lock_generation,omitempty"`
	LockDevice     uint64           `json:"lock_device,omitempty"`
	LockInode      uint64           `json:"lock_inode,omitempty"`
	Intent         *ExecutionIntent `json:"intent,omitempty"`
	M2StartDigest  string           `json:"m2_start_digest,omitempty"`
	Code           string           `json:"code,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// PublicationFact is deliberately separate from execution facts and mailbox
// cursors. A reconnect may append publication progress without changing the
// immutable assessment identity.
type PublicationFact struct {
	Schema         int       `json:"schema"`
	Kind           string    `json:"kind"`
	InstructionID  string    `json:"instruction_id"`
	EnvelopeDigest string    `json:"envelope_digest"`
	Generation     uint64    `json:"generation"`
	CreatedAt      time.Time `json:"created_at"`
}

// PublicationLease is a generation-owned, compare-and-swap lease over one
// immutable outbox projection. The nonce makes a reconnect/supersession
// distinguishable even when it reuses the same generation number in tests.
type PublicationLease struct {
	EventID     string
	EventDigest string
	Generation  uint64
	Epoch       uint64
	Nonce       string
	State       string
	Message     fleetcommander.Message
}

type publicationState struct {
	EventID     string `json:"event_id"`
	EventDigest string `json:"event_digest"`
	Generation  uint64 `json:"generation"`
	Epoch       uint64 `json:"epoch"`
	Nonce       string `json:"nonce"`
	State       string `json:"state"`
}

const executionSchema = 1

type ExecutionLease struct {
	store    *Store
	file     *os.File
	intent   ExecutionIntent
	once     sync.Once
	released atomic.Bool
}

func (l *ExecutionLease) Intent() ExecutionIntent {
	if l == nil {
		return ExecutionIntent{}
	}
	return l.intent
}

// Release relinquishes the live execution ownership. It does not remove the
// immutable intent or any execution facts, so a later process cannot retry it.
func (l *ExecutionLease) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	var err error
	l.once.Do(func() {
		l.released.Store(true)
		err = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		closeErr := l.file.Close()
		if err == nil {
			err = closeErr
		}
	})
	return err
}

// Record appends an execution fact while the live execution lock is held.
func (l *ExecutionLease) Record(kind, code string, at time.Time) error {
	if l == nil || l.file == nil || l.released.Load() || !validExecutionKind(kind) || kind == "execution_intent" || kind == "claim_acquired" {
		return errors.New("invalid execution fact")
	}
	return appendExecutionFact(l.store.executionPath, ExecutionFact{Schema: executionSchema, Kind: kind, ExecutionID: l.intent.ExecutionID, InstructionID: l.intent.InstructionID, EnvelopeDigest: l.intent.EnvelopeDigest, BootID: l.intent.BootID, OwnerPID: l.intent.OwnerPID, OwnerStartTime: l.intent.OwnerStartTime, LockGeneration: l.intent.LockGeneration, LockDevice: l.intent.LockDevice, LockInode: l.intent.LockInode, Code: boundedExecutionCode(code), CreatedAt: at.UTC()})
}

// RecordM2StartWitness appends the digest of M2's immutable
// assessment_started record. The mailbox cannot manufacture this digest.
func (l *ExecutionLease) RecordM2StartWitness(digest string, at time.Time) error {
	if l == nil || l.file == nil || l.released.Load() || !validDigest(digest) {
		return errors.New("invalid M2 start witness")
	}
	return appendExecutionFact(l.store.executionPath, ExecutionFact{Schema: executionSchema, Kind: "m2_start_witnessed", ExecutionID: l.intent.ExecutionID, InstructionID: l.intent.InstructionID, EnvelopeDigest: l.intent.EnvelopeDigest, BootID: l.intent.BootID, OwnerPID: l.intent.OwnerPID, OwnerStartTime: l.intent.OwnerStartTime, LockGeneration: l.intent.LockGeneration, LockDevice: l.intent.LockDevice, LockInode: l.intent.LockInode, M2StartDigest: digest, CreatedAt: at.UTC()})
}

func boundedExecutionCode(code string) string {
	if len(code) > 64 {
		return code[:64]
	}
	return code
}

func (s *Store) RecordPublication(instructionID, envelopeDigest, kind string, generation uint64, at time.Time) error {
	if s == nil || instructionID == "" || envelopeDigest == "" || generation == 0 || !validPublicationKind(kind) {
		return errors.New("invalid publication fact")
	}
	return appendPublicationFact(s.publicationPath, PublicationFact{Schema: executionSchema, Kind: kind, InstructionID: instructionID, EnvelopeDigest: envelopeDigest, Generation: generation, CreatedAt: at.UTC()})
}

func (s *Store) PublicationFacts() ([]PublicationFact, error) {
	if s == nil {
		return nil, errors.New("nil mailbox")
	}
	b, err := os.ReadFile(s.publicationPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || len(b) > 256<<10 {
		return nil, errors.New("invalid publication journal")
	}
	var facts []PublicationFact
	last := make(map[string]int)
	for _, line := range splitLines(b) {
		if len(line) == 0 {
			continue
		}
		var fact PublicationFact
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fact); err != nil || fact.Schema != executionSchema || fact.InstructionID == "" || len(fact.EnvelopeDigest) != 64 || fact.Generation == 0 || fact.CreatedAt.IsZero() || !validPublicationKind(fact.Kind) {
			return nil, errors.New("corrupt publication journal")
		}
		rank := map[string]int{"publication_claimed": 1, "published": 2, "acknowledged": 3}[fact.Kind]
		if prior := last[fact.InstructionID]; rank < prior || rank > prior+1 {
			return nil, errors.New("invalid publication fact order")
		}
		last[fact.InstructionID] = rank
		facts = append(facts, fact)
	}
	return facts, nil
}

func validPublicationKind(kind string) bool {
	return kind == "publication_claimed" || kind == "published" || kind == "acknowledged"
}

type WorkerLease struct {
	Schema         int       `json:"schema"`
	Generation     uint64    `json:"generation"`
	WorkerEpoch    uint64    `json:"worker_epoch"`
	Attempts       uint8     `json:"attempts"`
	RetryNotBefore time.Time `json:"retry_not_before,omitempty"`
	State          string    `json:"state"`
	OutcomeCode    string    `json:"outcome_code,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type ProjectionLifecycle struct {
	Generation  uint64    `json:"generation"`
	WorkerEpoch uint64    `json:"worker_epoch"`
	State       string    `json:"state"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// M2Adapter is intentionally local. Accept validates the exact local case and
// invokes M2's existing idempotent entry point; Lookup is the authoritative
// durable provenance read used to form all ship projections.
type M2Adapter interface {
	Accept(context.Context, []byte) error
	Lookup(string) (delegation.Record, bool)
}

type state struct {
	Schema      int                         `json:"schema"`
	FleetID     string                      `json:"fleet_id"`
	ShipID      string                      `json:"ship_id"`
	Inbox       []fleetcommander.Message    `json:"inbox"`
	Outbox      []fleetcommander.Message    `json:"outbox"`
	InboxAck    uint64                      `json:"inbox_ack"`
	OutboxAck   uint64                      `json:"outbox_ack"`
	NextOutbox  uint64                      `json:"next_outbox"`
	Worker      *WorkerLease                `json:"worker,omitempty"`
	Projection  *ProjectionLifecycle        `json:"projection,omitempty"`
	Publication *publicationState           `json:"publication,omitempty"`
	Claims      map[string]InstructionClaim `json:"claims,omitempty"`
}

type Store struct {
	mu                                                              sync.Mutex
	path, lockPath, executionPath, publicationPath, fleetID, shipID string
	st                                                              state
}

func Open(root, fleetID, shipID string) (*Store, error) {
	if !validID(fleetID) || !validID(shipID) {
		return nil, errors.New("invalid delegation mailbox identity")
	}
	root, err := project.CanonicalRoot(root)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, ".shipmates", "delegationmailbox")
	if err := safeParents(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fleetID+"-"+shipID+".json")
	if err := ensureRegular(path); err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(fleetID + "\x00" + shipID))
	executionPath := filepath.Join(dir, "execution-"+fmt.Sprintf("%x", h[:])+".jsonl")
	publicationPath := filepath.Join(dir, "publication-"+fmt.Sprintf("%x", h[:])+".jsonl")
	s := &Store{path: path, lockPath: path + ".lock", executionPath: executionPath, publicationPath: publicationPath, fleetID: fleetID, shipID: shipID, st: state{Schema: StateSchema, FleetID: fleetID, ShipID: shipID}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Cursors returns the durable Fleet-to-ship and ship-to-Fleet cursors used by
// a new connection-generation owner. It is read-only and never allocates or
// advances either lane.
func (s *Store) Cursors() (fleetAck, shipAck uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s == nil {
		return 0, 0
	}
	return s.st.InboxAck, s.st.OutboxAck
}

// AcquireWorkerLease durably reserves one bounded pre-start attempt for a
// connection generation. A false result is a safe no-worker decision: the
// scheduler may continue draining the mailbox without invoking M2.
func (s *Store) AcquireWorkerLease(generation uint64, now time.Time) (WorkerLease, bool, error) {
	if s == nil || generation == 0 {
		return WorkerLease{}, false, errors.New("invalid worker generation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Worker != nil {
		old := *s.st.Worker
		if old.Generation == generation && (old.State == "leased" || old.State == "running") {
			return old, false, nil
		}
		if old.Attempts >= MaxWorkerPreStartAttempts || now.Before(old.RetryNotBefore) {
			return old, false, nil
		}
	}
	var attempts uint8
	var epoch uint64
	if s.st.Worker != nil {
		attempts = s.st.Worker.Attempts
		epoch = s.st.Worker.WorkerEpoch
	}
	attempts++
	workerEpoch := uint64(epoch) + 1
	lease := WorkerLease{Schema: 1, Generation: generation, WorkerEpoch: workerEpoch, Attempts: attempts, State: "leased", UpdatedAt: now.UTC()}
	s.st.Worker = &lease
	if err := s.commit(); err != nil {
		return WorkerLease{}, false, err
	}
	return lease, true, nil
}

func (s *Store) MarkWorkerStarted(generation, epoch uint64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Worker == nil || s.st.Worker.Generation != generation || s.st.Worker.WorkerEpoch != epoch || s.st.Worker.State != "leased" {
		return errors.New("worker lease is stale")
	}
	s.st.Worker.State = "running"
	s.st.Worker.UpdatedAt = now.UTC()
	return s.commit()
}

func (s *Store) MarkWorkerOutcome(generation, epoch uint64, state, code string, now time.Time) error {
	if state != "completed" && state != "failed_before_start" && state != "indeterminate" && state != "released" {
		return errors.New("invalid worker outcome")
	}
	if !validWorkerCode(code) {
		return errors.New("invalid worker outcome code")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Worker == nil || s.st.Worker.Generation != generation || s.st.Worker.WorkerEpoch != epoch {
		return errors.New("worker lease is stale")
	}
	s.st.Worker.State, s.st.Worker.OutcomeCode, s.st.Worker.UpdatedAt = state, boundedCode(code), now.UTC()
	if state == "failed_before_start" {
		s.st.Worker.RetryNotBefore = now.UTC().Add(time.Duration(s.st.Worker.Attempts) * time.Second)
	} else {
		s.st.Worker.RetryNotBefore = time.Time{}
	}
	return s.commit()
}

func (s *Store) WorkerLease() (WorkerLease, bool) {
	if s == nil {
		return WorkerLease{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Worker == nil {
		return WorkerLease{}, false
	}
	return *s.st.Worker, true
}

func (s *Store) WorkerIsCurrent(generation, epoch uint64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Worker != nil && s.st.Worker.Generation == generation && s.st.Worker.WorkerEpoch == epoch && (s.st.Worker.State == "leased" || s.st.Worker.State == "running")
}

func (s *Store) HasTerminalProjection() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, message := range s.st.Outbox {
		var completed fleetcommander.Completed
		if fleetcommander.DecodeClosed(message.Body, &completed) == nil && completed.Type == fleetcommander.CompletedType {
			return true
		}
	}
	return false
}

func (s *Store) TerminalProjectionResult() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, message := range s.st.Outbox {
		var completed fleetcommander.Completed
		if fleetcommander.DecodeClosed(message.Body, &completed) == nil && completed.Type == fleetcommander.CompletedType {
			return completed.Result
		}
	}
	return ""
}

// Deliver durably commits the complete unchanged instruction before the
// caller is allowed to acknowledge the carrier.
func (s *Store) Deliver(m fleetcommander.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := m.Validate(); err != nil || m.Direction != fleetcommander.FleetToShip || m.FleetID != s.fleetID || m.ShipID != s.shipID {
		return errors.New("invalid delegation delivery")
	}
	for _, old := range s.st.Inbox {
		if old.MailboxSequence == m.MailboxSequence {
			if same(old, m) {
				return nil
			}
			return errors.New("delivery sequence conflict")
		}
	}
	if m.MailboxSequence == 0 || m.MailboxSequence != uint64(len(s.st.Inbox))+1 {
		return errors.New("delivery sequence gap")
	}
	if len(s.st.Inbox)-int(s.st.InboxAck) >= MaxInbox {
		return errors.New("delegation inbox backpressure")
	}
	s.st.Inbox = append(s.st.Inbox, m)
	return s.commit()
}

// PendingInstructions returns immutable instructions whose durable claim has
// not reached a terminal or uncertain M2 boundary. It is read-only; the
// process supervisor performs the atomic claim before starting assessment.
func (s *Store) PendingInstructions() ([]fleetcommander.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fleetcommander.Message, 0)
	for _, message := range s.st.Inbox {
		var instruction fleetcommander.Instruction
		if fleetcommander.DecodeClosed(message.Body, &instruction) != nil {
			return nil, errors.New("invalid instruction body")
		}
		claim, ok := s.st.Claims[message.InstructionID]
		if ok && (claim.State == "m2_started_observed" || claim.State == "terminal" || claim.State == "indeterminate" || claim.State == "published" || claim.State == "acknowledged") {
			continue
		}
		out = append(out, message)
	}
	return out, nil
}

// ClaimInstruction reserves one exact immutable instruction across all Store
// instances by taking the owner-only file lock and replaying the latest state
// while holding it. Only pre-M2 claims may use the bounded retry schedule.
func (s *Store) ClaimInstruction(m fleetcommander.Message, now time.Time) (InstructionClaim, bool, error) {
	var instruction fleetcommander.Instruction
	if fleetcommander.DecodeClosed(m.Body, &instruction) != nil || instruction.EnvelopeDigest == "" || m.InstructionID == "" {
		return InstructionClaim{}, false, errors.New("invalid instruction claim")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return InstructionClaim{}, false, err
	}
	defer unlock()
	if latest.Claims == nil {
		latest.Claims = make(map[string]InstructionClaim)
	}
	if prior, ok := latest.Claims[m.InstructionID]; ok {
		if prior.EnvelopeDigest != instruction.EnvelopeDigest {
			return InstructionClaim{}, false, errors.New("instruction claim conflict")
		}
		// m2_call_entered is irreversible. Absence of an observed M2 record is
		// never proof that entry did not occur, so this claim is not retried.
		return prior, false, nil
	}
	claim := InstructionClaim{InstructionID: m.InstructionID, EnvelopeDigest: instruction.EnvelopeDigest, State: "m2_call_entered", Attempts: 1, RetryNotBefore: now.UTC().Add(time.Second), UpdatedAt: now.UTC()}
	latest.Claims[m.InstructionID] = claim
	if err := s.writeLocked(latest); err != nil {
		return InstructionClaim{}, false, err
	}
	s.st = latest
	return claim, true, nil
}

// AcquireExecution atomically records a claim and then its immutable
// execution_intent while holding a dedicated cross-process lock. A false
// result means another live owner holds the lock or an intent already exists.
// The only recoverable crash window is a durable claim with no intent; taking
// the lock is positive owner-death proof and permits at most three attempts.
func (s *Store) AcquireExecution(m fleetcommander.Message, m2Provenance string, now time.Time) (*ExecutionLease, bool, error) {
	var instruction fleetcommander.Instruction
	if s == nil || fleetcommander.DecodeClosed(m.Body, &instruction) != nil || m.InstructionID == "" || instruction.EnvelopeDigest == "" || m2Provenance == "" {
		return nil, false, errors.New("invalid execution binding")
	}
	lock, err := os.OpenFile(s.executionPath+".lock", os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, false, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	identity, err := lockIdentityOf(lock)
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return nil, false, err
	}
	if err := verifyExecutionLockPath(s.executionPath+".lock", identity); err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return nil, false, err
	}
	release := func(e error) (*ExecutionLease, bool, error) {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return nil, false, e
	}
	facts, err := readExecutionFacts(s.executionPath)
	if err != nil {
		return release(err)
	}
	for _, fact := range facts {
		if fact.Intent != nil && fact.Intent.InstructionID == m.InstructionID {
			if fact.Intent.EnvelopeDigest != instruction.EnvelopeDigest || fact.Intent.M2Provenance != m2Provenance {
				return release(errors.New("execution binding conflict"))
			}
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			_ = lock.Close()
			return nil, false, nil
		}
	}
	var prior *ExecutionFact
	for i := range facts {
		fact := &facts[i]
		if fact.Kind == "claim_acquired" && fact.InstructionID == m.InstructionID {
			if fact.EnvelopeDigest != instruction.EnvelopeDigest {
				return release(errors.New("execution binding conflict"))
			}
			if prior != nil && fact.LockGeneration <= prior.LockGeneration {
				return release(errors.New("corrupt execution claim order"))
			}
			prior = fact
		}
	}
	attempts := uint8(1)
	if prior != nil {
		if prior.LockGeneration >= MaxWorkerPreStartAttempts {
			return release(errors.New("execution retry budget exhausted"))
		}
		dead, proofErr := positiveOwnerDeath(*prior, identity)
		if proofErr != nil {
			return release(proofErr)
		}
		if !dead {
			return release(errors.New("execution owner is not positively dead"))
		}
		attempts = uint8(prior.LockGeneration + 1)
	}
	bootID, err := linuxBootID()
	if err != nil {
		return release(errors.New("execution prerequisites unavailable"))
	}
	start, err := linuxProcessStartTime(os.Getpid())
	if err != nil {
		return release(errors.New("execution owner identity unavailable"))
	}
	intent := ExecutionIntent{Schema: executionSchema, ExecutionID: executionID(now), InstructionID: m.InstructionID, EnvelopeDigest: instruction.EnvelopeDigest, M2Provenance: m2Provenance, BootID: bootID, OwnerPID: os.Getpid(), OwnerStartTime: start, LockGeneration: uint64(attempts), LockDevice: identity.Device, LockInode: identity.Inode, CreatedAt: now.UTC()}
	claimFact := ExecutionFact{Schema: executionSchema, Kind: "claim_acquired", ExecutionID: intent.ExecutionID, InstructionID: m.InstructionID, EnvelopeDigest: instruction.EnvelopeDigest, BootID: bootID, OwnerPID: os.Getpid(), OwnerStartTime: start, LockGeneration: uint64(attempts), LockDevice: identity.Device, LockInode: identity.Inode, Code: strconv.Itoa(int(attempts)), CreatedAt: now.UTC()}
	if err := verifyExecutionLockPath(s.executionPath+".lock", identity); err != nil {
		return release(err)
	}
	if err := appendExecutionFact(s.executionPath, claimFact); err != nil {
		return release(err)
	}
	if err := appendExecutionFact(s.executionPath, ExecutionFact{Schema: executionSchema, Kind: "execution_intent", ExecutionID: intent.ExecutionID, InstructionID: m.InstructionID, EnvelopeDigest: instruction.EnvelopeDigest, BootID: bootID, OwnerPID: os.Getpid(), OwnerStartTime: start, LockGeneration: uint64(attempts), LockDevice: identity.Device, LockInode: identity.Inode, Intent: &intent, CreatedAt: now.UTC()}); err != nil {
		return release(err)
	}
	return &ExecutionLease{store: s, file: lock, intent: intent}, true, nil
}

type lockIdentity struct{ Device, Inode uint64 }

func lockIdentityOf(f *os.File) (lockIdentity, error) {
	if f == nil {
		return lockIdentity{}, errors.New("missing execution lock")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return lockIdentity{}, errors.New("unsafe execution lock")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Dev == 0 || st.Ino == 0 {
		return lockIdentity{}, errors.New("execution lock identity unavailable")
	}
	return lockIdentity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, nil
}

func verifyExecutionLockPath(path string, want lockIdentity) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("execution lock path changed or is unsafe")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Dev) != want.Device || uint64(st.Ino) != want.Inode {
		return errors.New("execution lock identity changed")
	}
	return nil
}

// positiveOwnerDeath rejects absence-of-contention heuristics. The caller
// must hold the exact lock inode exclusively; only boot change, proc absence,
// or a verified start-time mismatch proves the recorded owner is dead.
func positiveOwnerDeath(claim ExecutionFact, current lockIdentity) (bool, error) {
	if claim.Kind != "claim_acquired" || claim.OwnerPID <= 0 || claim.OwnerStartTime == "" || claim.BootID == "" || claim.LockDevice == 0 || claim.LockInode == 0 || claim.LockDevice != current.Device || claim.LockInode != current.Inode {
		return false, errors.New("execution owner identity is not bound to lock")
	}
	boot, err := linuxBootID()
	if err != nil {
		return false, errors.New("owner-death boot evidence unavailable")
	}
	if boot != claim.BootID {
		return true, nil
	}
	start, err := linuxProcessStartTime(claim.OwnerPID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, errors.New("owner-death proc evidence unavailable")
	}
	if start == claim.OwnerStartTime {
		return false, errors.New("execution owner identity is still live")
	}
	return true, nil
}

func executionID(now time.Time) string {
	b := fmt.Sprintf("%d-%d", now.UnixNano(), os.Getpid())
	h := sha256.Sum256([]byte(b))
	return fmt.Sprintf("exe_%x", h[:16])
}

func readExecutionFacts(path string) ([]ExecutionFact, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 256<<10 {
		return nil, errors.New("execution journal exceeds bound")
	}
	var facts []ExecutionFact
	claims := make(map[string]ExecutionFact)
	intents := make(map[string]ExecutionIntent)
	phase := make(map[string]uint8)
	for _, line := range splitLines(b) {
		if len(line) == 0 {
			continue
		}
		var fact ExecutionFact
		dec := json.NewDecoder(strings.NewReader(string(line)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fact); err != nil || fact.Schema != executionSchema || fact.Kind == "" || fact.ExecutionID == "" || !validExecutionKind(fact.Kind) || fact.CreatedAt.IsZero() {
			return nil, errors.New("corrupt execution journal")
		}
		switch fact.Kind {
		case "claim_acquired":
			if fact.InstructionID == "" || fact.EnvelopeDigest == "" || fact.BootID == "" || fact.OwnerPID <= 0 || fact.OwnerStartTime == "" || fact.LockGeneration == 0 || fact.LockDevice == 0 || fact.LockInode == 0 {
				return nil, errors.New("corrupt execution claim")
			}
			if _, exists := claims[fact.ExecutionID]; exists {
				return nil, errors.New("duplicate execution claim")
			}
			claims[fact.ExecutionID] = fact
		case "execution_intent":
			claim, claimed := claims[fact.ExecutionID]
			if fact.Intent == nil || fact.Intent.ExecutionID != fact.ExecutionID || fact.Intent.InstructionID != fact.InstructionID || fact.Intent.EnvelopeDigest != fact.EnvelopeDigest || fact.Intent.BootID != fact.BootID || fact.Intent.OwnerPID != fact.OwnerPID || fact.Intent.OwnerStartTime != fact.OwnerStartTime || fact.Intent.LockGeneration != fact.LockGeneration || fact.Intent.LockDevice != fact.LockDevice || fact.Intent.LockInode != fact.LockInode || fact.Intent.M2Provenance == "" || !claimed || claim.InstructionID != fact.InstructionID || claim.EnvelopeDigest != fact.EnvelopeDigest || claim.BootID != fact.BootID || claim.OwnerPID != fact.OwnerPID || claim.OwnerStartTime != fact.OwnerStartTime || claim.LockGeneration != fact.LockGeneration || claim.LockDevice != fact.LockDevice || claim.LockInode != fact.LockInode {
				return nil, errors.New("corrupt execution intent")
			}
			if _, exists := intents[fact.ExecutionID]; exists {
				return nil, errors.New("duplicate execution intent")
			}
			intents[fact.ExecutionID] = *fact.Intent
			phase[fact.ExecutionID] = 0
		case "m2_call_entered":
			intent, ok := intents[fact.ExecutionID]
			if !ok || phase[fact.ExecutionID] != 0 || !factMatchesIntent(fact, intent) {
				return nil, errors.New("invalid execution fact order")
			}
			phase[fact.ExecutionID] = 1
		case "m2_start_witnessed":
			intent, ok := intents[fact.ExecutionID]
			if !ok || phase[fact.ExecutionID] != 1 || !factMatchesIntent(fact, intent) || !validDigest(fact.M2StartDigest) {
				return nil, errors.New("invalid execution fact order")
			}
			phase[fact.ExecutionID] = 2
		case "terminal", "indeterminate", "outcome":
			intent, ok := intents[fact.ExecutionID]
			if !ok || phase[fact.ExecutionID] < 1 || phase[fact.ExecutionID] > 2 || !factMatchesIntent(fact, intent) {
				return nil, errors.New("invalid execution fact order")
			}
			phase[fact.ExecutionID] = 3
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

func factMatchesIntent(f ExecutionFact, i ExecutionIntent) bool {
	return f.InstructionID == i.InstructionID && f.EnvelopeDigest == i.EnvelopeDigest && f.BootID == i.BootID && f.OwnerPID == i.OwnerPID && f.OwnerStartTime == i.OwnerStartTime && f.LockGeneration == i.LockGeneration && f.LockDevice == i.LockDevice && f.LockInode == i.LockInode
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out, b = append(out, b[:i]), b[i+1:]
	}
	return out
}

func appendExecutionFact(path string, fact ExecutionFact) error {
	if err := ensureRegular(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("unsafe execution journal")
	}
	b, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

func appendPublicationFact(path string, fact PublicationFact) error {
	if err := ensureRegular(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("unsafe publication journal")
	}
	b, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = d.Sync()
		_ = d.Close()
	}
	return err
}

func validExecutionKind(kind string) bool {
	return kind == "claim_acquired" || kind == "execution_intent" || kind == "m2_call_entered" || kind == "m2_start_witnessed" || kind == "terminal" || kind == "indeterminate"
}

func linuxBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || len(strings.TrimSpace(string(b))) < 16 {
		return "", errors.New("boot id unavailable")
	}
	return strings.TrimSpace(string(b)), nil
}

func linuxProcessStartTime(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	parts := strings.Fields(string(b))
	if len(parts) < 22 {
		return "", errors.New("process start time unavailable")
	}
	return parts[21], nil
}

func (s *Store) MarkInstructionState(instructionID, envelopeDigest, state string, now time.Time) error {
	if !validClaimState(state) {
		return errors.New("invalid instruction claim state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return err
	}
	defer unlock()
	claim, ok := latest.Claims[instructionID]
	if !ok || claim.EnvelopeDigest != envelopeDigest {
		return errors.New("instruction claim is stale")
	}
	claim.State, claim.UpdatedAt = state, now.UTC()
	if state == "m2_call_entered" {
		claim.RetryNotBefore = now.UTC().Add(time.Second)
	} else {
		claim.RetryNotBefore = time.Time{}
	}
	latest.Claims[instructionID] = claim
	if err := s.writeLocked(latest); err != nil {
		return err
	}
	s.st = latest
	return nil
}

func (s *Store) Claim(instructionID string) (InstructionClaim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim, ok := s.st.Claims[instructionID]
	return claim, ok
}

func (s *Store) InstructionDigest(instructionID string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, message := range s.st.Inbox {
		if message.InstructionID != instructionID {
			continue
		}
		var instruction fleetcommander.Instruction
		if fleetcommander.DecodeClosed(message.Body, &instruction) == nil {
			return instruction.EnvelopeDigest, true
		}
	}
	return "", false
}

// Process invokes M2 only when no durable terminal record exists. A restart
// after M2 commit therefore repairs projections without dispatching Sol again.
func (s *Store) Process(ctx context.Context, adapter M2Adapter) error {
	return s.process(ctx, adapter, nil)
}

// ProcessIf is the generation-fenced variant used by the local assessment
// worker. The fence is checked before any projection is published. If a
// generation is superseded while M2 is finishing, M2 remains authoritative
// and the next generation repairs the projections from its durable record.
func (s *Store) ProcessIf(ctx context.Context, adapter M2Adapter, active func() bool) error {
	return s.process(ctx, adapter, active)
}

func (s *Store) process(ctx context.Context, adapter M2Adapter, active func() bool) error {
	if adapter == nil {
		return errors.New("missing M2 adapter")
	}
	s.mu.Lock()
	inbox := append([]fleetcommander.Message(nil), s.st.Inbox...)
	s.mu.Unlock()
	ack := s.currentInboxAck()
	for _, m := range inbox {
		if active != nil && !active() {
			return nil
		}
		var b fleetcommander.Instruction
		if fleetcommander.DecodeClosed(m.Body, &b) != nil {
			return errors.New("invalid instruction body")
		}
		var env delegation.Envelope
		if json.Unmarshal(b.Envelope, &env) != nil || env.DelegationID == "" {
			return errors.New("invalid delegation envelope")
		}
		// Keep the generation fence immediately adjacent to every durable
		// projection boundary. In particular, a superseded worker must not
		// publish a received event before it starts M2 work.
		if active != nil && !active() {
			return nil
		}
		s.emitReceived(m, env.DelegationID)
		record, ok := adapter.Lookup(env.DelegationID)
		if ok && record.EnvelopeDigest != b.EnvelopeDigest {
			// M2 owns delegation identity. Never project a durable record for a
			// different envelope merely because a hostile delivery reused its ID.
			return errors.New("M2 envelope disagreement")
		}
		if !ok && m.MailboxSequence <= ack {
			// The inbox was already acknowledged and no M2 lifecycle exists;
			// do not invent or rerun work during repair.
			continue
		}
		if !ok {
			if active != nil && !active() {
				return nil
			}
			if err := adapter.Accept(ctx, b.Envelope); err != nil {
				if record, ok = adapter.Lookup(env.DelegationID); !ok {
					return err
				}
			} else if record, ok = adapter.Lookup(env.DelegationID); !ok {
				return errors.New("M2 did not durably publish lifecycle")
			}
		}
		if record.Lifecycle == "assessment_started" {
			// Accept can return after a generation is superseded (for example,
			// after it durably reserved assessment_started). Do not let that
			// stale worker manufacture the restart projection; the next active
			// generation repairs it from the durable M2 record.
			if active != nil && !active() {
				return nil
			}
			if err := s.projectIndeterminate(m, env.DelegationID, record); err != nil {
				return err
			}
			if active != nil && !active() {
				return nil
			}
			if err := s.markInbox(m.MailboxSequence); err != nil {
				return err
			}
			continue
		}
		if active != nil && !active() {
			return nil
		}
		if err := s.projectRecord(m, env.DelegationID, record); err != nil {
			return err
		}
		if active != nil && !active() {
			return nil
		}
		if err := s.markInbox(m.MailboxSequence); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Repair(ctx context.Context, adapter M2Adapter) error { return s.Process(ctx, adapter) }

func (s *Store) currentInboxAck() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.st.InboxAck }
func (s *Store) markInbox(n uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.st.InboxAck {
		s.st.InboxAck = n
		return s.commit()
	}
	return nil
}

func (s *Store) emitReceived(m fleetcommander.Message, delegationID string) error {
	body, _ := json.Marshal(fleetcommander.Progress{Type: fleetcommander.ProgressType, DelegationID: delegationID, State: "received"})
	return s.emit(fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: opaque("rcv-" + m.InstructionID), InstructionID: m.InstructionID, FleetID: s.fleetID, ShipID: s.shipID, Direction: fleetcommander.ShipToFleet, ExpiresAt: m.ExpiresAt, Body: body})
}

func (s *Store) projectRecord(m fleetcommander.Message, delegationID string, r delegation.Record) error {
	switch string(r.Lifecycle) {
	case "accepted":
		return s.emitProgress(m, delegationID, "accepted")
	case "assessment_started":
		return s.emitProgress(m, delegationID, "assessing")
	case "advised", "rejected", "expired", "revoked", "indeterminate":
		result := r.Result
		if result == "" {
			switch string(r.Lifecycle) {
			case "rejected":
				result = "rejected"
			case "expired":
				result = "expired"
			case "revoked":
				result = "revoked"
			default:
				result = "indeterminate"
			}
		}
		p := fleetcommander.Completed{Type: fleetcommander.CompletedType, DelegationID: delegationID, Result: result, ReasonCode: string(r.ReasonCode), ProvenanceDigest: r.ProvenanceDigest, SailState: r.SailState}
		if result == "advised" {
			p.AdvisoryDecision = string(r.AdvisoryDecision)
		}
		if err := p.Validate(); err != nil {
			return err
		}
		body, _ := json.Marshal(p)
		return s.emit(fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: opaque("cmp-" + r.RecordID), InstructionID: m.InstructionID, FleetID: s.fleetID, ShipID: s.shipID, Direction: fleetcommander.ShipToFleet, ExpiresAt: m.ExpiresAt, Body: body})
	default:
		return errors.New("unknown M2 lifecycle")
	}
}

func (s *Store) projectIndeterminate(m fleetcommander.Message, delegationID string, r delegation.Record) error {
	digest := r.EnvelopeDigest
	if digest == "" {
		var err error
		digest, err = fleetcommander.MessageDigest(m)
		if err != nil {
			return err
		}
	}
	p := fleetcommander.Completed{Type: fleetcommander.CompletedType, DelegationID: delegationID, Result: "indeterminate", ReasonCode: "restart_after_assessment", ProvenanceDigest: digest, SailState: "not_evaluated"}
	if err := p.Validate(); err != nil {
		return err
	}
	body, _ := json.Marshal(p)
	return s.emit(fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: opaque("ind-" + delegationID), InstructionID: m.InstructionID, FleetID: s.fleetID, ShipID: s.shipID, Direction: fleetcommander.ShipToFleet, ExpiresAt: m.ExpiresAt, Body: body})
}
func (s *Store) emitProgress(m fleetcommander.Message, id, state string) error {
	body, _ := json.Marshal(fleetcommander.Progress{Type: fleetcommander.ProgressType, DelegationID: id, State: state})
	return s.emit(fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: opaque("prg-" + m.InstructionID + "-" + state), InstructionID: m.InstructionID, FleetID: s.fleetID, ShipID: s.shipID, Direction: fleetcommander.ShipToFleet, ExpiresAt: m.ExpiresAt, Body: body})
}
func (s *Store) emit(m fleetcommander.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.MailboxSequence == 0 {
		m.MailboxSequence = 1
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("invalid projection: %w", err)
	}
	m.MailboxSequence = 0
	for _, old := range s.st.Outbox {
		if old.MessageID == m.MessageID {
			if same(old, m) {
				return nil
			}
			return errors.New("projection conflict")
		}
	}
	if len(s.st.Outbox)-int(s.st.OutboxAck) >= MaxOutbox {
		return errors.New("delegation outbox backpressure")
	}
	s.st.NextOutbox++
	m.MailboxSequence = s.st.NextOutbox
	s.st.Outbox = append(s.st.Outbox, m)
	digest, err := fleetcommander.MessageDigest(m)
	if err != nil {
		return err
	}
	if s.st.Publication == nil {
		s.st.Publication = &publicationState{EventID: m.MessageID, EventDigest: digest, State: "projection_pending"}
	}
	if s.st.Projection == nil {
		s.st.Projection = &ProjectionLifecycle{State: "persisted"}
	}
	if s.st.Worker != nil {
		s.st.Projection.Generation = s.st.Worker.Generation
		s.st.Projection.WorkerEpoch = s.st.Worker.WorkerEpoch
	}
	s.st.Projection.State = "persisted"
	s.st.Projection.UpdatedAt = time.Now().UTC()
	return s.commit()
}

// AcquirePublicationLease claims the next unacknowledged projection and
// persists the claim under the mailbox lock. It is the only lease acquisition
// boundary; callers must use the returned epoch and nonce for every later
// transition.
func (s *Store) AcquirePublicationLease(generation uint64, now time.Time) (PublicationLease, bool, error) {
	if s == nil || generation == 0 {
		return PublicationLease{}, false, errors.New("invalid publication generation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return PublicationLease{}, false, err
	}
	defer unlock()
	if latest.Publication != nil && (latest.Publication.State == "publication_leased" || latest.Publication.State == "sent_unacknowledged") && latest.Publication.Generation == generation {
		for _, m := range latest.Outbox {
			if m.MessageID == latest.Publication.EventID {
				p := latest.Publication
				return PublicationLease{EventID: p.EventID, EventDigest: p.EventDigest, Generation: p.Generation, Epoch: p.Epoch, Nonce: p.Nonce, State: p.State, Message: m}, true, nil
			}
		}
		return PublicationLease{}, false, errors.New("publication lease event missing")
	}
	var event fleetcommander.Message
	for _, m := range latest.Outbox {
		if m.MailboxSequence > latest.OutboxAck {
			event = m
			break
		}
	}
	if event.MessageID == "" {
		return PublicationLease{}, false, nil
	}
	digest, err := fleetcommander.MessageDigest(event)
	if err != nil {
		return PublicationLease{}, false, err
	}
	epoch := uint64(1)
	if latest.Publication != nil {
		epoch = latest.Publication.Epoch + 1
	}
	nonce := fmt.Sprintf("pub-%d-%x", epoch, sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", event.MessageID, generation, epoch))))
	latest.Publication = &publicationState{EventID: event.MessageID, EventDigest: digest, Generation: generation, Epoch: epoch, Nonce: nonce, State: "publication_leased"}
	if err := s.writeLocked(latest); err != nil {
		return PublicationLease{}, false, err
	}
	s.st = latest
	return PublicationLease{EventID: event.MessageID, EventDigest: digest, Generation: generation, Epoch: epoch, Nonce: nonce, State: "publication_leased", Message: event}, true, nil
}

func (s *Store) validPublicationLeaseLocked(p *publicationState, l PublicationLease) bool {
	return p != nil && p.EventID == l.EventID && p.EventDigest == l.EventDigest && p.Generation == l.Generation && p.Epoch == l.Epoch && p.Nonce == l.Nonce
}

// MarkPublicationSent records that the leased immutable event was handed to
// Fleet. It intentionally does not advance the cursor.
func (s *Store) MarkPublicationSent(l PublicationLease, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.validPublicationLeaseLocked(latest.Publication, l) || (latest.Publication.State != "publication_leased" && latest.Publication.State != "sent_unacknowledged") {
		return errors.New("stale publication lease")
	}
	latest.Publication.State = "sent_unacknowledged"
	if err := s.writeLocked(latest); err != nil {
		return err
	}
	s.st = latest
	return nil
}

// ValidatePublicationLease is the final pre-send fence. The caller must use
// it immediately before handing the event to the transport.
func (s *Store) ValidatePublicationLease(l PublicationLease) error {
	if s == nil {
		return errors.New("nil mailbox")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.validPublicationLeaseLocked(latest.Publication, l) || (latest.Publication.State != "publication_leased" && latest.Publication.State != "sent_unacknowledged") {
		return errors.New("stale publication lease")
	}
	return nil
}

// AcknowledgePublication revalidates the lease and advances the outbox cursor
// in the same mailbox-lock transaction. Stale generations cannot acknowledge.
func (s *Store) AcknowledgePublication(l PublicationLease, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.validPublicationLeaseLocked(latest.Publication, l) || latest.Publication.State != "sent_unacknowledged" {
		return errors.New("stale publication lease")
	}
	found := false
	for _, m := range latest.Outbox {
		if m.MessageID == l.EventID && m.MailboxSequence > latest.OutboxAck {
			latest.OutboxAck = m.MailboxSequence
			found = true
			break
		}
	}
	if !found {
		return errors.New("publication event is not pending")
	}
	kept := latest.Outbox[:0]
	for _, m := range latest.Outbox {
		if m.MailboxSequence > latest.OutboxAck {
			kept = append(kept, m)
		}
	}
	latest.Outbox = kept
	latest.Publication.State = "acknowledged"
	if err := s.writeLocked(latest); err != nil {
		return err
	}
	s.st = latest
	return nil
}

// DetachPublicationLease atomically returns an in-flight projection to a
// replayable pending state. The immutable event and digest remain unchanged.
func (s *Store) DetachPublicationLease(l PublicationLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, unlock, err := s.lockedLatest()
	if err != nil {
		return err
	}
	defer unlock()
	if !s.validPublicationLeaseLocked(latest.Publication, l) {
		return nil
	}
	latest.Publication.Generation, latest.Publication.Nonce = 0, ""
	latest.Publication.State = "projection_pending"
	if err := s.writeLocked(latest); err != nil {
		return err
	}
	s.st = latest
	return nil
}

func (s *Store) PublicationLease() (PublicationLease, bool) {
	if s == nil {
		return PublicationLease{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Publication
	if p == nil || (p.State != "publication_leased" && p.State != "sent_unacknowledged") || p.Generation == 0 {
		return PublicationLease{}, false
	}
	for _, m := range s.st.Outbox {
		if m.MessageID == p.EventID {
			return PublicationLease{EventID: p.EventID, EventDigest: p.EventDigest, Generation: p.Generation, Epoch: p.Epoch, Nonce: p.Nonce, State: p.State, Message: m}, true
		}
	}
	return PublicationLease{}, false
}

func (s *Store) PullOutbox(ack uint64, limit int) ([]fleetcommander.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ack < s.st.OutboxAck || ack > s.st.NextOutbox {
		return nil, errors.New("invalid projection acknowledgement")
	}
	changed := false
	if ack > s.st.OutboxAck {
		s.st.OutboxAck = ack
		s.dropAcked()
		changed = true
	}
	if limit <= 0 || limit > MaxOutbox {
		limit = MaxOutbox
	}
	out := []fleetcommander.Message{}
	for _, m := range s.st.Outbox {
		if m.MailboxSequence <= s.st.OutboxAck {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, m)
	}
	if changed {
		if err := s.commit(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func (s *Store) AckOutbox(n uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < s.st.OutboxAck || n > s.st.NextOutbox {
		return errors.New("invalid projection acknowledgement")
	}
	s.st.OutboxAck = n
	s.dropAcked()
	return s.commit()
}
func (s *Store) dropAcked() {
	kept := s.st.Outbox[:0]
	for _, m := range s.st.Outbox {
		if m.MailboxSequence > s.st.OutboxAck {
			kept = append(kept, m)
		}
	}
	s.st.Outbox = kept
}
func same(a, b fleetcommander.Message) bool {
	a.MailboxSequence = 1
	b.MailboxSequence = 1
	x, _ := fleetcommander.MessageDigest(a)
	y, _ := fleetcommander.MessageDigest(b)
	return x == y
}
func opaque(s string) string {
	if len(s) >= 16 {
		return s[:min(96, len(s))]
	}
	return "m3-" + s + "-000000000000"
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func validID(s string) bool {
	if len(s) < 16 || len(s) > 96 {
		return false
	}
	for i, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || i > 0 && (c == '_' || c == '-')) {
			return false
		}
	}
	return true
}
func ensureRegular(p string) error {
	info, e := os.Lstat(p)
	if os.IsNotExist(e) {
		fd, x := unix.Open(p, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if x != nil {
			return x
		}
		unix.Close(fd)
		return nil
	}
	if e != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("unsafe delegation mailbox file")
	}
	return nil
}
func safeParents(d string) error {
	if e := os.MkdirAll(d, 0700); e != nil {
		return e
	}
	for p := d; ; p = filepath.Dir(p) {
		i, e := os.Lstat(p)
		if e != nil || i.Mode()&os.ModeSymlink != 0 || !i.IsDir() {
			return errors.New("unsafe delegation mailbox directory")
		}
		if p == d && i.Mode().Perm() != 0700 {
			return errors.New("delegation mailbox directory permissions")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func (s *Store) load() error {
	b, e := os.ReadFile(s.path)
	if e != nil {
		return e
	}
	if len(b) == 0 {
		return s.commit()
	}
	d, err := decodeState(b, s.fleetID, s.shipID)
	if err != nil {
		return errors.New("corrupt delegation mailbox")
	}
	for _, m := range append(append([]fleetcommander.Message{}, d.Inbox...), d.Outbox...) {
		if m.Validate() != nil {
			return errors.New("corrupt delegation mailbox message")
		}
	}
	s.st = d
	return nil
}

func decodeState(b []byte, fleetID, shipID string) (state, error) {
	var d state
	if fleetcommander.DecodeClosed(b, &d) != nil || d.Schema != StateSchema || d.FleetID != fleetID || d.ShipID != shipID || len(d.Inbox) > MaxInbox || len(d.Outbox) > MaxOutbox {
		return state{}, errors.New("invalid mailbox state")
	}
	if d.Worker != nil && (d.Worker.Schema != 1 || d.Worker.Generation == 0 || d.Worker.WorkerEpoch == 0 || d.Worker.Attempts > MaxWorkerPreStartAttempts || !validWorkerState(d.Worker.State) || (d.Worker.OutcomeCode != "" && !validWorkerCode(d.Worker.OutcomeCode))) {
		return state{}, errors.New("invalid worker lease")
	}
	if d.Projection != nil && (d.Projection.State == "" || d.Projection.Generation == 0 && d.Projection.WorkerEpoch != 0) {
		return state{}, errors.New("invalid projection lifecycle")
	}
	if d.Publication != nil {
		p := d.Publication
		if p.EventID == "" || !validDigest(p.EventDigest) || !validPublicationState(p.State) || (p.State != "projection_pending" && p.Epoch == 0) || (p.State == "publication_leased" && (p.Generation == 0 || p.Nonce == "")) {
			return state{}, errors.New("invalid publication lease")
		}
	}
	for id, claim := range d.Claims {
		if id == "" || claim.InstructionID != id || claim.EnvelopeDigest == "" || claim.Attempts == 0 || claim.Attempts > MaxWorkerPreStartAttempts || !validClaimState(claim.State) {
			return state{}, errors.New("invalid instruction claim")
		}
	}
	return d, nil
}

func (s *Store) lockedLatest() (state, func(), error) {
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return state{}, nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		_ = lock.Close()
		return state{}, nil, err
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return state{}, nil, err
	}
	latest, err := decodeState(b, s.fleetID, s.shipID)
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return state{}, nil, err
	}
	return latest, func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }, nil
}

func (s *Store) writeLocked(value state) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".delegation-mailbox-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(append(b, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func boundedCode(code string) string {
	if len(code) > 64 {
		return code[:64]
	}
	return code
}

func validWorkerState(state string) bool {
	return state == "leased" || state == "running" || state == "completed" || state == "failed_before_start" || state == "indeterminate" || state == "released"
}

func validWorkerCode(code string) bool {
	return code == "worker_cancelled" || code == "worker_error" || code == "worker_panic" || code == "worker_lease_start_failed" || code == "assessment_terminal" || code == ""
}

func validClaimState(state string) bool {
	return state == "m2_call_entered" || state == "m2_started_observed" || state == "terminal" || state == "indeterminate" || state == "projection_pending" || state == "publication_claimed" || state == "published" || state == "acknowledged"
}

func validPublicationState(state string) bool {
	return state == "projection_pending" || state == "publication_leased" || state == "sent_unacknowledged" || state == "acknowledged"
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
func (s *Store) commit() error {
	lock, e := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX); e != nil {
		return e
	}
	// Preserve claims written by another Store instance while this instance
	// commits an unrelated cursor or projection update.
	if raw, readErr := os.ReadFile(s.path); readErr == nil && len(raw) > 0 {
		if latest, decodeErr := decodeState(raw, s.fleetID, s.shipID); decodeErr == nil {
			if latest.Claims != nil {
				if s.st.Claims == nil {
					s.st.Claims = make(map[string]InstructionClaim)
				}
				for id, claim := range latest.Claims {
					if current, ok := s.st.Claims[id]; !ok || current.UpdatedAt.Before(claim.UpdatedAt) {
						s.st.Claims[id] = claim
					}
				}
			}
		}
	}
	b, e := json.Marshal(s.st)
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(filepath.Dir(s.path), ".delegation-mailbox-")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e != nil {
		return e
	}
	if _, e = tmp.Write(append(b, '\n')); e != nil {
		return e
	}
	if e = tmp.Sync(); e != nil {
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, s.path); e != nil {
		return e
	}
	f, e := os.Open(filepath.Dir(s.path))
	if e == nil {
		defer f.Close()
		e = f.Sync()
	}
	return e
}
