// Package fleetcommandermailbox is the Fleet-owned durable half of M3. It is
// deliberately local: transport adapters may call it, but it owns no tunnel.
package fleetcommandermailbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/project"
	"golang.org/x/sys/unix"
)

const (
	StateSchema            = 1
	DefaultMaxInstructions = 16
	DefaultMaxEvents       = 32
	maxStateBytes          = 4 << 20
)

type Limits struct{ MaxInstructions, MaxEvents int }
type persisted struct {
	Schema             int                      `json:"schema"`
	FleetID            string                   `json:"fleet_id"`
	ShipID             string                   `json:"ship_id"`
	NextFleetSequence  uint64                   `json:"next_fleet_sequence"`
	NextShipSequence   uint64                   `json:"next_ship_sequence"`
	FleetAck           uint64                   `json:"fleet_ack"`
	ShipAck            uint64                   `json:"ship_ack"`
	Instructions       []fleetcommander.Message `json:"instructions"`
	Events             []fleetcommander.Message `json:"events"`
	InstructionDigests map[string]string        `json:"instruction_digests,omitempty"`
}

type Store struct {
	mu                              sync.Mutex
	path, lockPath, fleetID, shipID string
	limits                          Limits
	state                           persisted
}

func Open(root, fleetID, shipID string, limits Limits) (*Store, error) {
	if !validID(fleetID) || !validID(shipID) {
		return nil, errors.New("invalid mailbox identity")
	}
	if limits.MaxInstructions == 0 {
		limits.MaxInstructions = DefaultMaxInstructions
	}
	if limits.MaxEvents == 0 {
		limits.MaxEvents = DefaultMaxEvents
	}
	if limits.MaxInstructions < 1 || limits.MaxInstructions > DefaultMaxInstructions || limits.MaxEvents < 1 || limits.MaxEvents > DefaultMaxEvents {
		return nil, errors.New("invalid mailbox limits")
	}
	canonical, err := project.CanonicalRoot(root)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(canonical, ".shipmates", "fleetcommandermailbox")
	if err := safeParents(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, fleetID+"-"+shipID+".json")
	if err := ensureRegular(path); err != nil {
		return nil, err
	}
	s := &Store{path: path, lockPath: path + ".lock", fleetID: fleetID, shipID: shipID, limits: limits, state: persisted{Schema: StateSchema, FleetID: fleetID, ShipID: shipID, InstructionDigests: map[string]string{}}}
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

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return errors.New("mailbox state exceeds bound")
	}
	if len(b) == 0 {
		return s.commit()
	}
	var d persisted
	if fleetcommander.DecodeClosed(b, &d) != nil || d.Schema != StateSchema || d.FleetID != s.fleetID || d.ShipID != s.shipID || len(d.Instructions) > s.limits.MaxInstructions || len(d.Events) > s.limits.MaxEvents {
		return errors.New("corrupt mailbox state")
	}
	for _, m := range d.Instructions {
		if m.Validate() != nil || m.FleetID != s.fleetID || m.ShipID != s.shipID || m.Direction != fleetcommander.FleetToShip {
			return errors.New("corrupt mailbox instruction")
		}
	}
	for _, m := range d.Events {
		if m.Validate() != nil || m.FleetID != s.fleetID || m.ShipID != s.shipID || m.Direction != fleetcommander.ShipToFleet {
			return errors.New("corrupt mailbox event")
		}
	}
	s.state = d
	if s.state.InstructionDigests == nil {
		s.state.InstructionDigests = map[string]string{}
	}
	return nil
}

func (s *Store) EnqueueInstruction(principal fleetidentity.CommanderPrincipal, m fleetcommander.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if principal.Capability != fleetidentity.CommanderDelegateCapability || principal.FleetID != s.fleetID || principal.CredentialGeneration == 0 || !contains(principal.ShipIDs, s.shipID) {
		return errors.New("commander capability refused")
	}
	if m.MailboxSequence == 0 {
		m.MailboxSequence = 1
	}
	if err := m.Validate(); err != nil || m.Direction != fleetcommander.FleetToShip || m.FleetID != s.fleetID || m.ShipID != s.shipID {
		return errors.New("invalid instruction")
	}
	m.MailboxSequence = 0
	if !time.Now().UTC().Before(m.ExpiresAt) {
		return errors.New("instruction expired")
	}
	fingerprint := messageFingerprint(m)
	if old, exists := s.state.InstructionDigests[m.InstructionID]; exists {
		if old == fingerprint {
			return nil
		}
		return errors.New("instruction conflict")
	}
	if s.unackedInstructions() >= s.limits.MaxInstructions {
		return errors.New("backpressure")
	}
	s.state.NextFleetSequence++
	m.MailboxSequence = s.state.NextFleetSequence
	s.state.Instructions = append(s.state.Instructions, m)
	s.state.InstructionDigests[m.InstructionID] = fingerprint
	return s.commit()
}

func (s *Store) Pull(fleetAck uint64) (*fleetcommander.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fleetAck < s.state.FleetAck || fleetAck > s.state.NextFleetSequence {
		return nil, errors.New("invalid fleet acknowledgement")
	}
	changed := false
	if fleetAck > s.state.FleetAck {
		s.state.FleetAck = fleetAck
		changed = s.dropAckedInstructions() || changed
	}
	now := time.Now().UTC()
	// A cumulative cursor may move only across the contiguous head. Expiring a
	// later row must not acknowledge/drop an earlier still-live instruction.
	for _, message := range s.state.Instructions {
		if message.MailboxSequence != s.state.FleetAck+1 || now.Before(message.ExpiresAt) {
			break
		}
		s.state.FleetAck = message.MailboxSequence
		changed = true
	}
	if changed {
		changed = s.dropAckedInstructions() || changed
	}
	var next *fleetcommander.Message
	for _, m := range s.state.Instructions {
		if m.MailboxSequence > s.state.FleetAck {
			x := m
			next = &x
			break
		}
	}
	if changed {
		if err := s.commit(); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func (s *Store) PullCommander(fleetAck uint64) (*fleetcommander.Message, error) {
	return s.Pull(fleetAck)
}
func (s *Store) AckCommander(fleetAck uint64) error                  { return s.AckFleet(fleetAck) }
func (s *Store) IngestCommanderEvent(m fleetcommander.Message) error { return s.IngestEvent(m) }

// PurgeUndelivered is called after Commander capability revocation. It keeps
// acknowledged history and all ship event projections intact.
func (s *Store) PurgeUndelivered() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.state.Instructions[:0]
	for _, message := range s.state.Instructions {
		if message.MailboxSequence <= s.state.FleetAck {
			kept = append(kept, message)
		}
	}
	s.state.Instructions = kept
	return s.commit()
}

func (s *Store) AckFleet(sequence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence < s.state.FleetAck || sequence > s.state.NextFleetSequence {
		return errors.New("invalid fleet acknowledgement")
	}
	s.state.FleetAck = sequence
	s.dropAckedInstructions()
	return s.commit()
}

func (s *Store) IngestEvent(m fleetcommander.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := m.Validate(); err != nil || m.Direction != fleetcommander.ShipToFleet || m.FleetID != s.fleetID || m.ShipID != s.shipID {
		return errors.New("invalid ship event")
	}
	for _, old := range s.state.Events {
		if old.MailboxSequence == m.MailboxSequence {
			if sameMessage(old, m) {
				return nil
			}
			return errors.New("event sequence conflict")
		}
	}
	if m.MailboxSequence == 0 {
		s.state.NextShipSequence++
		m.MailboxSequence = s.state.NextShipSequence
	} else if m.MailboxSequence != s.state.NextShipSequence+1 {
		return errors.New("event sequence gap")
	} else {
		s.state.NextShipSequence = m.MailboxSequence
	}
	if len(s.state.Events) >= s.limits.MaxEvents {
		if mbodyCompleted(m) {
			return errors.New("completed event retention backpressure")
		}
		return errors.New("backpressure")
	}
	// Progress coalescing happens at the ship outbox via deterministic
	// projection keys. Fleet must persist every sequence it acknowledges: doing
	// it here after incrementing NextShipSequence would create an acknowledged
	// cursor gap and permit an ACK to discard an unstored event.
	s.state.Events = append(s.state.Events, m)
	return s.commit()
}

func (s *Store) Events(after uint64, limit int) ([]fleetcommander.Message, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > s.limits.MaxEvents {
		limit = s.limits.MaxEvents
	}
	out := []fleetcommander.Message{}
	next := after
	for _, m := range s.state.Events {
		if m.MailboxSequence <= after {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, m)
		next = m.MailboxSequence
	}
	return out, next, nil
}
func (s *Store) AckEvents(sequence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence < s.state.ShipAck || sequence > s.state.NextShipSequence {
		return errors.New("invalid ship acknowledgement")
	}
	s.state.ShipAck = sequence
	kept := s.state.Events[:0]
	for _, m := range s.state.Events {
		if m.MailboxSequence > sequence {
			kept = append(kept, m)
		}
	}
	s.state.Events = kept
	return s.commit()
}

func (s *Store) unackedInstructions() int {
	n := 0
	for _, m := range s.state.Instructions {
		if m.MailboxSequence > s.state.FleetAck {
			n++
		}
	}
	return n
}
func (s *Store) dropAckedInstructions() bool {
	kept := s.state.Instructions[:0]
	changed := false
	for _, m := range s.state.Instructions {
		if m.MailboxSequence <= s.state.FleetAck {
			changed = true
		} else {
			kept = append(kept, m)
		}
	}
	s.state.Instructions = kept
	return changed
}
func sameMessage(a, b fleetcommander.Message) bool {
	a.MailboxSequence = 0
	b.MailboxSequence = 0
	x, _ := fleetcommander.MessageDigest(a)
	y, _ := fleetcommander.MessageDigest(b)
	return x == y
}
func messageFingerprint(m fleetcommander.Message) string {
	m.MailboxSequence = 1
	d, _ := fleetcommander.MessageDigest(m)
	return d
}
func sameProgress(a, b fleetcommander.Message) bool {
	var x, y fleetcommander.Progress
	_ = json.Unmarshal(a.Body, &x)
	_ = json.Unmarshal(b.Body, &y)
	return x.DelegationID == y.DelegationID && x.State == y.State
}
func mbodyCompleted(m fleetcommander.Message) bool {
	var x struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(m.Body, &x)
	return x.Type == fleetcommander.CompletedType
}

func (s *Store) commit() error {
	if err := safeParents(filepath.Dir(s.path)); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	b, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return errors.New("mailbox state exceeds bound")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".mailbox-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.path))
}

func validID(s string) bool { return len(s) >= 16 && len(s) <= 96 && idChars(s) }
func idChars(s string) bool {
	for i, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || i > 0 && (c == '_' || c == '-')) {
			return false
		}
	}
	return true
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func ensureRegular(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		f, e := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return e
		}
		unix.Close(f)
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("unsafe mailbox file")
	}
	return nil
}
func safeParents(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe mailbox directory")
		}
		if p == dir && info.Mode().Perm() != 0700 {
			return fmt.Errorf("mailbox directory must be owner-only")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
