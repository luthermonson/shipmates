package fleettunnel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

type commanderChannel struct {
	mu        sync.Mutex
	responses [][]byte
}

func (c *commanderChannel) Send(_ context.Context, raw []byte) error {
	var carrier fleetcommander.Carrier
	if _, err := fleetcommander.DecodeCarrier(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &carrier); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch carrier.Type {
	case "ship.pull.v1":
		rawEnvelope, _ := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "fleet-commander-m1", "valid-offer.json"))
		body, _ := json.Marshal(fleetcommander.Instruction{Type: fleetcommander.InstructionType, EnvelopeDigest: "838796163412b44d5a3822edf117c20edcca04eedd9d6dd8b97a5aa9bcb6cd93", Envelope: rawEnvelope})
		m := fleetcommander.Message{Schema: fleetcommander.MessageSchema, MessageID: "msg_0123456789abcdef", InstructionID: "ins_0123456789abcdef", FleetID: carrier.FleetID, ShipID: carrier.ShipID, Direction: fleetcommander.FleetToShip, MailboxSequence: 1, ExpiresAt: time.Date(2026, 7, 14, 19, 30, 0, 0, time.UTC), Body: body}
		b, _ := json.Marshal(fleetcommander.Carrier{Schema: fleetcommander.CarrierSchema, FleetID: carrier.FleetID, ShipID: carrier.ShipID, ConnectionGeneration: carrier.ConnectionGeneration, Type: "fleet.delivery.v1", FleetToShipAck: 1, ShipToFleetAck: carrier.ShipToFleetAck, Message: &m})
		c.responses = append(c.responses, b)
	case "mailbox.ack.v1":
		b, _ := json.Marshal(fleetcommander.Carrier{Schema: fleetcommander.CarrierSchema, FleetID: carrier.FleetID, ShipID: carrier.ShipID, ConnectionGeneration: carrier.ConnectionGeneration, Type: "mailbox.ack.v1", FleetToShipAck: carrier.FleetToShipAck, ShipToFleetAck: carrier.ShipToFleetAck})
		c.responses = append(c.responses, b)
	}
	return nil
}
func (c *commanderChannel) Receive(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.responses) == 0 {
		return nil, context.DeadlineExceeded
	}
	b := c.responses[0]
	c.responses = c.responses[1:]
	return b, nil
}
func (c *commanderChannel) PeerServiceIdentity() string { return "fleet" }
func (c *commanderChannel) Close() error                { return nil }

func TestPullCommanderPersistsBeforeTransportAck(t *testing.T) {
	ch := &commanderChannel{}
	delivered := false
	ack, err := PullCommander(context.Background(), ch, "flt_0123456789abcdef", "shp_0123456789abcdef", 1, 0, 0, func(m fleetcommander.Message) error {
		delivered = true
		if m.MailboxSequence != 1 {
			return context.Canceled
		}
		return nil
	})
	if err != nil || ack != 1 || !delivered {
		t.Fatalf("ack=%d delivered=%v err=%v", ack, delivered, err)
	}
}

func TestQualifyRequiresM7AndCommanderCapabilities(t *testing.T) {
	clientCh, serverCh := pair()
	clock := &fakeClock{now: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	client, err := NewClient(ClientConfig{FleetID: "flt_0123456789abcdef", ServiceIdentity: "fleet-service", CredentialID: "cred_0123456789abcdef", Secret: strings.Repeat("s", 24), ShipID: "shp_0123456789abcdef", IOTimeout: time.Second, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer serverCh.Close()
		challenge, _ := json.Marshal(Challenge{Version: 1, FleetID: "flt_0123456789abcdef", Nonce: "nonce", ExpiresAt: clock.Now().Add(time.Minute), ServiceIdentity: "fleet-service"})
		_ = serverCh.Send(context.Background(), challenge)
		if _, err := serverCh.Receive(context.Background()); err != nil {
			return
		}
		accepted, _ := json.Marshal(Accepted{Version: 1, ShipID: "shp_0123456789abcdef", Generation: 1, LeaseMillis: 1000, Capabilities: []string{CapabilityObserverOnly, "fleet.snapshot.v1", "fleet.events.v1", "fleet.gaps.v1", CapabilityCommanderMailbox, CapabilityCommanderDelegate}})
		_ = serverCh.Send(context.Background(), accepted)
	}()
	if _, err := client.Qualify(context.Background(), clientCh); err != nil {
		t.Fatal(err)
	}
}

func TestQualifyRejectsDuplicateCapabilities(t *testing.T) {
	if hasAcceptedCapabilities([]string{CapabilityObserverOnly, string(fleetobserve.CapabilitySnapshotV1), string(fleetobserve.CapabilityEventsV1), string(fleetobserve.CapabilityGapsV1), CapabilityCommanderMailbox, CapabilityCommanderMailbox}) {
		t.Fatal("duplicate negotiated capability accepted")
	}
}

func TestCommanderLocalErrorSanitizesWrappedDetails(t *testing.T) {
	err := (&CommanderLocalError{Err: errors.New("/secret/prompt-and-token")}).Error()
	if err != "commander_local" || strings.Contains(err, "secret") || strings.Contains(err, "token") {
		t.Fatalf("unsanitized local error %q", err)
	}
}
