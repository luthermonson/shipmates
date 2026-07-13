package fleetsteer

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/livesession"
)

type concurrencyCheckingWriter struct {
	mu         sync.Mutex
	active     int
	concurrent bool
	entered    chan any
	release    chan struct{}
}

func (*concurrencyCheckingWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *concurrencyCheckingWriter) WriteJSON(value any) error {
	w.mu.Lock()
	w.active++
	if w.active > 1 {
		w.concurrent = true
	}
	w.mu.Unlock()
	w.entered <- value
	<-w.release
	w.mu.Lock()
	w.active--
	w.mu.Unlock()
	return nil
}

func TestReverseWriterSerializesReadyAgainstDelivery(t *testing.T) {
	registry, operator, _, in, clock := fixture(t)
	socket := &concurrencyCheckingWriter{entered: make(chan any, 2), release: make(chan struct{})}
	ep := &reverseEndpoint{
		writer:        &reverseWriter{conn: socket},
		wait:          map[string]chan livesession.RemoteSteerResult{},
		waitInterrupt: map[string]chan livesession.RemoteInterruptResult{},
		registry:      registry,
		shipID:        in.ShipID,
		clock:         clock,
	}

	readyDone := make(chan error, 1)
	wantReady := reverseReady{Type: "reverse_ready", SteerRegistered: true, InterruptRegistered: true}
	go func() { readyDone <- ep.writer.write(time.Time{}, wantReady) }()
	if got := <-socket.entered; got != wantReady {
		t.Fatalf("first write = %#v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	delivery := DeliveryV1{Operator: fleetidentity.OperatorPrincipal{
		FleetID: operator.Record.FleetID, SubjectID: operator.Record.SubjectID,
		CredentialID: operator.Record.CredentialID, Capability: operator.Record.Capability,
		CredentialGeneration: operator.Record.CredentialGeneration, ShipIDs: operator.Record.ShipIDs,
	}, Request: livesession.RemoteSteerRequest{OperationID: "operation-race"}}
	deliverDone := make(chan livesession.RemoteSteerResult, 1)
	go func() { deliverDone <- ep.Deliver(ctx, delivery) }()

	// Delivery has registered its waiter before blocking on the shared writer.
	deadline := time.After(time.Second)
	for {
		ep.stateMu.Lock()
		_, registered := ep.wait[delivery.Request.OperationID]
		ep.stateMu.Unlock()
		if registered {
			break
		}
		select {
		case <-deadline:
			t.Fatal("delivery waiter was not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(socket.release)
	if err := <-readyDone; err != nil {
		t.Fatal(err)
	}
	if got := <-socket.entered; got.(reverseRequest).Type != "turn_steer" {
		t.Fatalf("second write = %#v", got)
	}

	ep.stateMu.Lock()
	response := ep.wait[delivery.Request.OperationID]
	delete(ep.wait, delivery.Request.OperationID)
	ep.stateMu.Unlock()
	response <- result(delivery.Request.OperationID, "accepted", "accepted")
	if got := <-deliverDone; got.Outcome != livesession.RemoteSteerAccepted {
		t.Fatalf("delivery result = %+v", got)
	}
	socket.mu.Lock()
	concurrent := socket.concurrent
	socket.mu.Unlock()
	if concurrent {
		t.Fatal("readiness and delivery entered the websocket writer concurrently")
	}
}

func TestRunReverseClientSendsBasicAuthorizationWithoutPanic(t *testing.T) {
	wantErr := errors.New("dial stopped")
	var gotHeader http.Header
	originalDial := reverseDialContext
	reverseDialContext = func(_ context.Context, _ string, header http.Header) (*websocket.Conn, *http.Response, error) {
		gotHeader = header.Clone()
		return nil, nil, wantErr
	}
	t.Cleanup(func() { reverseDialContext = originalDial })

	identity := fleetidentity.ShipState{
		CredentialID:     "ship-credential",
		CredentialSecret: "ship-secret",
	}
	err := RunReverseClient(context.Background(), "http://127.0.0.1:1", identity, 1, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunReverseClient error = %v", err)
	}
	user, pass, ok := (&http.Request{Header: gotHeader}).BasicAuth()
	if got := [2]string{user, pass}; !ok || got != [2]string{identity.CredentialID, identity.CredentialSecret} {
		t.Fatalf("Basic authorization = %#v", got)
	}
}

func TestReverseLoopbackHostAcceptsIPv4AndIPv6(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !reverseLoopbackHost(host) {
			t.Fatalf("loopback host %q was rejected", host)
		}
	}
	for _, host := range []string{"example.com", "192.0.2.1", ""} {
		if reverseLoopbackHost(host) {
			t.Fatalf("non-loopback host %q was accepted", host)
		}
	}
}
