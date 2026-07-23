package fleetcommandermailbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/delegation"
	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
)

func TestMailboxDurableReplayConflictAndCapacity(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship, Limits{MaxInstructions: 2, MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	p := fleetidentity.CommanderPrincipal{FleetID: fleet, CredentialID: "cdc_0123456789abcdef", Capability: fleetidentity.CommanderDelegateCapability, CredentialGeneration: 1, ShipIDs: []string{ship}}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	d, _ := delegation.EnvelopeDigest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: d, Envelope: raw})
	m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_0123456789abcdef", InstructionID: "ins_0123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, ExpiresAt: time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond), Body: body}
	if err := s.EnqueueInstruction(p, m); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueInstruction(p, m); err != nil {
		t.Fatal("identical instruction was not idempotent")
	}
	got, err := s.Pull(0)
	if err != nil || got == nil || got.MailboxSequence != 1 {
		t.Fatalf("pull=%+v err=%v", got, err)
	}
	if err := s.AckFleet(1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Pull(1); got != nil {
		t.Fatal("acknowledged instruction replayed")
	}
	conflict := m
	conflict.MessageID = "msg_1123456789abcdef"
	conflict.InstructionID = m.InstructionID
	conflict.Body = body
	if err := s.EnqueueInstruction(p, conflict); err == nil {
		t.Fatal("instruction conflict accepted")
	}

	progressBody, _ := json.Marshal(fleetcommander.Progress{Type: fleetcommander.ProgressType, DelegationID: "dlg_0123456789abcdef", State: "received"})
	e := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "evt_0123456789abcdef", InstructionID: m.InstructionID, FleetID: fleet, ShipID: ship, Direction: fleetcommander.ShipToFleet, MailboxSequence: 1, ExpiresAt: m.ExpiresAt, Body: progressBody}
	if err := s.IngestEvent(e); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestEvent(e); err != nil {
		t.Fatal("event replay not idempotent")
	}
	if err := s.AckEvents(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(s.Path()); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("mailbox mode=%v err=%v", st.Mode(), err)
	}
}

func TestMailboxRejectsSymlinkState(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".shipmates", "fleetcommandermailbox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "flt_0123456789abcdef-shp_0123456789abcdef.json")
	if err := os.Symlink(filepath.Join(root, "other"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "flt_0123456789abcdef", "shp_0123456789abcdef", Limits{}); err == nil {
		t.Fatal("symlink mailbox state accepted")
	}
}

func TestPullRejectsRollbackAndFutureAcknowledgements(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pull(1); err == nil {
		t.Fatal("future pull acknowledgement accepted")
	}
	if err := s.AckFleet(0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pull(0); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryCannotAcknowledgePastEarlierLiveInstruction(t *testing.T) {
	root := t.TempDir()
	fleet, ship := "flt_0123456789abcdef", "shp_0123456789abcdef"
	s, err := Open(root, fleet, ship, Limits{MaxInstructions: 2, MaxEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	p := fleetidentity.CommanderPrincipal{FleetID: fleet, CredentialID: "cdc_0123456789abcdef", Capability: fleetidentity.CommanderDelegateCapability, CredentialGeneration: 1, ShipIDs: []string{ship}}
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	d, _ := delegation.EnvelopeDigest(raw)
	body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: d, Envelope: raw})
	live := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_2123456789abcdef", InstructionID: "ins_2123456789abcdef", FleetID: fleet, ShipID: ship, Direction: fleetcommander.FleetToShip, ExpiresAt: time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond), Body: body}
	if err := s.EnqueueInstruction(p, live); err != nil {
		t.Fatal(err)
	}
	expired := live
	expired.MessageID, expired.InstructionID = "msg_3123456789abcdef", "ins_3123456789abcdef"
	expired.ExpiresAt = time.Now().UTC().Add(-time.Millisecond).Truncate(time.Millisecond)
	// Admission correctly refuses already-expired instructions. Model a row
	// expiring after admission to exercise cursor advancement under the normal
	// persisted ordering.
	expired.MailboxSequence = 2
	s.state.NextFleetSequence = 2
	s.state.Instructions = append(s.state.Instructions, expired)
	if got, err := s.Pull(0); err != nil || got == nil || got.MailboxSequence != 1 {
		t.Fatalf("live head was discarded: got=%+v err=%v", got, err)
	}
}
