package fleetobserve

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestM3M6NormalizationDropsProjectedAuthorityAndSecrets(t *testing.T) {
	canary := "SECRET-CANARY-/home/operator-policy-approval-controller-command"
	s, err := NormalizeShipSnapshot("ship-a", ShipStatusV1{ShipID: "ship-a", ShipLabel: canary, Connectivity: Online, ConnectionGeneration: 99, Personas: []PersonaStatusV1{{Persona: "captain", Installed: true, Session: SessionWorking, Turn: TurnActive, Activity: ActivityCommand, SessionInstance: canary, TurnInstance: canary, Summary: &SafeSummaryV1{Code: canary, Text: canary}}}})
	if err != nil {
		t.Fatal(err)
	}
	e, err := NormalizeObservationEvent(ObservationEventV1{Persona: "captain", SessionInstance: canary, TurnInstance: canary, Kind: ActivityEvent, Data: EventDataV1{Activity: ActivityCommand, Label: canary, Text: canary, ReasonCode: canary, RequestClass: canary, WarningCode: canary}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(struct {
		S ShipStatusV1
		E ObservationEventV1
	}{s, e})
	if strings.Contains(string(b), canary) || strings.Contains(string(b), "/home/operator") {
		t.Fatalf("secret reached projection: %s", b)
	}
	if e.Data.Label != "command" || s.ShipLabel != "" || s.Personas[0].SessionInstance != "" {
		t.Fatalf("not reconstructed: %#v %#v", s, e)
	}
	if _, err = NormalizeObservationEvent(ObservationEventV1{Kind: AgentMessage, Data: EventDataV1{Text: canary}}); code(err) != InvalidInput {
		t.Fatalf("agent content accepted: %v", err)
	}
	if _, err = NormalizeObservationEvent(ObservationEventV1{Kind: EventKind("controller.command"), Data: EventDataV1{Text: canary}}); code(err) != InvalidInput {
		t.Fatalf("unknown type accepted: %v", err)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time      { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) add(d time.Duration) { f.mu.Lock(); f.t = f.t.Add(d); f.mu.Unlock() }
func config(c Clock) Config {
	return Config{FleetID: "fleet-1", FleetEpoch: "epoch-1", MaxShips: 4, MaxPersonas: 4, MaxSnapshotBytes: 1 << 16, MaxEventBytes: 8192, PerShipIngress: 2, ReplayCapacity: 3, MaxSubscribers: 2, MaxPageSize: 10, MaxTerminalMetadata: 8, LeaseDuration: 10 * time.Second, StaleRetention: 20 * time.Second, Clock: c}
}
func status(id string) ShipStatusV1 {
	return ShipStatusV1{ShipID: id, ShipLabel: "safe", Personas: []PersonaStatusV1{{Persona: "captain", Installed: true, Session: SessionWorking, Turn: TurnActive, Activity: ActivityCommand, ApprovalWaiting: true, SessionInstance: "s-projected", TurnInstance: "t-projected"}}}
}

func TestGenerationSnapshotCursorAndGaps(t *testing.T) {
	c := &fakeClock{t: time.Unix(100, 0)}
	p, _ := New(config(c))
	if err := p.Connect("ship-a", 1); err != nil {
		t.Fatal(err)
	}
	if got := p.Snapshot().Ships; len(got) != 0 {
		t.Fatal("online before snapshot")
	}
	if err := p.InstallSnapshot("ship-a", 1, status("ship-a")); err != nil {
		t.Fatal(err)
	}
	if err := p.Connect("ship-a", 2); err != nil {
		t.Fatal(err)
	}
	if err := p.InstallSnapshot("ship-a", 1, status("ship-a")); code(err) != StaleGeneration {
		t.Fatalf("stale install: %v", err)
	}
	if err := p.InstallSnapshot("ship-a", 2, status("ship-a")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := p.Enqueue("ship-a", 2, ObservationEventV1{Persona: "captain", Kind: ActivityEvent, Data: EventDataV1{Activity: ActivityOther, Label: "activity"}}); err != nil {
			if code(err) != IngressFull {
				t.Fatal(err)
			}
			break
		}
		if err := p.Drain("ship-a", 2); err != nil {
			t.Fatal(err)
		}
	}
	z := uint64(0)
	r, err := p.Read("epoch-1", &z, 10)
	if err != nil {
		t.Fatal(err)
	}
	if r.Gap == nil || r.Gap.Reason != HistoryDropped || r.Snapshot == nil {
		t.Fatalf("missing history gap: %#v", r)
	}
	ahead := r.NextCursor
	r, err = p.Read("epoch-1", &ahead, 10)
	if err != nil || r.Gap == nil || r.Gap.Reason != CursorAhead {
		t.Fatalf("ahead: %#v %v", r, err)
	}
	r, err = p.Read("old", &z, 10)
	if err != nil || r.Gap == nil || r.Gap.Reason != EpochChanged {
		t.Fatalf("epoch: %#v %v", r, err)
	}
}

func TestIngressOverflowExplicitAndNonInterference(t *testing.T) {
	c := &fakeClock{t: time.Unix(100, 0)}
	p, _ := New(config(c))
	for _, id := range []string{"a", "b"} {
		_ = p.Connect(id, 1)
		_ = p.InstallSnapshot(id, 1, status(id))
	}
	e := ObservationEventV1{Kind: ProjectionWarning, Data: EventDataV1{WarningCode: "safe"}}
	if err := p.Enqueue("a", 1, e); err != nil {
		t.Fatal(err)
	}
	_ = p.Enqueue("a", 1, e)
	if code(p.Enqueue("a", 1, e)) != IngressFull {
		t.Fatal("expected bounded overflow")
	}
	if err := p.Enqueue("b", 1, e); err != nil {
		t.Fatalf("unrelated ship blocked: %v", err)
	}
	if err := p.Drain("b", 1); err != nil {
		t.Fatal(err)
	}
	r, err := p.Resync("a", 1)
	if err != nil || r.Gap == nil || r.Gap.Reason != IngressDropped || r.Snapshot == nil {
		t.Fatalf("resync: %#v %v", r, err)
	}
}

func TestInjectedClockLivenessAndStaleGeneration(t *testing.T) {
	c := &fakeClock{t: time.Unix(100, 0)}
	p, _ := New(config(c))
	_ = p.Connect("a", 1)
	_ = p.InstallSnapshot("a", 1, status("a"))
	c.add(10 * time.Second)
	p.Expire()
	if p.Snapshot().Ships[0].Connectivity != Stale {
		t.Fatal("deadline must be stale")
	}
	_ = p.Connect("a", 2)
	_ = p.InstallSnapshot("a", 2, status("a"))
	if code(p.Heartbeat("a", 1)) != StaleGeneration || code(p.Disconnect("a", 1, true)) != StaleGeneration {
		t.Fatal("old generation altered liveness")
	}
	if err := p.Disconnect("a", 2, true); err != nil {
		t.Fatal(err)
	}
	if p.Snapshot().Ships[0].Connectivity != Offline {
		t.Fatal("clean close")
	}
}

func TestStrictAllowlistAndCanaries(t *testing.T) {
	c := &fakeClock{t: time.Unix(100, 0)}
	p, _ := New(config(c))
	_ = p.Connect("a", 1)
	bad := status("a")
	bad.Personas[0].Session = "mystery"
	if code(p.InstallSnapshot("a", 1, bad)) != InvalidInput {
		t.Fatal("unknown state accepted")
	}
	bad = status("a")
	bad.ShipLabel = "x\x1b[31m"
	if code(p.InstallSnapshot("a", 1, bad)) != InvalidInput {
		t.Fatal("terminal control accepted")
	}
	if err := p.InstallSnapshot("a", 1, status("a")); err != nil {
		t.Fatal(err)
	}
	canaries := []string{"raw-command-secret", "policy-secret", "approval-id-secret", "/host/private/path", "controller-secret"}
	b, _ := json.Marshal(p.Snapshot())
	for _, x := range canaries {
		if strings.Contains(string(b), x) {
			t.Fatalf("canary leaked: %s", x)
		}
	}
	if err := p.Enqueue("a", 1, ObservationEventV1{Kind: AgentMessage, Data: EventDataV1{Text: strings.Repeat("x", 4097)}}); code(err) != InvalidInput {
		t.Fatal("oversized event accepted")
	}
}

func TestConcurrentSupersedeHasOneWinner(t *testing.T) {
	p, _ := New(config(&fakeClock{t: time.Unix(100, 0)}))
	var wg sync.WaitGroup
	for _, g := range []uint64{2, 3, 4, 5, 6, 7} {
		wg.Add(1)
		go func(g uint64) { defer wg.Done(); _ = p.Connect("a", g) }(g)
	}
	wg.Wait()
	if err := p.InstallSnapshot("a", 7, status("a")); err != nil {
		t.Fatal(err)
	}
	for g := uint64(1); g < 7; g++ {
		if err := p.Heartbeat("a", g); code(err) != StaleGeneration {
			t.Fatalf("generation %d accepted", g)
		}
	}
}

func code(err error) Code {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}
