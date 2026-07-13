// Package fleettunnel implements the authenticated, outbound-only M7 ship
// observation tunnel. It intentionally has no command, controller, approval,
// filesystem, browser, CLI, or generic RPC surface.
package fleettunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

const (
	ProtocolVersion        = 1
	CapabilityObserverOnly = "fleet.observe.ship.v1"
	maxWireBytes           = 256 << 10
)

type Clock interface{ Now() time.Time }
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Channel is a bounded message transport whose peer identity was authenticated
// by the encrypted transport before the application handshake. Implementations
// must make Send and Receive honor context cancellation.
type Channel interface {
	Send(context.Context, []byte) error
	Receive(context.Context) ([]byte, error)
	PeerServiceIdentity() string
	Close() error
}

type Code string

const (
	InvalidHandshake  Code = "invalid_handshake"
	Unauthorized      Code = "unauthorized"
	ProtocolViolation Code = "protocol_violation"
	Backpressure      Code = "backpressure"
	Revoked           Code = "revoked"
	Superseded        Code = "superseded"
)

type Error struct{ Code Code }

func (e *Error) Error() string      { return string(e.Code) }
func fail(c Code) error             { return &Error{Code: c} }
func IsCode(err error, c Code) bool { var e *Error; return errors.As(err, &e) && e.Code == c }

type Challenge struct {
	Version         int       `json:"version"`
	FleetID         string    `json:"fleet_id"`
	Nonce           string    `json:"nonce"`
	ExpiresAt       time.Time `json:"expires_at"`
	ServiceIdentity string    `json:"service_identity"`
}
type Authenticate struct {
	Version      int    `json:"version"`
	CredentialID string `json:"credential_id"`
	Proof        string `json:"proof"`
}
type Accepted struct {
	Version      int      `json:"version"`
	ShipID       string   `json:"ship_id"`
	Generation   uint64   `json:"generation"`
	LeaseMillis  int64    `json:"lease_millis"`
	Capabilities []string `json:"capabilities"`
}
type Resume struct {
	FleetEpoch       string `json:"fleet_epoch,omitempty"`
	LastAcknowledged uint64 `json:"last_acknowledged,omitempty"`
	LocalGap         bool   `json:"local_gap,omitempty"`
}
type Frame struct {
	Version    int           `json:"version"`
	ShipID     string        `json:"ship_id"`
	Generation uint64        `json:"generation"`
	Number     uint64        `json:"number"`
	Type       string        `json:"type"`
	Snapshot   *WireSnapshot `json:"snapshot,omitempty"`
	Event      *WireEvent    `json:"event,omitempty"`
	Resume     *Resume       `json:"resume,omitempty"`
}

// WireSnapshot and WireEvent are the complete publishable ship-to-Fleet data
// schema. A persona is only a bounded local slot; no string identifier field
// exists on the wire.
type WirePersonaState struct {
	Slot            uint16                    `json:"slot"`
	Session         fleetobserve.SessionState `json:"session"`
	Turn            fleetobserve.TurnState    `json:"turn"`
	Activity        fleetobserve.Activity     `json:"activity"`
	ApprovalWaiting bool                      `json:"approval_waiting"`
}
type WireSnapshot struct {
	Personas []WirePersonaState `json:"personas"`
}
type WireEvent struct {
	Slot uint16                 `json:"slot"`
	Kind fleetobserve.EventKind `json:"kind"`
	Data json.RawMessage        `json:"data"`
}

type wirePresencePayload struct{}
type wireSessionPayload struct {
	Session fleetobserve.SessionState `json:"session"`
}
type wireTurnPayload struct {
	Turn string `json:"turn"`
}
type wireActivityPayload struct {
	Activity fleetobserve.Activity `json:"activity"`
}
type wireApprovalWaitingPayload struct {
	Waiting bool `json:"waiting"`
}
type wireRequestRefusedPayload struct {
	RequestClass string `json:"request_class"`
	ReasonCode   string `json:"reason_code"`
}
type wireProjectionWarningPayload struct {
	WarningCode string `json:"warning_code"`
}

func projectSnapshot(shipID string, in WireSnapshot) (fleetobserve.ShipStatusV1, error) {
	if len(in.Personas) == 0 || len(in.Personas) > 256 {
		return fleetobserve.ShipStatusV1{}, fail(ProtocolViolation)
	}
	out := fleetobserve.ShipStatusV1{ShipID: shipID, Personas: make([]fleetobserve.PersonaStatusV1, len(in.Personas))}
	for i, p := range in.Personas {
		if int(p.Slot) != i {
			return fleetobserve.ShipStatusV1{}, fail(ProtocolViolation)
		}
		out.Personas[i] = fleetobserve.PersonaStatusV1{Persona: fleetobserve.OpaquePersonaReference(shipID, p.Slot), Installed: true, Session: p.Session, Turn: p.Turn, Activity: p.Activity, ApprovalWaiting: p.ApprovalWaiting}
	}
	return fleetobserve.NormalizeShipSnapshot(shipID, out)
}

func projectEvent(shipID string, in WireEvent) (fleetobserve.ObservationEventV1, error) {
	var data fleetobserve.EventDataV1
	var payload any
	switch in.Kind {
	case fleetobserve.PersonaInstalled, fleetobserve.PersonaRemoved:
		payload = &wirePresencePayload{}
	case fleetobserve.SessionStateEvent:
		payload = &wireSessionPayload{}
	case fleetobserve.TurnStateEvent:
		payload = &wireTurnPayload{}
	case fleetobserve.ActivityEvent:
		payload = &wireActivityPayload{}
	case fleetobserve.ApprovalWaiting:
		payload = &wireApprovalWaitingPayload{}
	case fleetobserve.RequestRefused:
		payload = &wireRequestRefusedPayload{}
	case fleetobserve.ProjectionWarning:
		payload = &wireProjectionWarningPayload{}
	default:
		return fleetobserve.ObservationEventV1{}, fail(ProtocolViolation)
	}
	if strict(in.Data, payload) != nil {
		return fleetobserve.ObservationEventV1{}, fail(ProtocolViolation)
	}
	switch p := payload.(type) {
	case *wireSessionPayload:
		data.Session = p.Session
	case *wireTurnPayload:
		data.Turn = p.Turn
	case *wireActivityPayload:
		data.Activity = p.Activity
	case *wireApprovalWaitingPayload:
		data.Waiting = &p.Waiting
	case *wireRequestRefusedPayload:
		data.RequestClass, data.ReasonCode = p.RequestClass, p.ReasonCode
	case *wireProjectionWarningPayload:
		data.WarningCode = p.WarningCode
	}
	return fleetobserve.NormalizeObservationEvent(fleetobserve.ObservationEventV1{Persona: fleetobserve.OpaquePersonaReference(shipID, in.Slot), Kind: in.Kind, Data: data})
}

func wireEvent(slot uint16, in fleetobserve.ObservationEventV1) (WireEvent, error) {
	var payload any
	switch in.Kind {
	case fleetobserve.PersonaInstalled, fleetobserve.PersonaRemoved:
		payload = wirePresencePayload{}
	case fleetobserve.SessionStateEvent:
		payload = wireSessionPayload{in.Data.Session}
	case fleetobserve.TurnStateEvent:
		payload = wireTurnPayload{in.Data.Turn}
	case fleetobserve.ActivityEvent:
		payload = wireActivityPayload{in.Data.Activity}
	case fleetobserve.ApprovalWaiting:
		if in.Data.Waiting == nil {
			return WireEvent{}, fail(ProtocolViolation)
		}
		payload = wireApprovalWaitingPayload{*in.Data.Waiting}
	case fleetobserve.RequestRefused:
		payload = wireRequestRefusedPayload{in.Data.RequestClass, in.Data.ReasonCode}
	case fleetobserve.ProjectionWarning:
		payload = wireProjectionWarningPayload{in.Data.WarningCode}
	default:
		return WireEvent{}, fail(ProtocolViolation)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return WireEvent{}, fail(ProtocolViolation)
	}
	return WireEvent{Slot: slot, Kind: in.Kind, Data: b}, nil
}

type Ack struct {
	Version    int                 `json:"version"`
	Type       string              `json:"type"`
	Number     uint64              `json:"number"`
	FleetEpoch string              `json:"fleet_epoch,omitempty"`
	Cursor     uint64              `json:"cursor,omitempty"`
	Gap        *fleetobserve.GapV1 `json:"gap,omitempty"`
}

type Registry interface {
	AuthenticateShipProof(string, []byte, []byte) (fleetidentity.ShipPrincipal, error)
	AllocateConnectionGeneration(fleetidentity.ShipPrincipal) (uint64, error)
	ShipCredentialActive(fleetidentity.ShipPrincipal) (bool, error)
}
type Projection interface {
	Connect(string, uint64) error
	InstallSnapshot(string, uint64, fleetobserve.ShipStatusV1) error
	InstallResnapshot(string, uint64, fleetobserve.ShipStatusV1) error
	Enqueue(string, uint64, fleetobserve.ObservationEventV1) error
	Drain(string, uint64) error
	Heartbeat(string, uint64) error
	Disconnect(string, uint64, bool) error
	Resync(string, uint64) (fleetobserve.ReadResult, error)
	Snapshot() fleetobserve.FleetSnapshotV1
}
type ServerConfig struct {
	FleetID, ServiceIdentity               string
	HandshakeTTL, LeaseDuration, IOTimeout time.Duration
	Clock                                  Clock
	Random                                 io.Reader
}
type Server struct {
	cfg        ServerConfig
	identities Registry
	projection Projection
	mu         sync.Mutex
	active     map[string]activeConnection
	state      serverState
	closed     chan struct{}
}
type activeConnection struct {
	generation uint64
	principal  fleetidentity.ShipPrincipal
	channel    *ownedChannel
}

type serverState uint8

const (
	serverOpen serverState = iota
	serverClosing
	serverClosed
)

// ownedChannel makes tunnel close ownership independent of the transport's
// idempotency guarantees. Every shutdown and per-tunnel cleanup path shares it.
type ownedChannel struct {
	Channel
	once sync.Once
	err  error
}

func (s *Server) register(shipID string, a activeConnection) (activeConnection, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != serverOpen {
		return activeConnection{}, false, false
	}
	old, hadOld := s.active[shipID]
	s.active[shipID] = a
	return old, hadOld, true
}

func (c *ownedChannel) Close() error {
	c.once.Do(func() { c.err = c.Channel.Close() })
	return c.err
}

func NewServer(c ServerConfig, ids Registry, p Projection) (*Server, error) {
	if c.FleetID == "" || c.ServiceIdentity == "" || c.HandshakeTTL <= 0 || c.HandshakeTTL > time.Minute || c.LeaseDuration <= 0 || c.IOTimeout <= 0 || ids == nil || p == nil {
		return nil, fail(InvalidHandshake)
	}
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	if c.Random == nil {
		c.Random = rand.Reader
	}
	return &Server{cfg: c, identities: ids, projection: p, active: map[string]activeConnection{}, closed: make(chan struct{})}, nil
}

func transcript(c Challenge, version int, credential string) []byte {
	// JSON of a closed struct is a deterministic, length-delimited transcript.
	b, _ := json.Marshal(struct {
		Domain     string    `json:"domain"`
		Challenge  Challenge `json:"challenge"`
		Selected   int       `json:"selected"`
		Credential string    `json:"credential_id"`
	}{"shipmates/fleet-tunnel/v1", c, version, credential})
	return b
}
func strict(data []byte, dst any) error {
	if len(data) == 0 || len(data) > maxWireBytes {
		return fail(ProtocolViolation)
	}
	d := json.NewDecoder(bytesReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fail(ProtocolViolation)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fail(ProtocolViolation)
	}
	return nil
}

type byteReader struct {
	b []byte
	n int
}

func bytesReader(b []byte) *byteReader { return &byteReader{b: b} }
func (r *byteReader) Read(p []byte) (int, error) {
	if r.n == len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.n:])
	r.n += n
	return n, nil
}
func sendJSON(ctx context.Context, ch Channel, v any) error {
	b, e := json.Marshal(v)
	if e != nil || len(b) > maxWireBytes {
		return fail(ProtocolViolation)
	}
	return ch.Send(ctx, b)
}
func (s *Server) ioctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, s.cfg.IOTimeout)
}

// Serve handles exactly one already-encrypted inbound endpoint. The ship is the
// party that must have initiated the underlying connection.
func (s *Server) Serve(ctx context.Context, ch Channel) (retErr error) {
	owned := &ownedChannel{Channel: ch}
	ch = owned
	defer ch.Close()
	if ch.PeerServiceIdentity() == "" {
		return fail(InvalidHandshake)
	}
	n := make([]byte, 32)
	if _, e := io.ReadFull(s.cfg.Random, n); e != nil {
		return fail(InvalidHandshake)
	}
	c := Challenge{ProtocolVersion, s.cfg.FleetID, base64.RawURLEncoding.EncodeToString(n), s.cfg.Clock.Now().Add(s.cfg.HandshakeTTL).UTC(), s.cfg.ServiceIdentity}
	cx, cancel := s.ioctx(ctx)
	e := sendJSON(cx, ch, c)
	cancel()
	if e != nil {
		return fail(Backpressure)
	}
	cx, cancel = s.ioctx(ctx)
	raw, e := ch.Receive(cx)
	cancel()
	if e != nil {
		return fail(Backpressure)
	}
	var a Authenticate
	if strict(raw, &a) != nil || a.Version != ProtocolVersion || !s.cfg.Clock.Now().Before(c.ExpiresAt) {
		return fail(InvalidHandshake)
	}
	proof, e := base64.RawURLEncoding.DecodeString(a.Proof)
	if e != nil {
		return fail(Unauthorized)
	}
	principal, e := s.identities.AuthenticateShipProof(a.CredentialID, transcript(c, a.Version, a.CredentialID), proof)
	if e != nil {
		return fail(Unauthorized)
	}
	g, e := s.identities.AllocateConnectionGeneration(principal)
	if e != nil {
		return fail(Unauthorized)
	}
	if e = s.projection.Connect(principal.ShipID, g); e != nil {
		return fail(Superseded)
	}
	old, hadOld, registered := s.register(principal.ShipID, activeConnection{g, principal, owned})
	if !registered {
		return fail(Superseded)
	}
	if hadOld && old.generation < g {
		_ = old.channel.Close()
	}
	defer func() {
		s.mu.Lock()
		if cur, ok := s.active[principal.ShipID]; ok && cur.generation == g {
			delete(s.active, principal.ShipID)
		}
		s.mu.Unlock()
	}()
	accepted := Accepted{ProtocolVersion, principal.ShipID, g, s.cfg.LeaseDuration.Milliseconds(), []string{CapabilityObserverOnly, string(fleetobserve.CapabilitySnapshotV1), string(fleetobserve.CapabilityEventsV1), string(fleetobserve.CapabilityGapsV1)}}
	cx, cancel = s.ioctx(ctx)
	e = sendJSON(cx, ch, accepted)
	cancel()
	if e != nil {
		return fail(Backpressure)
	}
	ready := false
	var last uint64
	defer func() {
		if ready {
			_ = s.projection.Disconnect(principal.ShipID, g, retErr == nil)
		}
	}()
	for {
		cx, cancel = s.ioctx(ctx)
		raw, e = ch.Receive(cx)
		cancel()
		if e != nil {
			return fail(Backpressure)
		}
		active, activeErr := s.identities.ShipCredentialActive(principal)
		if activeErr != nil || !active {
			return fail(Revoked)
		}
		var f Frame
		if strict(raw, &f) != nil || f.Version != ProtocolVersion || f.ShipID != principal.ShipID || f.Generation != g || f.Number == 0 || f.Number != last+1 {
			return fail(ProtocolViolation)
		}
		last = f.Number
		ack := Ack{Version: ProtocolVersion, Type: "ack", Number: last}
		switch f.Type {
		case "snapshot", "resnapshot":
			if f.Snapshot == nil || f.Event != nil || (f.Type == "snapshot") != !ready {
				return fail(ProtocolViolation)
			}
			normalized, ne := projectSnapshot(principal.ShipID, *f.Snapshot)
			if ne != nil {
				return fail(ProtocolViolation)
			}
			if f.Type == "snapshot" {
				e = s.projection.InstallSnapshot(principal.ShipID, g, normalized)
			} else {
				e = s.projection.InstallResnapshot(principal.ShipID, g, normalized)
			}
			if e != nil {
				return fail(ProtocolViolation)
			}
			ready = true
			fs := s.projection.Snapshot()
			ack.FleetEpoch, ack.Cursor = fs.FleetEpoch, fs.SnapshotCursor
			if f.Resume != nil && (f.Resume.LocalGap || f.Resume.FleetEpoch != "" || f.Resume.LastAcknowledged != 0) {
				r, _ := s.projection.Resync(principal.ShipID, g)
				ack.Type = "resnapshot"
				ack.Gap = r.Gap
			}
		case "event":
			if !ready || f.Event == nil || f.Snapshot != nil || f.Resume != nil {
				return fail(ProtocolViolation)
			}
			normalized, ne := projectEvent(principal.ShipID, *f.Event)
			if ne != nil {
				return fail(ProtocolViolation)
			}
			if e = s.projection.Enqueue(principal.ShipID, g, normalized); e != nil {
				r, _ := s.projection.Resync(principal.ShipID, g)
				ack.Type = "resnapshot"
				ack.Gap = r.Gap
			} else if e = s.projection.Drain(principal.ShipID, g); e != nil {
				return fail(ProtocolViolation)
			}
			fs := s.projection.Snapshot()
			ack.FleetEpoch, ack.Cursor = fs.FleetEpoch, fs.SnapshotCursor
		case "heartbeat":
			if !ready || f.Event != nil || f.Snapshot != nil || f.Resume != nil {
				return fail(ProtocolViolation)
			}
			if e = s.projection.Heartbeat(principal.ShipID, g); e != nil {
				return fail(Superseded)
			}
			fs := s.projection.Snapshot()
			ack.FleetEpoch, ack.Cursor = fs.FleetEpoch, fs.SnapshotCursor
		case "close":
			if !ready || f.Event != nil || f.Snapshot != nil || f.Resume != nil {
				return fail(ProtocolViolation)
			}
			cx, cancel = s.ioctx(ctx)
			_ = sendJSON(cx, ch, Ack{Version: 1, Type: "close", Number: last})
			cancel()
			return nil
		default:
			return fail(ProtocolViolation)
		}
		cx, cancel = s.ioctx(ctx)
		e = sendJSON(cx, ch, ack)
		cancel()
		if e != nil {
			return fail(Backpressure)
		}
	}
}

// CloseRevokedConnections is called after a credential/ship revocation commit.
// It closes matching established connections without waiting for another ship
// frame; the projection observes transport loss through the serving goroutine.
func (s *Server) CloseRevokedConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.active {
		active, err := s.identities.ShipCredentialActive(a.principal)
		if err != nil || !active {
			_ = a.channel.Close()
		}
	}
}

// CloseAll terminates every hijacked tunnel during production shutdown.
func (s *Server) CloseAll() {
	s.mu.Lock()
	if s.state != serverOpen {
		closed := s.closed
		s.mu.Unlock()
		<-closed
		return
	}
	s.state = serverClosing
	connections := make([]*ownedChannel, 0, len(s.active))
	for _, a := range s.active {
		connections = append(connections, a.channel)
	}
	s.mu.Unlock()
	for _, ch := range connections {
		_ = ch.Close()
	}
	s.mu.Lock()
	s.state = serverClosed
	close(s.closed)
	s.mu.Unlock()
}

type ClientConfig struct {
	FleetID, ServiceIdentity, CredentialID, Secret, ShipID string
	IOTimeout                                              time.Duration
	Clock                                                  Clock
	Connected                                              func(context.Context, uint64) (func(), error)
}
type Client struct{ cfg ClientConfig }

func NewClient(c ClientConfig) (*Client, error) {
	if c.FleetID == "" || c.ServiceIdentity == "" || c.CredentialID == "" || c.Secret == "" || c.ShipID == "" || c.IOTimeout <= 0 {
		return nil, fail(InvalidHandshake)
	}
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	return &Client{c}, nil
}

// RunLocal performs one connection from the production typed local-state
// boundary. Persona identity is assigned by the trusted installed-project
// adapter and is never accepted from a caller-constructed tunnel frame.
func (c *Client) RunLocal(ctx context.Context, ch Channel, adapter *fleetobserve.LocalStateAdapter, states []fleetobserve.LocalPersonaState, resume Resume, localEvents []fleetobserve.LocalEvent) (lastEpoch string, lastCursor uint64, err error) {
	return c.RunLocalUpdates(ctx, ch, adapter, states, resume, localEvents, nil)
}

// RunLocalUpdates publishes full trusted-slot resnapshots whenever local live
// ownership changes. Updates are serialized with heartbeats on this goroutine,
// so frame numbering and Fleet cursor ordering remain exact.
func (c *Client) RunLocalUpdates(ctx context.Context, ch Channel, adapter *fleetobserve.LocalStateAdapter, states []fleetobserve.LocalPersonaState, resume Resume, localEvents []fleetobserve.LocalEvent, updates <-chan []fleetobserve.LocalPersonaState) (lastEpoch string, lastCursor uint64, err error) {
	if adapter == nil || adapter.ValidateStates(states) != nil {
		return "", 0, fail(ProtocolViolation)
	}
	snapshot := WireSnapshot{Personas: make([]WirePersonaState, len(states))}
	for i, state := range states {
		snapshot.Personas[i] = WirePersonaState{Slot: uint16(i), Session: state.Session, Turn: state.Turn, Activity: state.Activity, ApprovalWaiting: state.ApprovalWaiting}
	}
	events := make([]WireEvent, len(localEvents))
	for i, event := range localEvents {
		if event.PersonaIndex > int(^uint16(0)) {
			return "", 0, fail(ProtocolViolation)
		}
		normalized, normalizeErr := adapter.Event(event)
		if normalizeErr != nil {
			return "", 0, fail(ProtocolViolation)
		}
		events[i], normalizeErr = wireEvent(uint16(event.PersonaIndex), normalized)
		if normalizeErr != nil {
			return "", 0, normalizeErr
		}
	}
	projectUpdate := func(next []fleetobserve.LocalPersonaState) (WireSnapshot, error) {
		if adapter.ValidateStates(next) != nil {
			return WireSnapshot{}, fail(ProtocolViolation)
		}
		snap := WireSnapshot{Personas: make([]WirePersonaState, len(next))}
		for i, state := range next {
			snap.Personas[i] = WirePersonaState{Slot: uint16(i), Session: state.Session, Turn: state.Turn, Activity: state.Activity, ApprovalWaiting: state.ApprovalWaiting}
		}
		return snap, nil
	}
	return c.runProjected(ctx, ch, snapshot, resume, events, updates, projectUpdate)
}

func (c *Client) runProjected(ctx context.Context, ch Channel, snapshot WireSnapshot, resume Resume, events []WireEvent, updates <-chan []fleetobserve.LocalPersonaState, projectUpdate func([]fleetobserve.LocalPersonaState) (WireSnapshot, error)) (lastEpoch string, lastCursor uint64, err error) {
	defer ch.Close()
	if ch.PeerServiceIdentity() != c.cfg.ServiceIdentity {
		return "", 0, fail(InvalidHandshake)
	}
	ioctx := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, c.cfg.IOTimeout) }
	cx, x := ioctx()
	raw, e := ch.Receive(cx)
	x()
	if e != nil {
		return "", 0, fail(Backpressure)
	}
	var q Challenge
	if strict(raw, &q) != nil || q.Version != 1 || q.FleetID != c.cfg.FleetID || q.ServiceIdentity != c.cfg.ServiceIdentity || !c.cfg.Clock.Now().Before(q.ExpiresAt) {
		return "", 0, fail(InvalidHandshake)
	}
	p := fleetidentity.ShipProof(c.cfg.Secret, transcript(q, 1, c.cfg.CredentialID))
	a := Authenticate{1, c.cfg.CredentialID, base64.RawURLEncoding.EncodeToString(p)}
	cx, x = ioctx()
	e = sendJSON(cx, ch, a)
	x()
	if e != nil {
		return "", 0, fail(Backpressure)
	}
	cx, x = ioctx()
	raw, e = ch.Receive(cx)
	x()
	if e != nil {
		return "", 0, fail(Backpressure)
	}
	var ok Accepted
	if strict(raw, &ok) != nil || ok.Version != 1 || ok.ShipID != c.cfg.ShipID || ok.Generation == 0 || !hasOnlyObserve(ok.Capabilities) {
		return "", 0, fail(InvalidHandshake)
	}
	var disconnect func()
	if c.cfg.Connected != nil {
		disconnect, e = c.cfg.Connected(ctx, ok.Generation)
		if e != nil {
			return "", 0, e
		}
		defer disconnect()
	}
	n := uint64(1)
	send := func(f Frame) (Ack, error) {
		cx, x := ioctx()
		defer x()
		if e := sendJSON(cx, ch, f); e != nil {
			return Ack{}, fail(Backpressure)
		}
		raw, e := ch.Receive(cx)
		if e != nil {
			return Ack{}, fail(Backpressure)
		}
		var a Ack
		if strict(raw, &a) != nil || a.Version != 1 || a.Number != f.Number || (a.Type != "ack" && a.Type != "resnapshot" && a.Type != "close") {
			return Ack{}, fail(ProtocolViolation)
		}
		return a, nil
	}
	ack, e := send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "snapshot", Snapshot: &snapshot, Resume: &resume})
	if e != nil {
		return "", 0, e
	}
	lastEpoch, lastCursor = ack.FleetEpoch, ack.Cursor
	if ack.Type == "resnapshot" { /* the just-committed snapshot is the boundary */
	}
	for i := range events {
		n++
		ack, e = send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "event", Event: &events[i]})
		if e != nil {
			return lastEpoch, lastCursor, e
		}
		if ack.Type == "resnapshot" {
			return ack.FleetEpoch, ack.Cursor, fail(Backpressure)
		}
		lastEpoch, lastCursor = ack.FleetEpoch, ack.Cursor
	}
	if c.cfg.Connected != nil {
		ticker := time.NewTicker(time.Duration(ok.LeaseMillis/2) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				n++
				_, _ = send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "close"})
				return lastEpoch, lastCursor, ctx.Err()
			case <-ticker.C:
				n++
				ack, e = send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "heartbeat"})
				if e != nil {
					return lastEpoch, lastCursor, e
				}
				lastEpoch, lastCursor = ack.FleetEpoch, ack.Cursor
			case next, open := <-updates:
				if !open {
					updates = nil
					continue
				}
				snap, projectionErr := projectUpdate(next)
				if projectionErr != nil {
					return lastEpoch, lastCursor, projectionErr
				}
				n++
				ack, e = send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "resnapshot", Snapshot: &snap})
				if e != nil {
					return lastEpoch, lastCursor, e
				}
				lastEpoch, lastCursor = ack.FleetEpoch, ack.Cursor
			}
		}
	}
	n++
	_, e = send(Frame{Version: 1, ShipID: c.cfg.ShipID, Generation: ok.Generation, Number: n, Type: "close"})
	return lastEpoch, lastCursor, e
}
func hasOnlyObserve(c []string) bool {
	required := map[string]bool{CapabilityObserverOnly: false, string(fleetobserve.CapabilitySnapshotV1): false, string(fleetobserve.CapabilityEventsV1): false, string(fleetobserve.CapabilityGapsV1): false}
	for _, v := range c {
		if _, ok := required[v]; !ok {
			return false
		}
		required[v] = true
	}
	for _, v := range required {
		if !v {
			return false
		}
	}
	return true
}

// RetryDelay implements bounded exponential reconnect without jitter; callers
// can use an injected clock/transport in deterministic tests.
func RetryDelay(attempt int, base, max time.Duration) (time.Duration, error) {
	if attempt < 0 || base <= 0 || max < base {
		return 0, fmt.Errorf("invalid retry policy")
	}
	d := base
	for i := 0; i < attempt && d < max; i++ {
		if d > max/2 {
			return max, nil
		}
		d *= 2
	}
	if d > max {
		d = max
	}
	return d, nil
}
