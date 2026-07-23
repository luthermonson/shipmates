//go:build linux

package installer

import (
	"context"
	"os"
	"testing"
)

func TestProductionSystemdManagerReadOnlyIntegration(t *testing.T) {
	if os.Getenv("SHIPMATES_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("set SHIPMATES_SYSTEMD_INTEGRATION=1 for the read-only host gate")
	}
	m := productionSystemdManager{}
	state, err := m.UnitState(context.Background(), QualifierUnitName)
	if err != nil {
		t.Fatal(err)
	}
	if state == QualifierStateUnknown {
		t.Fatal("systemd returned an unknown qualifier state")
	}
	pending, err := m.PendingJob(context.Background(), QualifierUnitName)
	if err != nil {
		t.Fatal(err)
	}
	if pending && state == QualifierStateMissing {
		t.Fatal("missing qualifier unit has a pending job")
	}
}
