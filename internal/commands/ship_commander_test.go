package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
)

func TestCommanderCompositionDisabledIsInert(t *testing.T) {
	root := t.TempDir()
	cb, err := makeCommanderStep(root, project.Config{}, fleetidentity.ShipState{})
	if err != nil || cb != nil {
		t.Fatalf("disabled callback present=%t err=%v", cb != nil, err)
	}
	if _, err := os.Stat(filepath.Join(root, project.Dir)); !os.IsNotExist(err) {
		t.Fatalf("disabled composition created project state: %v", err)
	}
}

func TestCommanderCompositionRejectsCrossFleetBeforeMailbox(t *testing.T) {
	root := t.TempDir()
	cfg := project.Config{Recovery: recovery.Config{AutoCaptain: true, CommanderDelegation: recovery.CommanderDelegationConfig{Enabled: true, FleetID: "flt_0123456789abcdef", ProtocolVersion: 1, MaxOfferSeconds: 60}}}
	cb, err := makeCommanderStep(root, cfg, fleetidentity.ShipState{FleetID: "flt_ffffffffffffffff", ShipID: "shp_0123456789abcdef"})
	if err == nil || cb != nil {
		t.Fatalf("cross-fleet callback present=%t err=%v", cb != nil, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, project.Dir, "delegations")); !os.IsNotExist(statErr) {
		t.Fatalf("cross-fleet validation created delegation state: %v", statErr)
	}
}

func TestCommanderCompositionRejectsMissingTrustedState(t *testing.T) {
	root := t.TempDir()
	// This exercises the callback's first connection boundary without a Fleet
	// transport: no plan/state means no mailbox or M2 assessment is opened.
	local := delegationTestConfig()
	cb, err := makeCommanderStep(root, local, fleetidentity.ShipState{FleetID: local.Recovery.CommanderDelegation.FleetID, ShipID: "shp_0123456789abcdef"})
	if err != nil || cb == nil {
		t.Fatalf("callback present=%t err=%v", cb != nil, err)
	}
	step, stepErr := cb(context.Background(), nil, 1)
	if step != nil || stepErr == nil {
		t.Fatal("missing trusted state accepted")
	}
	if _, statErr := os.Stat(filepath.Join(root, project.Dir, "delegationmailbox")); !os.IsNotExist(statErr) {
		t.Fatalf("missing state created mailbox: %v", statErr)
	}
}

func delegationTestConfig() project.Config {
	return project.Config{Recovery: recovery.Config{AutoCaptain: true, CommanderDelegation: recovery.CommanderDelegationConfig{Enabled: true, FleetID: "flt_0123456789abcdef", ProtocolVersion: 1, MaxOfferSeconds: 60, PermittedIssuers: []recovery.CommanderIssuerConfig{{KeyID: "cmdkey_0123456789ab", PublicKey: "ERERERERERERERERERERERERERERERERERERERERERE"}}}}}
}
