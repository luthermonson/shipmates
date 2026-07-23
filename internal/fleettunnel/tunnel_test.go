package fleettunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetcommander"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

type fakeChannel struct {
	in   <-chan []byte
	out  chan<- []byte
	peer string
	once *sync.Once
}

func (f *fakeChannel) Send(ctx context.Context, b []byte) error {
	b = append([]byte(nil), b...)
	select {
	case f.out <- b:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeChannel) Receive(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return nil, context.Canceled
		}
		return append([]byte(nil), b...), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeChannel) PeerServiceIdentity() string { return f.peer }
func (f *fakeChannel) Close() error                { f.once.Do(func() { close(f.out) }); return nil }
func pair() (*fakeChannel, *fakeChannel) {
	a := make(chan []byte, 8)
	b := make(chan []byte, 8)
	o := new(sync.Once)
	return &fakeChannel{b, a, "fleet-service", o}, &fakeChannel{a, b, "authenticated-ship", o}
}

func fixture(t *testing.T) (*fleetidentity.Registry, *fleetobserve.Projection, fleetidentity.EnrollmentResult, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	random := bytes.NewReader(bytes.Repeat([]byte("0123456789abcdef"), 512))
	r, e := fleetidentity.NewRegistry("flt_0123456789abcdef", clock, random)
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.CreateEnrollment(time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	en, e := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if e != nil {
		t.Fatal(e)
	}
	p, e := fleetobserve.New(fleetobserve.Config{FleetID: en.FleetID, FleetEpoch: "epc_0123456789abcdef", MaxShips: 8, MaxPersonas: 8, MaxSnapshotBytes: 65536, MaxEventBytes: 8192, PerShipIngress: 2, ReplayCapacity: 16, MaxSubscribers: 2, MaxPageSize: 16, MaxTerminalMetadata: 64, LeaseDuration: time.Minute, StaleRetention: time.Minute, Clock: clock})
	if e != nil {
		t.Fatal(e)
	}
	return r, p, en, clock
}
func serverFor(t *testing.T, r *fleetidentity.Registry, p *fleetobserve.Projection, c *fakeClock, random byte) *Server {
	t.Helper()
	s, e := NewServer(ServerConfig{FleetID: "flt_0123456789abcdef", ServiceIdentity: "fleet-service", HandshakeTTL: 30 * time.Second, LeaseDuration: time.Minute, IOTimeout: time.Second, Clock: c, Random: bytes.NewReader(bytes.Repeat([]byte{random}, 64))}, r, p)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func status(string) WireSnapshot {
	return WireSnapshot{Personas: []WirePersonaState{{Slot: 0, Session: fleetobserve.SessionIdle, Turn: fleetobserve.TurnNone, Activity: fleetobserve.ActivityIdle}}}
}

type schedulerMailbox struct{}

func (schedulerMailbox) PullCommander(string, uint64) (*fleetcommander.Message, error) {
	return nil, nil
}
func (schedulerMailbox) AckCommander(string, uint64) error                         { return nil }
func (schedulerMailbox) IngestCommanderEvent(string, fleetcommander.Message) error { return nil }

type countingCommanderStep struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelFunc
}

func (s *countingCommanderStep) Step(context.Context) error {
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if calls >= 2 && s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *countingCommanderStep) Calls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func TestRunProjectedFairlySchedulesCommanderAfterInitialSnapshot(t *testing.T) {
	r, p, en, c := fixture(t)
	s, err := NewServer(ServerConfig{FleetID: en.FleetID, ServiceIdentity: "fleet-service", HandshakeTTL: 30 * time.Second, LeaseDuration: time.Minute, IOTimeout: time.Second, Clock: c, Random: bytes.NewReader(bytes.Repeat([]byte{21}, 64)), CommanderMailbox: schedulerMailbox{}}, r, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := &countingCommanderStep{cancel: cancel}
	client, err := NewClient(ClientConfig{FleetID: en.FleetID, ServiceIdentity: "fleet-service", CredentialID: en.Credential.CredentialID, Secret: en.Credential.Secret, ShipID: en.ShipID, IOTimeout: time.Second, Clock: c, Connected: func(context.Context, uint64) (func(), error) { return func() {}, nil }, CommanderStep: func(context.Context, Channel, uint64) (CommanderStep, error) { return step, nil }})
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan []fleetobserve.LocalPersonaState, 1)
	updates <- []fleetobserve.LocalPersonaState{{Session: fleetobserve.SessionWorking, Turn: fleetobserve.TurnActive, Activity: fleetobserve.ActivityOther}}
	close(updates)
	cl, sv := pair()
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Serve(ctx, sv) }()
	_, _, runErr := client.runProjected(ctx, cl, status(en.ShipID), Resume{}, nil, updates, func(next []fleetobserve.LocalPersonaState) (WireSnapshot, error) {
		return WireSnapshot{Personas: []WirePersonaState{{Slot: 0, Session: next[0].Session, Turn: next[0].Turn, Activity: next[0].Activity}}}, nil
	})
	if runErr == nil || step.Calls() < 2 {
		t.Fatalf("run=%v commander_calls=%d", runErr, step.Calls())
	}
	if got := p.Snapshot().Ships[0].Personas[0].Turn; got != fleetobserve.TurnActive {
		t.Fatalf("M7 update starved; turn=%q", got)
	}
	<-serverDone
}

func TestDeterministicOutboundHandshakeSnapshotEventAndClose(t *testing.T) {
	r, p, en, c := fixture(t)
	s := serverFor(t, r, p, c, 1)
	client, e := NewClient(ClientConfig{FleetID: en.FleetID, ServiceIdentity: "fleet-service", CredentialID: en.Credential.CredentialID, Secret: en.Credential.Secret, ShipID: en.ShipID, IOTimeout: time.Second, Clock: c})
	if e != nil {
		t.Fatal(e)
	}
	cl, sv := pair()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), sv) }()
	event, e := wireEvent(0, fleetobserve.ObservationEventV1{Kind: fleetobserve.SessionStateEvent, Data: fleetobserve.EventDataV1{Session: fleetobserve.SessionWorking}})
	if e != nil {
		t.Fatal(e)
	}
	epoch, cursor, e := client.runProjected(context.Background(), cl, status(en.ShipID), Resume{}, []WireEvent{event}, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if epoch != "epc_0123456789abcdef" || cursor != 2 {
		t.Fatalf("ack = %q:%d", epoch, cursor)
	}
	snap := p.Snapshot()
	if len(snap.Ships) != 1 || snap.Ships[0].ShipID != en.ShipID || snap.Ships[0].Connectivity != fleetobserve.Offline {
		t.Fatalf("snapshot = %#v", snap)
	}
	if got := snap.Ships[0].Personas[0].Persona; got == "backend" || got != fleetobserve.OpaquePersonaReference(en.ShipID, 0) {
		t.Fatalf("server-derived persona ref = %q", got)
	}
}

func TestRunLocalUpdatesPropagatesActiveTurnResnapshot(t *testing.T) {
	r, p, en, c := fixture(t)
	s := serverFor(t, r, p, c, 12)
	client, err := NewClient(ClientConfig{
		FleetID: en.FleetID, ServiceIdentity: "fleet-service",
		CredentialID: en.Credential.CredentialID, Secret: en.Credential.Secret,
		ShipID: en.ShipID, IOTimeout: time.Second, Clock: c,
		Connected: func(context.Context, uint64) (func(), error) { return func() {}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "agents", "backend.toml"), []byte("name='backend'\ndeveloper_instructions='Test backend.'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := fleetobserve.OpenLocalStateAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan []fleetobserve.LocalPersonaState, 1)
	updates <- []fleetobserve.LocalPersonaState{{Session: fleetobserve.SessionWorking, Turn: fleetobserve.TurnActive, Activity: fleetobserve.ActivityOther}}
	close(updates)
	ctx, cancel := context.WithCancel(context.Background())
	cl, sv := pair()
	serverDone := make(chan error, 1)
	clientDone := make(chan error, 1)
	go func() { serverDone <- s.Serve(ctx, sv) }()
	go func() {
		_, _, runErr := client.RunLocalUpdates(ctx, cl, adapter, []fleetobserve.LocalPersonaState{{Session: fleetobserve.SessionIdle, Turn: fleetobserve.TurnNone, Activity: fleetobserve.ActivityIdle}}, Resume{}, nil, updates)
		clientDone <- runErr
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := p.Snapshot()
		if len(snap.Ships) == 1 && len(snap.Ships[0].Personas) == 1 && snap.Ships[0].Personas[0].Turn == fleetobserve.TurnActive {
			if snap.Ships[0].Personas[0].Session != fleetobserve.SessionWorking {
				t.Fatalf("active turn session = %q", snap.Ships[0].Personas[0].Session)
			}
			cancel()
			<-clientDone
			<-serverDone
			return
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	t.Fatalf("active-turn update was not projected: %#v", p.Snapshot())
}

func TestCapturedProofCannotAuthenticateDifferentNonce(t *testing.T) {
	r, p, en, c := fixture(t)
	makeProof := func(nonceByte byte) (Challenge, Authenticate) {
		q := Challenge{1, en.FleetID, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{nonceByte}, 32)), c.Now().Add(30 * time.Second), "fleet-service"}
		proof := fleetidentity.ShipProof(en.Credential.Secret, transcript(q, 1, en.Credential.CredentialID))
		return q, Authenticate{1, en.Credential.CredentialID, base64.RawURLEncoding.EncodeToString(proof)}
	}
	_, captured := makeProof(1)
	s := serverFor(t, r, p, c, 2)
	cl, sv := pair()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), sv) }()
	var q Challenge
	raw, e := cl.Receive(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &q); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(captured)
	if e = cl.Send(context.Background(), b); e != nil {
		t.Fatal(e)
	}
	if e = <-done; !IsCode(e, Unauthorized) {
		t.Fatalf("got %v", e)
	}
}

func TestEstablishedConnectionClosesAfterCredentialRevocation(t *testing.T) {
	r, p, en, c := fixture(t)
	s := serverFor(t, r, p, c, 3)
	cl, sv := pair()
	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), sv) }()
	var q Challenge
	raw, _ := cl.Receive(context.Background())
	_ = json.Unmarshal(raw, &q)
	proof := fleetidentity.ShipProof(en.Credential.Secret, transcript(q, 1, en.Credential.CredentialID))
	sendRaw(t, cl, Authenticate{1, en.Credential.CredentialID, base64.RawURLEncoding.EncodeToString(proof)})
	raw, _ = cl.Receive(context.Background())
	var ok Accepted
	_ = json.Unmarshal(raw, &ok)
	shipStatus := status(en.ShipID)
	sendRaw(t, cl, Frame{Version: 1, ShipID: en.ShipID, Generation: ok.Generation, Number: 1, Type: "snapshot", Snapshot: &shipStatus})
	_, _ = cl.Receive(context.Background())
	if e := r.RevokeShipCredential(en.ShipID, en.Credential.CredentialID); e != nil {
		t.Fatal(e)
	}
	sendRaw(t, cl, Frame{Version: 1, ShipID: en.ShipID, Generation: ok.Generation, Number: 2, Type: "heartbeat"})
	if e := <-done; !IsCode(e, Revoked) {
		t.Fatalf("got %v", e)
	}
}

func TestCloseAllDeterministicallyClosesActiveTunnels(t *testing.T) {
	r, p, _, c := fixture(t)
	s := serverFor(t, r, p, c, 9)
	clientSide, serverSide := pair()
	s.active["shp_0123456789abcdef"] = activeConnection{generation: 1, channel: &ownedChannel{Channel: serverSide}}
	s.CloseAll()
	if _, err := clientSide.Receive(context.Background()); err == nil {
		t.Fatal("active tunnel remained open")
	}
}

type nonIdempotentChannel struct {
	mu           sync.Mutex
	closes       int
	closeStarted chan struct{}
	releaseClose chan struct{}
	startedOnce  sync.Once
}

func (c *nonIdempotentChannel) Send(context.Context, []byte) error { return nil }
func (c *nonIdempotentChannel) Receive(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *nonIdempotentChannel) PeerServiceIdentity() string { return "authenticated-ship" }
func (c *nonIdempotentChannel) Close() error {
	c.mu.Lock()
	c.closes++
	n := c.closes
	c.mu.Unlock()
	if n != 1 {
		panic("non-idempotent channel closed more than once")
	}
	c.startedOnce.Do(func() { close(c.closeStarted) })
	if c.releaseClose != nil {
		<-c.releaseClose
	}
	return nil
}
func (c *nonIdempotentChannel) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func TestRegistrationDuringShutdownIsRejectedWithoutBecomingActive(t *testing.T) {
	r, p, _, c := fixture(t)
	s := serverFor(t, r, p, c, 10)
	blocker := &nonIdempotentChannel{closeStarted: make(chan struct{}), releaseClose: make(chan struct{})}
	s.active["shp_active"] = activeConnection{generation: 1, channel: &ownedChannel{Channel: blocker}}
	shutdownDone := make(chan struct{})
	go func() { s.CloseAll(); close(shutdownDone) }()
	<-blocker.closeStarted // CloseAll has changed state and completed its snapshot.

	racing := &nonIdempotentChannel{closeStarted: make(chan struct{})}
	owned := &ownedChannel{Channel: racing}
	if _, _, registered := s.register("shp_racing", activeConnection{generation: 2, channel: owned}); registered {
		t.Fatal("registration succeeded after shutdown began")
	}
	_ = owned.Close() // the same deferred cleanup used by Serve
	s.mu.Lock()
	_, active := s.active["shp_racing"]
	s.mu.Unlock()
	if active || racing.closeCount() != 1 {
		t.Fatalf("racing tunnel active=%v closes=%d", active, racing.closeCount())
	}
	close(blocker.releaseClose)
	<-shutdownDone
}

func TestRepeatedCloseAllWaitsAndNonIdempotentChannelsCloseOnce(t *testing.T) {
	r, p, _, c := fixture(t)
	s := serverFor(t, r, p, c, 11)
	ch := &nonIdempotentChannel{closeStarted: make(chan struct{}), releaseClose: make(chan struct{})}
	owned := &ownedChannel{Channel: ch}
	s.active["shp_active"] = activeConnection{generation: 1, channel: owned}
	first, second := make(chan struct{}), make(chan struct{})
	go func() { s.CloseAll(); close(first) }()
	<-ch.closeStarted
	go func() { s.CloseAll(); close(second) }()
	select {
	case <-second:
		t.Fatal("concurrent CloseAll returned before closing completed")
	default:
	}
	close(ch.releaseClose)
	<-first
	<-second
	s.CloseAll()
	_ = owned.Close() // per-tunnel cleanup racing or following shutdown is harmless.
	if got := ch.closeCount(); got != 1 {
		t.Fatalf("underlying Close calls = %d, want 1", got)
	}
}

func TestMaliciousPersonaIdentifierCannotEnterWireFrame(t *testing.T) {
	canary := "RAW_PERSONA_CANARY"
	raw := []byte(`{"version":1,"ship_id":"shp_0123456789abcdef","generation":1,"number":1,"type":"snapshot","snapshot":{"personas":[{"slot":0,"session":"idle","turn":"none","activity":"idle","approval_waiting":false,"persona":"` + canary + `"}]}}`)
	var f Frame
	if err := strict(raw, &f); !IsCode(err, ProtocolViolation) {
		t.Fatalf("identifier-bearing frame accepted: %v", err)
	}
	b, err := json.Marshal(Frame{Version: 1, ShipID: "shp_0123456789abcdef", Generation: 1, Number: 1, Type: "snapshot", Snapshot: ptrSnapshot(status(""))})
	if err != nil || bytes.Contains(b, []byte(`"persona"`)) || bytes.Contains(b, []byte(canary)) {
		t.Fatalf("publishable frame carries identifier field: %s (%v)", b, err)
	}
}

func TestRunLocalEventWireDropsIrrelevantFieldCanaries(t *testing.T) {
	waiting := true
	tests := []fleetobserve.LocalEvent{
		{PersonaIndex: 0, Kind: fleetobserve.SessionStateEvent, Data: fleetobserve.EventDataV1{Session: fleetobserve.SessionWorking, Text: "TEXT_CANARY", Label: "LABEL_CANARY", RequestClass: "CONTROLLER_CANARY"}},
		{PersonaIndex: 0, Kind: fleetobserve.ActivityEvent, Data: fleetobserve.EventDataV1{Activity: fleetobserve.ActivityCommand, Label: "POLICY_CANARY", Text: "PERSONA_CANARY"}},
		{PersonaIndex: 0, Kind: fleetobserve.ApprovalWaiting, Data: fleetobserve.EventDataV1{Waiting: &waiting, ReasonCode: "APPROVAL_CANARY", Text: "UNKNOWN_CANARY"}},
	}
	// Use the production validator through a temporary canonical inventory.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "agents", "backend.toml"), []byte("name='backend'\ndeveloper_instructions='Test backend.'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := fleetobserve.OpenLocalStateAdapter(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range tests {
		normalized, err := adapter.Event(in)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := wireEvent(0, normalized)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"TEXT_CANARY", "LABEL_CANARY", "CONTROLLER_CANARY", "POLICY_CANARY", "PERSONA_CANARY", "APPROVAL_CANARY", "UNKNOWN_CANARY"} {
			if bytes.Contains(b, []byte(canary)) {
				t.Fatalf("irrelevant field survived in %s", b)
			}
		}
		if _, err := projectEvent("shp_0123456789abcdef", wire); err != nil {
			t.Fatalf("wire rejected: %v (%s)", err, b)
		}
	}
}

func TestWireEventRejectsIrrelevantFieldsForKind(t *testing.T) {
	raw := []byte(`{"slot":0,"kind":"session.state","data":{"session":"working","text":"CANARY"}}`)
	var event WireEvent
	if err := strict(raw, &event); err != nil {
		t.Fatal(err)
	}
	if _, err := projectEvent("shp_0123456789abcdef", event); !IsCode(err, ProtocolViolation) {
		t.Fatalf("irrelevant field accepted: %v", err)
	}
}

func ptrSnapshot(v WireSnapshot) *WireSnapshot { return &v }
func sendRaw(t *testing.T, ch Channel, v any) {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = ch.Send(context.Background(), b); e != nil {
		t.Fatal(e)
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		got, e := RetryDelay(i, time.Second, 5*time.Second)
		if e != nil || got != w {
			t.Fatalf("attempt %d: %v %v", i, got, e)
		}
	}
}
