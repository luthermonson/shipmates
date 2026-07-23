package fleetcommander

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrozenInstructionAndCarrierCodecs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := envelopeDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(Instruction{Type: InstructionType, EnvelopeDigest: d, Envelope: raw})
	m := Message{Schema: MessageSchema, MessageID: "msg_0123456789abcdef", InstructionID: "ins_0123456789abcdef", FleetID: "flt_0123456789abcdef", ShipID: "shp_0123456789abcdef", Direction: FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 18, 0, 0, time.UTC), Body: body}
	encoded, err := MarshalMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Body) != string(body) {
		t.Fatal("instruction envelope was not retained")
	}
	carrier := Carrier{Schema: CarrierSchema, FleetID: m.FleetID, ShipID: m.ShipID, ConnectionGeneration: 1, Type: "fleet.delivery.v1", Message: &decoded}
	cb, _ := json.Marshal(carrier)
	if _, err := DecodeCarrier(cb); err != nil {
		t.Fatal(err)
	}
	progress, _ := json.Marshal(Progress{Type: ProgressType, DelegationID: "dlg_0123456789abcdef", State: "accepted"})
	event := Message{Schema: MessageSchema, MessageID: "evt_0123456789abcdef", InstructionID: m.InstructionID, FleetID: m.FleetID, ShipID: m.ShipID, Direction: ShipToFleet, MailboxSequence: 1, ExpiresAt: m.ExpiresAt, Body: progress}
	ecb, _ := json.Marshal(Carrier{Schema: CarrierSchema, FleetID: m.FleetID, ShipID: m.ShipID, ConnectionGeneration: 1, Type: "ship.event.v1", FleetToShipAck: 1, ShipToFleetAck: 0, Message: &event})
	if got, err := DecodeCarrier(ecb); err != nil || got.Type != "ship.event.v1" {
		t.Fatalf("ship event carrier=%+v err=%v", got, err)
	}
}

func TestClosedCodecRejectsDuplicatesUnknownAndProjectionMismatch(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
	d, _ := envelopeDigest(raw)
	body, _ := json.Marshal(Instruction{Type: InstructionType, EnvelopeDigest: d, Envelope: raw})
	m := Message{Schema: MessageSchema, MessageID: "msg_0123456789abcdef", InstructionID: "ins_0123456789abcdef", FleetID: "flt_0123456789abcdef", ShipID: "shp_0123456789abcdef", Direction: FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 18, 0, 0, time.UTC), Body: body}
	b, _ := json.Marshal(m)
	if _, err := DecodeMessage(append(b[:len(b)-1], []byte(`,"extra":1}`)...)); err == nil {
		t.Fatal("unknown message field accepted")
	}
	badBody := []byte(`{"type":"commander.instruction.v1","type":"commander.instruction.v1","envelope_digest":"` + d + `","envelope":{}}`)
	m.Body = badBody
	b, _ = json.Marshal(m)
	if _, err := DecodeMessage(b); err == nil {
		t.Fatal("duplicate body key accepted")
	}
	completed := Completed{Type: CompletedType, DelegationID: "dlg_0123456789abcdef", Result: "rejected", ReasonCode: "advised", ProvenanceDigest: strings.Repeat("a", 64), SailState: "not_evaluated"}
	if err := completed.Validate(); err == nil {
		t.Fatal("invalid result/reason combination accepted")
	}
}

func TestCarrierRejectsCrossPartitionBusinessMessage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m3", "valid-ship-event.json"))
	if err != nil {
		t.Fatal(err)
	}
	var carrier map[string]any
	if err := json.Unmarshal(raw, &carrier); err != nil {
		t.Fatal(err)
	}
	carrier["ship_id"] = "shp_1123456789abcdef"
	mutated, _ := json.Marshal(carrier)
	if _, err := DecodeCarrier(mutated); err == nil {
		t.Fatal("cross-ship event carrier accepted")
	}
}
