package fleetsteer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/livesession"
)

type fakeClock struct{ at time.Time }

func (f *fakeClock) Now() time.Time { return f.at }

type fakeTunnel struct {
	mu     sync.Mutex
	calls  int
	last   deliveryV1
	result livesession.RemoteSteerResult
	wait   <-chan struct{}
}

type blockingReader struct {
	entered chan struct{}
	release chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}

func TestAtomicTargetInstallHidesProjectionUntilAliasesCommit(t *testing.T) {
	now := time.Unix(1000, 0)
	opaque := fleetobserve.OpaquePersonaReference("ship", 0)
	interruptRef := "int_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	random := &blockingReader{entered: make(chan struct{}), release: make(chan struct{})}
	ep := &ShipEndpoint{
		fleetID: "fleet", shipID: "ship", fleetEpoch: 2, generation: 3,
		steerClock: &fakeClock{at: now}, interruptClock: &fakeClock{at: now}, targets: map[string]livesession.RemoteSteerTarget{},
		interruptTargets: map[string]livesession.RemoteInterruptTarget{}, interruptPersonaAliases: map[string]string{},
		interruptPublicPersonas: map[string]string{}, interruptRandom: random,
	}
	target := livesession.RemoteSteerTarget{Reference: "steer", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: "session", Backend: "codex", ThreadID: "thread", TurnID: "turn", ExpiresAt: now.Add(time.Minute)}
	installDone := make(chan error, 1)
	go func() {
		installDone <- ep.InstallTargetsAndInterruptPersonaAliases([]livesession.RemoteSteerTarget{target}, map[string]string{opaque: "backend"})
	}()
	<-random.entered // The endpoint lock is held before any target is committed.

	interruptDone := make(chan error, 1)
	interruptStarted := make(chan struct{})
	go func() {
		close(interruptStarted)
		interruptDone <- ep.InstallInterruptTarget(context.Background(), fleetinterrupt.TargetInstallV1{FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: opaque, InterruptTargetRef: interruptRef, ExpiresAt: now.Add(time.Minute)})
	}()
	<-interruptStarted
	select {
	case err := <-interruptDone:
		t.Fatalf("interrupt install observed an uncommitted projection: %v", err)
	default:
	}

	close(random.release)
	if err := <-installDone; err != nil {
		t.Fatal(err)
	}
	if err := <-interruptDone; err != nil {
		t.Fatalf("interrupt install did not observe the atomic target/alias commit: %v", err)
	}
	if got := ep.interruptPublicPersonas[interruptRef]; got != opaque {
		t.Fatalf("public persona=%q want %q", got, opaque)
	}
}

func TestInterruptTargetBindsOpaquePersonaToPrivateTuple(t *testing.T) {
	now := time.Unix(1000, 0)
	opaque := fleetobserve.OpaquePersonaReference("ship", 0)
	ref := "int_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ep := &ShipEndpoint{
		fleetID: "fleet", shipID: "ship", fleetEpoch: 2, generation: 3,
		steerClock: &fakeClock{at: now}, interruptClock: &fakeClock{at: now}, targets: map[string]livesession.RemoteSteerTarget{
			"steer": {FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: "session", Backend: "codex", ThreadID: "thread", TurnID: "turn", ExpiresAt: now.Add(time.Minute)},
		},
		interruptTargets:        map[string]livesession.RemoteInterruptTarget{},
		interruptPersonaAliases: map[string]string{opaque: "backend"},
		interruptPublicPersonas: map[string]string{},
	}
	install := fleetinterrupt.TargetInstallV1{FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: opaque, InterruptTargetRef: ref, ExpiresAt: now.Add(time.Minute)}
	for _, persona := range []string{"backend", "prf_bad!"} {
		bad := install
		bad.Persona = persona
		if err := ep.InstallInterruptTarget(context.Background(), bad); err == nil {
			t.Fatalf("installed non-opaque persona %q", persona)
		}
	}
	if err := ep.InstallInterruptTarget(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	stored := ep.interruptTargets[ref]
	if stored.Persona != "backend" || stored.Backend != "codex" || ep.interruptPublicPersonas[ref] != opaque {
		t.Fatalf("binding public=%q private persona=%q backend=%q", ep.interruptPublicPersonas[ref], stored.Persona, stored.Backend)
	}
	request := fleetinterrupt.DeliveryV1{Operator: shipScopedPrincipal(fleetidentity.InterruptTurnCapability), Request: livesession.RemoteInterruptRequest{FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: opaque, TargetReference: ref, OperationID: "operation"}}
	if got := ep.DeliverInterrupt(context.Background(), request); got.ReasonCode != "shutdown" {
		t.Fatalf("opaque binding refused before private delivery: %+v", got)
	}
	request.Request.Persona = "backend"
	if got := ep.DeliverInterrupt(context.Background(), request); got.ReasonCode != "stale_target" {
		t.Fatalf("private persona accepted on public surface: %+v", got)
	}
}

func TestAtomicLocalTargetsUseSteerClockAndInterruptRefsUseCapabilityClock(t *testing.T) {
	wallNow := time.Now()
	interruptClock := &fakeClock{at: time.Unix(1000, 0)}
	opaque := fleetobserve.OpaquePersonaReference("ship", 0)
	ep := &ShipEndpoint{
		fleetID: "fleet", shipID: "ship", fleetEpoch: 2, generation: 3,
		steerClock: &fakeClock{at: wallNow}, interruptClock: interruptClock,
		targets: map[string]livesession.RemoteSteerTarget{}, interruptTargets: map[string]livesession.RemoteInterruptTarget{},
		interruptPersonaAliases: map[string]string{}, interruptPublicPersonas: map[string]string{},
		interruptRandom: strings.NewReader(strings.Repeat("x", 64)),
	}
	local := livesession.RemoteSteerTarget{Reference: "steer", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: "session", Backend: "codex", ThreadID: "thread", TurnID: "turn", ExpiresAt: wallNow.Add(TargetLifetime)}
	if err := ep.InstallTargetsAndInterruptPersonaAliases([]livesession.RemoteSteerTarget{local}, map[string]string{opaque: "backend"}); err != nil {
		t.Fatalf("wall-clock local target install: %v", err)
	}

	ref := "int_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	expires := interruptClock.Now().Add(time.Second)
	if err := ep.InstallInterruptTarget(context.Background(), fleetinterrupt.TargetInstallV1{FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: opaque, InterruptTargetRef: ref, ExpiresAt: expires}); err != nil {
		t.Fatalf("frozen-clock interrupt target install: %v", err)
	}
	request := fleetinterrupt.DeliveryV1{Operator: shipScopedPrincipal(fleetidentity.InterruptTurnCapability), Request: livesession.RemoteInterruptRequest{FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: opaque, TargetReference: ref, OperationID: "operation"}}
	if got := ep.DeliverInterrupt(context.Background(), request); got.ReasonCode != "shutdown" {
		t.Fatalf("unexpired interrupt ref was not delivered: %+v", got)
	}
	interruptClock.at = expires
	if got := ep.DeliverInterrupt(context.Background(), request); got.ReasonCode != "target_expired" {
		t.Fatalf("exact-expiry interrupt ref result=%+v", got)
	}
}

func (f *fakeTunnel) Deliver(ctx context.Context, d deliveryV1) livesession.RemoteSteerResult {
	f.mu.Lock()
	f.calls++
	f.last = d
	f.mu.Unlock()
	if f.wait != nil {
		select {
		case <-f.wait:
		case <-ctx.Done():
			return result(d.Request.OperationID, "indeterminate", "delivery_unknown")
		}
	}
	r := f.result
	r.OperationID = d.Request.OperationID
	return r
}

func fixture(t *testing.T) (*fleetidentity.Registry, fleetidentity.IssuedOperatorCredential, *Service, SubmitV1, *fakeClock) {
	t.Helper()
	clock := &fakeClock{time.Unix(1000, 0)}
	r, err := fleetidentity.NewRegistry("fleet-000000000001", clock, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := r.Enroll(a.ArtifactID, a.Secret, "txn-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	op, err := r.IssueOperator("operator-00000001", []string{ship.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(op.Record.FleetID, r, clock, &counterReader{})
	if err != nil {
		t.Fatal(err)
	}
	in := SubmitV1{SchemaVersion: 1, FleetID: op.Record.FleetID, FleetEpoch: 2, ShipID: ship.ShipID, ConnectionGeneration: 3, Persona: "backend", SteerTargetRef: "target-00000000000001", Message: "hello", OperationID: steerOperationID(1)}
	return r, op, s, in, clock
}

// shipScopedPrincipal is a complete operator grant for the "fleet"/"ship" pair
// used by the endpoint tests.
func shipScopedPrincipal(capability string) fleetidentity.OperatorPrincipal {
	return fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "operator-00000001", CredentialID: "opc_000000000000001", Capability: capability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
}

// steerOperationID builds a deterministic caller-owned 256-bit identifier.
func steerOperationID(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}

type counterReader struct{ n byte }

func (r *counterReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

func TestCapabilityAuthenticatedOneShipTransport(t *testing.T) {
	_, op, service, in, _ := fixture(t)
	tunnel := &fakeTunnel{result: result("", "accepted", "accepted")}
	disconnect, err := service.Connect(in.ShipID, in.ConnectionGeneration, tunnel)
	if err != nil {
		t.Fatal(err)
	}
	got := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in)
	if got.Outcome != livesession.RemoteSteerAccepted || got.OperationID == "" {
		t.Fatalf("result=%+v", got)
	}
	tunnel.mu.Lock()
	delivered := tunnel.last
	calls := tunnel.calls
	tunnel.mu.Unlock()
	digest := sha256.Sum256([]byte(in.Message))
	if calls != 1 || delivered.Operator.SubjectID != op.Record.SubjectID || delivered.Operator.CredentialGeneration != op.Record.CredentialGeneration || delivered.Request.OperationNonce == "" || delivered.Request.MessageSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("delivery=%+v calls=%d", delivered, calls)
	}
	if delivered.Request.OperationID == delivered.Request.OperationNonce {
		t.Fatal("operation id and nonce were not independent")
	}
	disconnect()
	offline := in
	offline.OperationID = steerOperationID(9)
	if got = service.Submit(context.Background(), op.Record.CredentialID, op.Secret, offline); got.ReasonCode != "ship_offline" {
		t.Fatalf("offline=%+v", got)
	}
}

func TestObserverShipRevocationReconnectAndRestartFailClosed(t *testing.T) {
	r, op, service, in, clock := fixture(t)
	tunnel := &fakeTunnel{result: result("", "accepted", "accepted")}
	if _, err := service.Connect(in.ShipID, 3, tunnel); err != nil {
		t.Fatal(err)
	}
	observer, err := r.IssueObserver([]string{in.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Submit(context.Background(), observer.CredentialID, observer.Secret, in); got.ReasonCode != "unauthorized" {
		t.Fatalf("observer mutated: %+v", got)
	}
	if got := service.Submit(context.Background(), "ship-credential00", "ship-secret", in); got.ReasonCode != "unauthorized" {
		t.Fatalf("ship mutated: %+v", got)
	}
	if err := r.RevokeOperatorCredential(op.Record.CredentialID, op.Record.CredentialGeneration); err != nil {
		t.Fatal(err)
	}
	if got := service.Submit(context.Background(), op.Record.CredentialID, op.Secret, in); got.ReasonCode != "unauthorized" {
		t.Fatalf("revoked=%+v", got)
	}
	service2, _ := NewService(in.FleetID, r, clock, &counterReader{})
	if got := service2.Submit(context.Background(), op.Record.CredentialID, op.Secret, in); got.ReasonCode != "unauthorized" {
		t.Fatalf("restart=%+v", got)
	}
	if _, err := service.Connect(in.ShipID, 2, tunnel); err == nil {
		t.Fatal("older reconnect replaced current generation")
	}
}

func TestBoundedDeliveryReturnsIndeterminateWithoutReplay(t *testing.T) {
	_, op, service, in, _ := fixture(t)
	blocked := make(chan struct{})
	tunnel := &fakeTunnel{wait: blocked}
	if _, err := service.Connect(in.ShipID, 3, tunnel); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := service.Submit(ctx, op.Record.CredentialID, op.Secret, in)
	if got.Outcome != livesession.RemoteSteerIndeterminate || got.ReasonCode != "delivery_unknown" {
		t.Fatalf("result=%+v", got)
	}
	tunnel.mu.Lock()
	calls := tunnel.calls
	tunnel.mu.Unlock()
	if calls > 1 {
		t.Fatalf("replayed calls=%d", calls)
	}
}
