package fleetsteer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/livesession"
)

type deadlineRecordingWriter struct {
	deadlines []time.Time
	written   []any
}

func (w *deadlineRecordingWriter) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	return nil
}
func (w *deadlineRecordingWriter) WriteJSON(v any) error {
	w.written = append(w.written, v)
	return nil
}

// TestReverseWriterNeverClearsTheWriteDeadline is the regression test for the
// readiness frame that was written with the zero time. gorilla treats the zero
// time as "no deadline", so a peer with a full receive window blocked the shared
// writer forever while Submit had already abandoned the delivery goroutine.
func TestReverseWriterNeverClearsTheWriteDeadline(t *testing.T) {
	socket := &deadlineRecordingWriter{}
	w := &reverseWriter{conn: socket}
	before := time.Now()
	if err := w.write(time.Time{}, reverseReady{Type: "reverse_ready"}); err != nil {
		t.Fatal(err)
	}
	if len(socket.deadlines) != 1 || socket.deadlines[0].IsZero() {
		t.Fatalf("deadlines=%v", socket.deadlines)
	}
	if socket.deadlines[0].Before(before) || socket.deadlines[0].After(before.Add(2*DeliveryDeadline)) {
		t.Fatalf("substituted deadline %v is not bounded by the delivery deadline", socket.deadlines[0])
	}
	explicit := before.Add(time.Minute)
	if err := w.write(explicit, reverseReady{}); err != nil {
		t.Fatal(err)
	}
	if !socket.deadlines[1].Equal(explicit) {
		t.Fatalf("caller deadline was overwritten: %v", socket.deadlines[1])
	}
}

type stubInterruptEndpoint struct{}

func (stubInterruptEndpoint) DeliverInterrupt(context.Context, fleetinterrupt.DeliveryV1) livesession.RemoteInterruptResult {
	return livesession.RemoteInterruptResult{}
}

// reverseFixture enrolls one ship and returns the Fleet service, registry, the
// ship credential, and its current authenticated connection generation.
func reverseFixture(t *testing.T) (*Service, *fleetidentity.Registry, fleetidentity.EnrollmentResult, uint64) {
	t.Helper()
	clock := &fakeClock{time.Unix(1000, 0)}
	registry, err := fleetidentity.NewRegistry("fleet-000000000001", clock, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := registry.Enroll(artifact.ArtifactID, artifact.Secret, "txn-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := registry.AuthenticateShip(ship.Credential.CredentialID, ship.Credential.Secret)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := registry.AllocateConnectionGeneration(principal)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ship.FleetID, registry, clock, &counterReader{})
	if err != nil {
		t.Fatal(err)
	}
	return service, registry, ship, generation
}

func dialReverse(t *testing.T, server *httptest.Server, ship fleetidentity.EnrollmentResult, generation uint64) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Basic "+basicAuth(ship.Credential.CredentialID, ship.Credential.Secret))
	header.Set("X-Shipmates-Connection-Generation", strconv.FormatUint(generation, 10))
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/fleet/v1/steer-tunnel", header)
}

// TestReverseHandlerBoundsFrameSizeAndRejectsOpenSchema covers the read limit
// and strict decoding the reverse tunnel never had. gorilla's default read limit
// is unlimited and its ReadJSON accepts unknown fields and trailing data, on the
// tunnel that carries prompt injection and turn kill.
func TestReverseHandlerBoundsFrameSizeAndRejectsOpenSchema(t *testing.T) {
	for name, frame := range map[string]string{
		"oversized":     `{"type":"turn_steer_result","operation_id":"` + strings.Repeat("a", maxReverseWireBytes) + `"}`,
		"unknown-field": `{"type":"turn_steer_result","operation_id":"x","result":{"schema_version":1,"operation_id":"x","outcome":"accepted","reason_code":"accepted","retry_disposition":"none"},"command":"shutdown"}`,
		"trailing-data": `{"type":"turn_steer_result","operation_id":"x","result":{"schema_version":1,"operation_id":"x","outcome":"accepted","reason_code":"accepted","retry_disposition":"none"}}{"type":"turn_steer_result","operation_id":"y"}`,
		"binary":        "",
	} {
		t.Run(name, func(t *testing.T) {
			service, registry, ship, generation := reverseFixture(t)
			server := httptest.NewServer(ReverseHandler(service, registry))
			defer server.Close()
			conn, _, err := dialReverse(t, server, ship, generation)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadLimit(maxReverseWireBytes * 4)
			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var ready reverseReady
			if err := conn.ReadJSON(&ready); err != nil || ready.Type != "reverse_ready" {
				t.Fatalf("ready=%+v err=%v", ready, err)
			}
			kind := websocket.TextMessage
			payload := []byte(frame)
			if name == "binary" {
				kind, payload = websocket.BinaryMessage, []byte(`{"type":"turn_steer_result","operation_id":"x"}`)
			}
			if err := conn.WriteMessage(kind, payload); err != nil {
				t.Fatal(err)
			}
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatalf("%s frame was accepted", name)
			}
			// The Fleet-side registration must be gone once the tunnel closes.
			deadline := time.After(5 * time.Second)
			for {
				service.mu.Lock()
				_, registered := service.connections[ship.ShipID]
				service.mu.Unlock()
				if !registered {
					return
				}
				select {
				case <-deadline:
					t.Fatal("the reverse connection stayed registered after the protocol failure")
				default:
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

// TestReverseHandlerClosesTheHijackedSocketWhenInterruptRegistrationFails is the
// regression test for the return that preceded the deferred Close: the upgraded
// connection leaked and the ship blocked on a handshake that would never come.
func TestReverseHandlerClosesTheHijackedSocketWhenInterruptRegistrationFails(t *testing.T) {
	service, registry, ship, generation := reverseFixture(t)
	interrupts, err := fleetinterrupt.NewService(ship.FleetID, registry, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	// A newer generation already owns interrupt delivery for this ship, so the
	// handler's interrupt registration for the dialed generation is refused.
	if _, err := interrupts.Connect(ship.ShipID, generation+1, stubInterruptEndpoint{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(ReverseHandler(service, registry, interrupts))
	defer server.Close()
	conn, _, err := dialReverse(t, server, ship, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("a tunnel that failed interrupt registration served a frame")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the hijacked socket was leaked; the ship waited %v for a close", elapsed)
	}
}

// TestReverseHandlerHardensTheServerReadPath asserts the read limit is installed
// on the Fleet side by observing gorilla's message-too-big close code.
func TestReverseHandlerHardensTheServerReadPath(t *testing.T) {
	service, registry, ship, generation := reverseFixture(t)
	server := httptest.NewServer(ReverseHandler(service, registry))
	defer server.Close()
	conn, _, err := dialReverse(t, server, ship, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var ready reverseReady
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", maxReverseWireBytes+1))); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Fatalf("read limit is not installed on the Fleet side: %v", err)
	}
}
