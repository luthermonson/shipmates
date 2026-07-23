package fleetidentity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommanderCapabilityIsSeparateScopedAndDurable(t *testing.T) {
	r, clock := newTestRegistry(t)
	_, ship := enroll(t, r)
	issued, err := r.IssueCommander("cmdr_0123456789abcdef", []string{ship.ShipID}, time.Hour)
	if err != nil || issued.Record.Capability != CommanderDelegateCapability {
		t.Fatalf("issue=%+v err=%v", issued, err)
	}
	p, err := r.AuthenticateCommander(issued.Record.CredentialID, issued.Secret, ship.FleetID, ship.ShipID)
	if err != nil || p.Capability != CommanderDelegateCapability || len(p.ShipIDs) != 1 {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	if _, err := r.AuthenticateOperator(issued.Record.CredentialID, issued.Secret, ship.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("commander inherited operator authority: %v", err)
	}
	rotated, err := r.RotateCommander(issued.Record.CredentialID, 1, time.Minute, time.Hour)
	if err != nil || rotated.Record.CredentialGeneration != 2 {
		t.Fatalf("rotate=%+v err=%v", rotated, err)
	}
	if err := r.CommitCommanderRotation(rotated.Record.CredentialID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AuthenticateCommander(issued.Record.CredentialID, issued.Secret, ship.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("old commander credential remained active: %v", err)
	}
	clock.Advance(time.Hour)
	if _, err := r.AuthenticateCommander(rotated.Record.CredentialID, rotated.Secret, ship.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("expired commander credential accepted: %v", err)
	}
}

func TestCommanderCapabilitySurvivesAuthorityRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	clock := &fakeClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	r, err := OpenRegistry(dir, "flt_0123456789abcdef", clock, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	_, ship := enroll(t, r)
	issued, err := r.IssueCommander("cmdr_0123456789abcdef", []string{ship.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(dir, "flt_0123456789abcdef", clock, &fakeRandom{next: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.AuthenticateCommander(issued.Record.CredentialID, issued.Secret, ship.FleetID, ship.ShipID); err != nil {
		t.Fatalf("commander did not survive restart: %v", err)
	}
}
