//go:build unix

package fleetobserver

import (
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

// TestEmptyObserverScopeFailsClosed is the regression test for the read path
// that treated an empty observer scope as authorization for every ship. All
// three deciders short-circuited to "allow" on len(scope) == 0.
func TestEmptyObserverScopeFailsClosed(t *testing.T) {
	for name, scope := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if allowed(scope, "shp_000000000000001") {
				t.Fatal("an empty scope authorized a ship read")
			}
			snapshot := fleetobserve.FleetSnapshotV1{
				FleetID: "fleet-000000000001",
				Ships: []fleetobserve.ShipStatusV1{
					{ShipID: "shp_000000000000001"},
					{ShipID: "shp_000000000000002"},
				},
			}
			if got := filterSnapshot(snapshot, scope); len(got.Ships) != 0 {
				t.Fatalf("an empty scope read %d ships from the snapshot", len(got.Ships))
			}
			now := time.Unix(1_000_000, 0)
			inner := fleetobserve.FleetSnapshotV1{
				FleetID:     "fleet-000000000001",
				GeneratedAt: now,
				Ships:       []fleetobserve.ShipStatusV1{{ShipID: "shp_000000000000001"}},
			}
			read := fleetobserve.ReadResult{
				Snapshot: &inner,
				Events: []fleetobserve.FleetEventV1{
					{Cursor: 1, ShipID: "shp_000000000000001"},
					{Cursor: 2, ShipID: "shp_000000000000002"},
				},
			}
			filterRead(&read, scope)
			if len(read.Events) != 0 {
				t.Fatalf("an empty scope read %d events", len(read.Events))
			}
			if read.Snapshot == nil || len(read.Snapshot.Ships) != 0 {
				t.Fatalf("an empty scope read the embedded snapshot: %+v", read.Snapshot)
			}
		})
	}
}

// TestExactObserverScopeStillReads keeps the fail-closed change from becoming a
// blanket refusal for a correctly scoped credential.
func TestExactObserverScopeStillReads(t *testing.T) {
	scope := []string{"shp_000000000000002"}
	if !allowed(scope, "shp_000000000000002") || allowed(scope, "shp_000000000000001") {
		t.Fatal("exact scope decision is wrong")
	}
	snapshot := fleetobserve.FleetSnapshotV1{Ships: []fleetobserve.ShipStatusV1{
		{ShipID: "shp_000000000000001"},
		{ShipID: "shp_000000000000002"},
	}}
	got := filterSnapshot(snapshot, scope)
	if len(got.Ships) != 1 || got.Ships[0].ShipID != "shp_000000000000002" {
		t.Fatalf("scoped snapshot=%+v", got.Ships)
	}
	read := fleetobserve.ReadResult{Events: []fleetobserve.FleetEventV1{
		{Cursor: 1, ShipID: "shp_000000000000001"},
		{Cursor: 2, ShipID: "shp_000000000000002"},
	}}
	filterRead(&read, scope)
	if len(read.Events) != 1 || read.Events[0].ShipID != "shp_000000000000002" {
		t.Fatalf("scoped events=%+v", read.Events)
	}
}
