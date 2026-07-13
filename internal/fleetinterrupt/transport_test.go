package fleetinterrupt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/livesession"
)

func interruptRef(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func interruptPersonaRef() string { return fleetobserve.OpaquePersonaReference("ship", 0) }

type fakeAuthority struct {
	principal fleetidentity.OperatorPrincipal
	record    fleetidentity.OperatorCredentialRecord
	err       error
}

func (f fakeAuthority) AuthenticateOperator(string, string, string, string) (fleetidentity.OperatorPrincipal, error) {
	return f.principal, f.err
}
func (f fakeAuthority) AuthenticateOperatorCredential(string, string) (fleetidentity.OperatorPrincipal, error) {
	return f.principal, f.err
}
func (f fakeAuthority) InspectOperator(string, uint64) (fleetidentity.OperatorCredentialRecord, error) {
	return f.record, f.err
}

type fakeEndpoint struct {
	calls  int
	result livesession.RemoteInterruptResult
}

type targetEndpoint struct {
	fakeEndpoint
	installed []TargetInstallV1
	err       error
}

func (f *targetEndpoint) InstallInterruptTarget(_ context.Context, in TargetInstallV1) error {
	f.installed = append(f.installed, in)
	return f.err
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func TestTargetDiscoveryDelayDoesNotConsumeCapabilityLifetime(t *testing.T) {
	baseline := time.Unix(1_700_000_000, 0)
	capabilityClock := &testClock{now: baseline}
	observationClock := &testClock{now: baseline}
	p, err := fleetobserve.New(fleetobserve.Config{FleetID: "fleet", FleetEpoch: "observation-epoch", MaxShips: 2, MaxPersonas: 2, MaxSnapshotBytes: 1 << 16, MaxEventBytes: 8192, PerShipIngress: 2, ReplayCapacity: 4, MaxSubscribers: 2, MaxPageSize: 4, MaxTerminalMetadata: 4, LeaseDuration: time.Minute, StaleRetention: time.Minute, Clock: observationClock})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Connect("ship", 1); err != nil {
		t.Fatal(err)
	}
	if err := p.InstallSnapshot("ship", 1, fleetobserve.ShipStatusV1{ShipID: "ship", Personas: []fleetobserve.PersonaStatusV1{{Persona: "backend", Installed: true, Session: fleetobserve.SessionWorking, Turn: fleetobserve.TurnActive, Activity: fleetobserve.ActivityOther}}}); err != nil {
		t.Fatal(err)
	}
	principal := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	s, err := NewServiceWithConfig("fleet", fakeAuthority{principal: principal}, strings.NewReader(strings.Repeat("x", 128)), ServiceConfig{Clock: capabilityClock, TargetLifetime: 500 * time.Millisecond, ObservationFreshness: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindProjection(p, 9); err != nil {
		t.Fatal(err)
	}
	ep := &targetEndpoint{}
	if _, err := s.Connect("ship", 1, ep); err != nil {
		t.Fatal(err)
	}

	// Simulate discovery startup being delayed longer than the capability
	// lifetime while the injected capability clock has not advanced.
	observationClock.Advance(750 * time.Millisecond)
	got := s.Targets(context.Background(), principal, "", 0)
	if len(got.Targets) != 1 {
		t.Fatalf("delayed current target discovery=%+v", got)
	}
	if want := baseline.Add(500 * time.Millisecond); !got.Targets[0].ExpiresAt.Equal(want) {
		t.Fatalf("target expiry=%v want capability-clock expiry=%v", got.Targets[0].ExpiresAt, want)
	}

	observationClock.Advance(1250 * time.Millisecond)
	if stale := s.Targets(context.Background(), principal, "", 0); len(stale.Targets) != 0 {
		t.Fatalf("observation at exact freshness boundary remained discoverable: %+v", stale)
	}
}

func TestTargetDiscoveryDiagnosticsCoverEverySilentConnectionBranch(t *testing.T) {
	baseline := time.Unix(1_700_000_000, 0)
	clock := &testClock{now: baseline}
	p, err := fleetobserve.New(fleetobserve.Config{FleetID: "fleet", FleetEpoch: "observation-epoch", MaxShips: 2, MaxPersonas: 2, MaxSnapshotBytes: 1 << 16, MaxEventBytes: 8192, PerShipIngress: 2, ReplayCapacity: 4, MaxSubscribers: 2, MaxPageSize: 4, MaxTerminalMetadata: 4, LeaseDuration: time.Minute, StaleRetention: time.Minute, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Connect("ship", 1); err != nil {
		t.Fatal(err)
	}
	if err = p.InstallSnapshot("ship", 1, fleetobserve.ShipStatusV1{ShipID: "ship", Personas: []fleetobserve.PersonaStatusV1{{Persona: interruptPersonaRef(), Installed: true, Session: fleetobserve.SessionWorking, Turn: fleetobserve.TurnActive, Activity: fleetobserve.ActivityOther}}}); err != nil {
		t.Fatal(err)
	}
	principal := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	s, err := NewServiceWithConfig("fleet", fakeAuthority{principal: principal}, strings.NewReader(strings.Repeat("x", 512)), ServiceConfig{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindProjection(p, 9); err != nil {
		t.Fatal(err)
	}

	assertDelta := func(want TargetDiagnostics) {
		t.Helper()
		before := s.TargetDiagnosticsForTest()
		if got := s.Targets(context.Background(), principal, "", 0); len(got.Targets) != 0 {
			t.Fatalf("unexpected target: %+v", got)
		}
		after := s.TargetDiagnosticsForTest()
		got := TargetDiagnostics{after.MissingConnection - before.MissingConnection, after.GenerationMismatch - before.GenerationMismatch, after.NotTargetInstaller - before.NotTargetInstaller, after.InstallFailure - before.InstallFailure}
		if got != want {
			t.Fatalf("diagnostic delta=%+v want=%+v", got, want)
		}
	}
	assertDelta(TargetDiagnostics{MissingConnection: 1})
	s.connections["ship"] = connection{generation: 2, endpoint: &targetEndpoint{}}
	assertDelta(TargetDiagnostics{GenerationMismatch: 1})
	s.connections["ship"] = connection{generation: 1, endpoint: &fakeEndpoint{}}
	assertDelta(TargetDiagnostics{NotTargetInstaller: 1})
	s.connections["ship"] = connection{generation: 1, endpoint: &targetEndpoint{err: errors.New("private failure")}}
	assertDelta(TargetDiagnostics{InstallFailure: 1})
}

func (f *fakeEndpoint) DeliverInterrupt(_ context.Context, d DeliveryV1) livesession.RemoteInterruptResult {
	f.calls++
	r := f.result
	r.OperationID = d.Request.OperationID
	return r
}

func TestServiceCapabilityIsolationAndExactGenerationTransport(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	a := fakeAuthority{principal: p}
	s, err := NewService("fleet", a, strings.NewReader(strings.Repeat("x", 128)))
	if err != nil {
		t.Fatal(err)
	}
	ep := &fakeEndpoint{result: livesession.RemoteInterruptResult{SchemaVersion: 1, Outcome: livesession.RemoteInterruptInterrupted, ReasonCode: "interrupted"}}
	stop, err := s.Connect("ship", 3, ep)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	in := SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: interruptPersonaRef(), InterruptTargetRef: interruptRef(1), OperationID: interruptRef(2)}
	r := s.Submit(context.Background(), "credential", "secret", in)
	if r.Outcome != livesession.RemoteInterruptInterrupted || ep.calls != 1 || r.OperationID != in.OperationID {
		t.Fatalf("result=%+v calls=%d", r, ep.calls)
	}
	a.principal.Capability = fleetidentity.SteerTurnCapability
	s.authority = a
	r = s.Submit(context.Background(), "credential", "secret", in)
	if r.Outcome != livesession.RemoteInterruptRefused || r.ReasonCode != "unauthorized" || ep.calls != 1 {
		t.Fatalf("steer grant reached interrupt: %+v", r)
	}
	in.ConnectionGeneration = 4
	in.OperationID = interruptRef(5)
	s.authority = fakeAuthority{principal: p}
	r = s.Submit(context.Background(), "credential", "secret", in)
	if r.ReasonCode != "stale_generation" || ep.calls != 1 {
		t.Fatalf("stale generation reached endpoint: %+v", r)
	}
}

func TestServiceDeadlineIsBounded(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	s, _ := NewService("fleet", fakeAuthority{principal: p}, strings.NewReader(strings.Repeat("x", 128)))
	ep := endpointFunc(func(ctx context.Context, _ DeliveryV1) livesession.RemoteInterruptResult {
		<-ctx.Done()
		return livesession.RemoteInterruptResult{}
	})
	_, _ = s.Connect("ship", 1, ep)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := s.Submit(ctx, "credential", "secret", SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, Persona: interruptPersonaRef(), InterruptTargetRef: interruptRef(3), OperationID: interruptRef(4)})
	if r.Outcome != livesession.RemoteInterruptIndeterminate || r.ReasonCode != "delivery_unknown" {
		t.Fatal(r)
	}
}

func TestServiceRequiresCanonicalOpaquePersonaReference(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	s, _ := NewService("fleet", fakeAuthority{principal: p}, strings.NewReader(strings.Repeat("x", 256)))
	ep := &fakeEndpoint{result: livesession.RemoteInterruptResult{SchemaVersion: 1, Outcome: livesession.RemoteInterruptInterrupted, ReasonCode: "interrupted"}}
	_, _ = s.Connect("ship", 1, ep)
	base := SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, InterruptTargetRef: interruptRef(8)}
	for i, persona := range []string{"backend", "prf_", "prf_0123456789abcdef01234567!"} {
		in := base
		in.Persona, in.OperationID = persona, interruptRef(byte(20+i))
		if got := s.Submit(context.Background(), "credential", "secret", in); got.ReasonCode != "invalid_request" {
			t.Fatalf("persona %q result = %+v", persona, got)
		}
	}
	in := base
	in.Persona, in.OperationID = interruptPersonaRef(), interruptRef(30)
	if got := s.Submit(context.Background(), "credential", "secret", in); got.Outcome != livesession.RemoteInterruptInterrupted || ep.calls != 1 {
		t.Fatalf("canonical reference result=%+v calls=%d", got, ep.calls)
	}
}

type endpointFunc func(context.Context, DeliveryV1) livesession.RemoteInterruptResult

func (f endpointFunc) DeliverInterrupt(c context.Context, d DeliveryV1) livesession.RemoteInterruptResult {
	return f(c, d)
}

func TestTerminalPersistFailureIsNeverPublishedAsDefinitive(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	for _, tc := range []struct {
		name   string
		result livesession.RemoteInterruptResult
	}{
		{"interrupted", result("", livesession.RemoteInterruptInterrupted, "interrupted")},
		{"already-terminal", result("", livesession.RemoteInterruptAlreadyTerminal, "already_terminal")},
		{"refused", result("", livesession.RemoteInterruptRefused, "stale_target")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s, err := OpenService("fleet", fakeAuthority{principal: p}, strings.NewReader(strings.Repeat("x", 256)), dir)
			if err != nil {
				t.Fatal(err)
			}
			ep := &fakeEndpoint{result: tc.result}
			_, _ = s.Connect("ship", 1, ep)
			failed := false
			s.persistFailure = func() error {
				if !failed && hasFinishedFleetOperation(s) {
					failed = true
					return errors.New("injected terminal persist failure")
				}
				return nil
			}
			in := SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, Persona: interruptPersonaRef(), InterruptTargetRef: interruptRef(41), OperationID: interruptRef(42)}
			got := s.Submit(context.Background(), "credential", "secret", in)
			assertPersistUncertain(t, got)
			if replay, ok := s.Query(in.OperationID); !ok {
				t.Fatal("terminal operation missing from query")
			} else {
				assertPersistUncertain(t, replay)
			}
			reopened, err := OpenService("fleet", fakeAuthority{principal: p}, strings.NewReader(strings.Repeat("y", 256)), filepath.Clean(dir))
			if err != nil {
				t.Fatal(err)
			}
			if replay, ok := reopened.Query(in.OperationID); !ok {
				t.Fatal("terminal operation missing after reopen")
			} else {
				assertPersistUncertain(t, replay)
			}
		})
	}
}

func TestTerminalPersistFailureReleasesConcurrentReplayWaitersOnlyWithUncertainty(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := OpenService("fleet", fakeAuthority{principal: p}, strings.NewReader(strings.Repeat("x", 512)), dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	_, _ = s.Connect("ship", 1, endpointFunc(func(_ context.Context, d DeliveryV1) livesession.RemoteInterruptResult {
		once.Do(func() { close(entered) })
		<-release
		return result(d.Request.OperationID, livesession.RemoteInterruptInterrupted, "interrupted")
	}))
	failed := false
	s.persistFailure = func() error {
		if !failed && hasFinishedFleetOperation(s) {
			failed = true
			return errors.New("injected terminal persist failure")
		}
		return nil
	}
	in := SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, Persona: interruptPersonaRef(), InterruptTargetRef: interruptRef(51), OperationID: interruptRef(52)}
	first := make(chan livesession.RemoteInterruptResult, 1)
	waiter := make(chan livesession.RemoteInterruptResult, 1)
	go func() { first <- s.Submit(context.Background(), "credential", "secret", in) }()
	<-entered
	go func() { waiter <- s.Submit(context.Background(), "credential", "secret", in) }()
	for {
		s.mu.Lock()
		n := 0
		for _, count := range s.waiters {
			n += count
		}
		s.mu.Unlock()
		if n == 1 {
			break
		}
		runtime.Gosched()
	}
	close(release)
	assertPersistUncertain(t, <-first)
	assertPersistUncertain(t, <-waiter)
}

func TestAttemptedAuditFailureRollbackReplayAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rollbackFail bool
	}{
		{name: "durable-refusal"},
		{name: "rollback-failure", rollbackFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "credential", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			s, err := OpenService("fleet", fakeAuthority{principal: p}, nil, dir)
			if err != nil {
				t.Fatal(err)
			}
			s.audit.failure = func() error { return errors.New("injected attempted-audit failure") }
			persistCalls := 0
			if tc.rollbackFail {
				s.persistFailure = func() error {
					persistCalls++
					if persistCalls == 2 {
						return errors.New("injected rollback failure")
					}
					return nil
				}
			}
			in := SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, Persona: interruptPersonaRef(), InterruptTargetRef: interruptRef(61), OperationID: interruptRef(62)}
			const callers = 8
			start := make(chan struct{})
			results := make(chan livesession.RemoteInterruptResult, callers)
			for i := 0; i < callers; i++ {
				go func() {
					<-start
					results <- s.Submit(context.Background(), "credential", "secret", in)
				}()
			}
			close(start)
			assert := assertAuditUnavailable
			if tc.rollbackFail {
				assert = assertPersistUncertain
			}
			for i := 0; i < callers; i++ {
				assert(t, <-results)
			}
			if replay, ok := s.Query(in.OperationID); !ok {
				t.Fatal("operation missing from query")
			} else {
				assert(t, replay)
			}
			reopened, err := OpenService("fleet", fakeAuthority{principal: p}, nil, filepath.Clean(dir))
			if err != nil {
				t.Fatal(err)
			}
			if replay, ok := reopened.Query(in.OperationID); !ok {
				t.Fatal("operation missing after reopen")
			} else {
				assert(t, replay)
			}
		})
	}
}

func assertAuditUnavailable(t *testing.T, got livesession.RemoteInterruptResult) {
	t.Helper()
	if got.Outcome != livesession.RemoteInterruptRefused || got.ReasonCode != "audit_unavailable" {
		t.Fatalf("attempted-audit failure published as %+v", got)
	}
}

func assertPersistUncertain(t *testing.T, got livesession.RemoteInterruptResult) {
	t.Helper()
	if got.Outcome != livesession.RemoteInterruptIndeterminate || got.ReasonCode != "internal_uncertain" || got.RetryDisposition != livesession.RemoteInterruptReplaySameOperation {
		t.Fatalf("persistence uncertainty published as %+v", got)
	}
}

// Called only by persistFailure while the service lock is held.
func hasFinishedFleetOperation(s *Service) bool {
	for _, op := range s.operations {
		if op.finished {
			return true
		}
	}
	return false
}
