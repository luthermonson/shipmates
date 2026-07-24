//go:build unix

package fleetobserver

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/livesession"
)

type blockedProductionInterruptEndpoint struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (e *blockedProductionInterruptEndpoint) DeliverInterrupt(ctx context.Context, d fleetinterrupt.DeliveryV1) livesession.RemoteInterruptResult {
	e.calls.Add(1)
	select {
	case e.entered <- struct{}{}:
	default:
	}
	select {
	case <-e.release:
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: d.Request.OperationID, Outcome: livesession.RemoteInterruptInterrupted, ReasonCode: "interrupted", RetryDisposition: livesession.RemoteInterruptNoRetry}
	case <-ctx.Done():
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: d.Request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "delivery_unknown", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
}

func TestProductionInterruptRestartReplaysUnfinishedAsFleetRestarted(t *testing.T) {
	store := filepath.Join(t.TempDir(), "authority")
	const fleetID = "flt_0123456789abcdef"
	registry, err := fleetidentity.OpenRegistry(store, fleetID, nil, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := registry.Enroll(enrollment.ArtifactID, enrollment.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := registry.IssueOperatorCapability("sub_0123456789abcdef", []string{ship.ShipID}, fleetidentity.InterruptTurnCapability, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	credential := operator.Record.CredentialID + "." + operator.Secret

	config := ProductionConfig{AuthorityStore: store, FleetID: fleetID, FleetEpoch: "epc_0123456789abcdef", SteerEpoch: 9, ServiceIdentity: "fleet-service"}
	beforeRestart, err := OpenProduction(config)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &blockedProductionInterruptEndpoint{entered: make(chan struct{}, 1), release: make(chan struct{})}
	disconnect, err := beforeRestart.Interrupt.Connect(ship.ShipID, 1, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer disconnect()
	firstServer := httptest.NewServer(beforeRestart.Handler())
	defer firstServer.Close()

	operationID, err := fleetinterrupt.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	request := fleetinterrupt.SubmitV1{
		SchemaVersion: 1, FleetID: fleetID, FleetEpoch: 9, ShipID: ship.ShipID, ConnectionGeneration: 1,
		Persona: fleetobserve.OpaquePersonaReference(ship.ShipID, 0), InterruptTargetRef: operationID, OperationID: operationID,
	}
	firstResult := make(chan livesession.RemoteInterruptResult, 1)
	firstError := make(chan error, 1)
	go func() {
		result, submitErr := (fleetinterrupt.Client{BaseURL: firstServer.URL, Credential: credential}).Submit(context.Background(), request)
		firstResult <- result
		firstError <- submitErr
	}()
	select {
	case <-endpoint.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not reach the in-flight ship boundary")
	}

	// Reconstructing the production composition from the same durable store
	// models a Fleet process crash at this point: the accepted record exists,
	// but no ship result crossed Fleet's terminal publication boundary.
	afterRestart, err := OpenProduction(config)
	if err != nil {
		t.Fatal(err)
	}
	restartedServer := httptest.NewServer(afterRestart.Handler())
	defer restartedServer.Close()
	replayed, err := (fleetinterrupt.Client{BaseURL: restartedServer.URL, Credential: credential}).Query(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Outcome != livesession.RemoteInterruptIndeterminate || replayed.ReasonCode != "fleet_restarted" || replayed.RetryDisposition != livesession.RemoteInterruptReplaySameOperation {
		t.Fatalf("restart replay=%+v", replayed)
	}
	if got := endpoint.calls.Load(); got != 1 {
		t.Fatalf("unfinished operation redelivered %d times", got)
	}

	close(endpoint.release)
	select {
	case err := <-firstError:
		if err != nil {
			t.Fatalf("original public request cleanup: %v", err)
		}
		<-firstResult
	case <-time.After(3 * time.Second):
		t.Fatal("original public request cleanup timeout")
	}
}
