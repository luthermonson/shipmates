package fleetsteer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/livesession"
)

// TestShipEndpointConstructorsNeverSilentlyDisableRevalidation locks the shape
// of the fix for a production endpoint that carried an authority which always
// failed, plus a boolean that skipped the only check that authority existed for.
func TestShipEndpointConstructorsNeverSilentlyDisableRevalidation(t *testing.T) {
	production, err := NewProductionShipEndpoint("fleet", "ship", 2, 3, ShipEndpointConfig{}, &LocalControl{})
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := NewAuthenticatedShipEndpoint("fleet", "ship", 2, 3, nil, &livesession.RemoteSteerCoordinator{}, &livesession.Manager{})
	if err != nil {
		t.Fatal(err)
	}
	for name, ep := range map[string]*ShipEndpoint{"production": production, "authenticated": authenticated} {
		if ep.authority != nil {
			t.Fatalf("%s endpoint claims an authority it cannot consult", name)
		}
	}
	if _, err := NewShipEndpoint("fleet", "ship", 2, 3, nil, nil, &livesession.RemoteSteerCoordinator{}, &livesession.Manager{}); err == nil {
		t.Fatal("the revalidating constructor accepted a nil authority")
	}
}

// TestProductionShipEndpointEnforcesCapabilityAndShipScope covers the checks the
// ship can make with no credential store at all. Before the fix Deliver read
// only the target reference: it never looked at the delivered capability, so an
// interrupt-only principal could steer, and DeliverInterrupt inspected nothing.
func TestProductionShipEndpointEnforcesCapabilityAndShipScope(t *testing.T) {
	ep, err := NewProductionShipEndpoint("fleet", "ship", 2, 3, ShipEndpointConfig{}, &LocalControl{})
	if err != nil {
		t.Fatal(err)
	}
	complete := shipScopedPrincipal(livesession.RemoteSteerCapability)
	request := livesession.RemoteSteerRequest{OperationID: "op", FleetID: "fleet", ShipID: "ship", FleetEpoch: 2, ConnectionGeneration: 3, TargetReference: "absent"}
	wrongCapability := complete
	wrongCapability.Capability = fleetidentity.InterruptTurnCapability
	foreignShip := complete
	foreignShip.ShipIDs = []string{"other-ship"}
	foreignFleet := complete
	foreignFleet.FleetID = "other-fleet"
	noGeneration := complete
	noGeneration.CredentialGeneration = 0
	anonymous := complete
	anonymous.SubjectID = ""
	for name, operator := range map[string]fleetidentity.OperatorPrincipal{
		"interrupt-capability": wrongCapability,
		"empty-capability":     {FleetID: "fleet", SubjectID: "s", CredentialID: "c", CredentialGeneration: 1, ShipIDs: []string{"ship"}},
		"foreign-ship":         foreignShip,
		"foreign-fleet":        foreignFleet,
		"no-generation":        noGeneration,
		"anonymous":            anonymous,
	} {
		if got := ep.Deliver(context.Background(), DeliveryV1{operator, request}); got.ReasonCode != "unauthorized" {
			t.Fatalf("%s steer principal was admitted: %+v", name, got)
		}
	}
	// A complete grant still reaches target resolution, so the new checks are
	// not a blanket refusal.
	if got := ep.Deliver(context.Background(), DeliveryV1{complete, request}); got.ReasonCode != "stale_target" {
		t.Fatalf("complete steer grant refused before target resolution: %+v", got)
	}
	steerOnly := shipScopedPrincipal(livesession.RemoteSteerCapability)
	interruptRequest := livesession.RemoteInterruptRequest{OperationID: "op", FleetID: "fleet", ShipID: "ship", FleetEpoch: 2, ConnectionGeneration: 3, TargetReference: "absent"}
	if got := ep.DeliverInterrupt(context.Background(), fleetinterrupt.DeliveryV1{Operator: steerOnly, Request: interruptRequest}); got.ReasonCode != "unauthorized" {
		t.Fatalf("steer principal was admitted to turn interruption: %+v", got)
	}
}

// TestShipEndpointWithAuthorityAlwaysRevalidates asserts the recheck is
// unconditional when the ship does own an authority. The removed boolean made
// exactly this call site a no-op for both production constructors.
func TestShipEndpointWithAuthorityAlwaysRevalidates(t *testing.T) {
	registry, op, _, in, clock := fixture(t)
	ep := &ShipEndpoint{
		fleetID: in.FleetID, shipID: in.ShipID, fleetEpoch: 2, generation: 3,
		authority: registry, steerClock: clock, interruptClock: clock,
		targets:                 map[string]livesession.RemoteSteerTarget{},
		interruptTargets:        map[string]livesession.RemoteInterruptTarget{},
		interruptPersonaAliases: map[string]string{}, interruptPublicPersonas: map[string]string{},
	}
	operator := fleetidentity.OperatorPrincipal{FleetID: op.Record.FleetID, SubjectID: op.Record.SubjectID, CredentialID: op.Record.CredentialID, Capability: op.Record.Capability, CredentialGeneration: op.Record.CredentialGeneration, ShipIDs: op.Record.ShipIDs}
	request := livesession.RemoteSteerRequest{OperationID: "op", FleetID: in.FleetID, ShipID: in.ShipID, FleetEpoch: 2, ConnectionGeneration: 3, TargetReference: "target"}
	// Revalidation runs before target resolution, so a live credential reaches
	// the (empty) target table and a revoked one never does.
	if got := ep.Deliver(context.Background(), DeliveryV1{operator, request}); got.ReasonCode != "stale_target" {
		t.Fatalf("live credential rejected: %+v", got)
	}
	if err := registry.RevokeOperatorCredential(op.Record.CredentialID, op.Record.CredentialGeneration); err != nil {
		t.Fatal(err)
	}
	if got := ep.Deliver(context.Background(), DeliveryV1{operator, request}); got.ReasonCode != "credential_revoked" {
		t.Fatalf("revoked credential steered the turn: %+v", got)
	}
}

// TestSteerReplayIsIdempotentForTheCallerOwnedOperation is the regression test
// for the retry hazard: Fleet used to mint a fresh operation id and nonce per
// call, so the ship-local at-most-once table could never recognize a retry and
// the same message entered the same live turn twice.
func TestSteerReplayIsIdempotentForTheCallerOwnedOperation(t *testing.T) {
	_, op, service, in, _ := fixture(t)
	tunnel := &fakeTunnel{result: result("", "accepted", "accepted")}
	if _, err := service.Connect(in.ShipID, in.ConnectionGeneration, tunnel); err != nil {
		t.Fatal(err)
	}
	first := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in)
	if first.Outcome != livesession.RemoteSteerAccepted || first.OperationID != in.OperationID {
		t.Fatalf("first submit=%+v", first)
	}
	replay := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in)
	if replay != first {
		t.Fatalf("replay=%+v want %+v", replay, first)
	}
	tunnel.mu.Lock()
	calls, delivered := tunnel.calls, tunnel.last
	tunnel.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the replay was delivered again: calls=%d", calls)
	}
	if delivered.Request.OperationID != in.OperationID {
		t.Fatalf("Fleet substituted its own operation id: %q", delivered.Request.OperationID)
	}
	// The same identifier bound to a different message is a distinct operation.
	edited := in
	edited.Message = "a different message for the same operation id"
	if got := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, edited); got.ReasonCode != "operation_conflict" {
		t.Fatalf("conflicting reuse=%+v", got)
	}
	for name, bad := range map[string]string{"absent": "", "short": "AAAA", "not-base64": strings.Repeat("!", 43)} {
		missing := in
		missing.OperationID = bad
		if got := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, missing); got.ReasonCode != "invalid_request" {
			t.Fatalf("%s operation id accepted: %+v", name, got)
		}
	}
	tunnel.mu.Lock()
	calls = tunnel.calls
	tunnel.mu.Unlock()
	if calls != 1 {
		t.Fatalf("a refused submit still reached the ship: calls=%d", calls)
	}
}

// TestSteerReplayWaitsForTheUndecidedOriginal covers the retry that arrives
// while the first delivery is still outstanding, which is exactly the five
// second delivery_unknown window an operator retries into.
func TestSteerReplayWaitsForTheUndecidedOriginal(t *testing.T) {
	_, op, service, in, _ := fixture(t)
	blocked := make(chan struct{})
	tunnel := &fakeTunnel{result: result("", "accepted", "accepted"), wait: blocked}
	if _, err := service.Connect(in.ShipID, in.ConnectionGeneration, tunnel); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan livesession.RemoteSteerResult, 1)
	go func() {
		firstDone <- service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in)
	}()
	deadline := time.After(5 * time.Second)
	for {
		tunnel.mu.Lock()
		started := tunnel.calls
		tunnel.mu.Unlock()
		if started == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the first delivery never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	replayDone := make(chan livesession.RemoteSteerResult, 1)
	go func() {
		replayDone <- service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in)
	}()
	// Give the replay time to reach the operation table before releasing.
	time.Sleep(20 * time.Millisecond)
	close(blocked)
	first, replay := <-firstDone, <-replayDone
	if first.Outcome != livesession.RemoteSteerAccepted || replay != first {
		t.Fatalf("first=%+v replay=%+v", first, replay)
	}
	tunnel.mu.Lock()
	calls := tunnel.calls
	tunnel.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the concurrent replay was delivered: calls=%d", calls)
	}
}
