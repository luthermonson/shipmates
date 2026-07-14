package fleetobserve

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestSyntheticFleetGentleSoak is a bounded, fake-Codex observation soak. It
// is intentionally opt-in so ordinary package tests never acquire a
// long-running test. The default duration is the Captain-approved 30 minutes.
func TestSyntheticFleetGentleSoak(t *testing.T) {
	raw := os.Getenv("SHIPMATES_SOAK_DURATION")
	if raw == "" {
		t.Skip("set SHIPMATES_SOAK_DURATION (at most 30m) to run the opt-in fake-Codex soak")
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > 30*time.Minute {
		t.Fatalf("invalid bounded soak duration %q", raw)
	}
	const (
		fleetID        = "fleet-soak"
		epoch          = "epoch-soak"
		ships          = 2
		perShipIngress = 8
		replay         = 64
		pageSize       = 16
		eventInterval  = time.Second
		heartbeat      = 5 * time.Second
		reconnect      = 30 * time.Second
	)

	baseGoroutines := runtime.NumGoroutine()
	baseFDs := countOpenFDs(t)
	var initial, sample, peak runtime.MemStats
	runtime.ReadMemStats(&initial)
	peak = initial

	p, err := New(Config{FleetID: fleetID, FleetEpoch: epoch, MaxShips: ships, MaxPersonas: 1, MaxSnapshotBytes: 65536, MaxEventBytes: 8192, PerShipIngress: perShipIngress, ReplayCapacity: replay, MaxSubscribers: 2, MaxPageSize: pageSize, MaxTerminalMetadata: 256, LeaseDuration: time.Minute, StaleRetention: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ships; i++ {
		id := "ship-" + strconv.Itoa(i+1)
		if err := p.Connect(id, 1); err != nil {
			t.Fatal(err)
		}
		if err := p.InstallSnapshot(id, 1, ShipStatusV1{ShipLabel: "synthetic-" + strconv.Itoa(i+1), Personas: []PersonaStatusV1{{Persona: "alpha", Installed: true, Session: SessionWorking, Turn: TurnActive, Activity: ActivityOther}}}); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < perShipIngress; j++ {
			if err := p.Enqueue(id, 1, ObservationEventV1{Persona: "alpha", Kind: ActivityEvent, Data: EventDataV1{Activity: ActivityOther, Label: "warmup"}}); err != nil {
				t.Fatal(err)
			}
		}
		if code(p.Enqueue(id, 1, ObservationEventV1{Persona: "alpha", Kind: ActivityEvent, Data: EventDataV1{Activity: ActivityOther, Label: "overflow"}})) != IngressFull {
			t.Fatalf("ship %s did not refuse a full ingress queue", id)
		}
		if _, err := p.Resync(id, 1); err != nil {
			t.Fatal(err)
		}
	}

	started := time.Now()
	deadline := started.Add(d)
	nextEvent, nextHeartbeat, nextReconnect := started, started, started.Add(reconnect)
	generations := [ships]uint64{1, 1}
	var after uint64
	for time.Now().Before(deadline) {
		now := time.Now()
		if !now.Before(nextEvent) {
			for i := 0; i < ships; i++ {
				id := "ship-" + strconv.Itoa(i+1)
				if err := p.Enqueue(id, generations[i], ObservationEventV1{Persona: "alpha", Kind: AgentMessage, Data: EventDataV1{Text: "synthetic bounded event"}}); err != nil {
					t.Fatalf("event enqueue: %v", err)
				}
				if err := p.Drain(id, generations[i]); err != nil {
					t.Fatalf("event drain: %v", err)
				}
			}
			nextEvent = nextEvent.Add(eventInterval)
		}
		if !now.Before(nextHeartbeat) {
			for i := 0; i < ships; i++ {
				if err := p.Heartbeat("ship-"+strconv.Itoa(i+1), generations[i]); err != nil {
					t.Fatalf("heartbeat: %v", err)
				}
			}
			nextHeartbeat = nextHeartbeat.Add(heartbeat)
		}
		if !now.Before(nextReconnect) {
			for i := 0; i < ships; i++ {
				id := "ship-" + strconv.Itoa(i+1)
				old := generations[i]
				if err := p.Disconnect(id, old, false); err != nil {
					t.Fatalf("disconnect: %v", err)
				}
				generations[i]++
				if err := p.Connect(id, generations[i]); err != nil {
					t.Fatalf("reconnect: %v", err)
				}
				if err := p.InstallSnapshot(id, generations[i], ShipStatusV1{ShipLabel: "synthetic-" + strconv.Itoa(i+1), Personas: []PersonaStatusV1{{Persona: "alpha", Installed: true, Session: SessionWorking, Turn: TurnActive, Activity: ActivityOther}}}); err != nil {
					t.Fatalf("resnapshot: %v", err)
				}
			}
			nextReconnect = nextReconnect.Add(reconnect)
		}

		result, err := p.Read(epoch, &after, pageSize)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if result.Gap != nil {
			if result.Snapshot == nil {
				t.Fatalf("gap without bounded snapshot: %#v", result.Gap)
			}
			after = result.Snapshot.SnapshotCursor
		} else if len(result.Events) > 0 {
			after = result.Events[len(result.Events)-1].Cursor
		} else if result.NextCursor > 0 {
			after = result.NextCursor - 1
		}
		runtime.ReadMemStats(&sample)
		if sample.Alloc > peak.Alloc {
			peak = sample
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := p.Read(epoch, func() *uint64 { x := uint64(0); return &x }(), pageSize); err != nil {
		t.Fatal(err)
	}
	if got := p.Snapshot(); len(got.Ships) != ships {
		t.Fatalf("ship isolation/count changed: %d", len(got.Ships))
	}
	runtime.GC()
	var final runtime.MemStats
	runtime.ReadMemStats(&final)
	finalFDs := countOpenFDs(t)
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > baseGoroutines+2 {
		t.Fatalf("goroutine leak: baseline=%d final=%d", baseGoroutines, finalGoroutines)
	}
	if finalFDs > baseFDs+2 {
		t.Fatalf("file descriptor leak: baseline=%d final=%d", baseFDs, finalFDs)
	}
	if final.Alloc > initial.Alloc+32<<20 || peak.Alloc > initial.Alloc+64<<20 {
		t.Fatalf("memory trend exceeded bounded ceiling: baseline=%d peak=%d final=%d", initial.Alloc, peak.Alloc, final.Alloc)
	}
	t.Logf("resources: duration=%s goroutines baseline=%d final=%d fds baseline=%d final=%d alloc_bytes baseline=%d peak=%d final=%d", d, baseGoroutines, finalGoroutines, baseFDs, finalFDs, initial.Alloc, peak.Alloc, final.Alloc)
}

func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("fd count unavailable: %v", err)
	}
	return len(entries)
}
