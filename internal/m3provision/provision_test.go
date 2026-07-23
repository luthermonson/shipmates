package m3provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/fleetconfig"
)

func TestValidateInvocationIsFixedAndSecretSafe(t *testing.T) {
	good := []string{"--profile", Profile}
	if err := ValidateInvocation(good, []string{"PATH=/usr/bin"}, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		env  []string
		uid  int
	}{
		{good, nil, 1000},
		{[]string{"--profile", "other"}, nil, 0},
		{[]string{"--profile", Profile, "extra"}, nil, 0},
		{good, []string{"SHIPMATES_" + "SECRET=not-used"}, 0},
	} {
		if err := ValidateInvocation(tc.args, tc.env, tc.uid); err == nil {
			t.Fatalf("unsafe invocation accepted: %+v", tc)
		}
	}
}

func TestDelegatedProbePlanCannotStartQualification(t *testing.T) {
	plan, err := BuildDelegatedProbePlan("/sys/fs/cgroup/shipmates")
	if err != nil || plan.StartsQualification || !plan.RequiresCleanup || len(plan.Controls) == 0 {
		t.Fatalf("unexpected probe plan: %+v err=%v", plan, err)
	}
	if _, err := BuildDelegatedProbePlan("/"); err == nil {
		t.Fatal("accepted cgroup root")
	}
}

func TestProvisionAtCreatesProtectedLocalProfileWithoutStartingAnything(t *testing.T) {
	root, err := os.MkdirTemp(".", ".m3-provision-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	base := filepath.Join(root, "etc", "shipmates", "m3-qualifier")
	if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "launcher")
	if err := os.WriteFile(helper, []byte("\x7fELF fixture identity"), 0700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "systemd", "shipmates.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	result, err := ProvisionAt(Layout{Base: base, Helper: helper, Unit: unit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile != Profile || result.CaptainCmd == "" || strings.Contains(result.CaptainCmd, "cds_") || strings.Contains(result.CaptainCmd, "secret") {
		t.Fatalf("unsafe result: %+v", result)
	}
	if _, err := fleetconfig.LoadRuntimeProfileAt(filepath.Join(base, "fleet.json"), filepath.Join(base, "trust"), filepath.Join(base, "credentials", "ship.json"), filepath.Join(base, "credentials", "commander.json"), true); err != nil {
		t.Fatalf("profile consumption failed: %v", err)
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(base, "fleet.json"), 0644}, {filepath.Join(base, "credentials", "ship.json"), 0600}, {filepath.Join(base, "credentials", "commander.json"), 0600}, {filepath.Join(base, "trust", "fleet-ca.pem"), 0644}, {filepath.Join(base, "secrets", "ship-proof.json"), 0600}, {filepath.Join(base, "helper-manifest.json"), 0600}, {unit, 0600}} {
		st, err := os.Stat(item.path)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != item.mode {
			t.Fatalf("protected artifact %s: %v mode=%v", item.path, err, st.Mode())
		}
	}
	unitBytes, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	unitText := string(unitBytes)
	if err := ValidateCredentialUnit(unitText, false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredentialUnit("LoadCredentialEncrypted=ship.json:key\nLoadCredentialEncrypted=commander.json:key\n", true); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"User=shipmates", "Group=shipmates", "Delegate=yes", "ProtectSystem=strict", "PrivateTmp=yes", "TimeoutStartSec=10s", "TimeoutStopSec=10s", "MemoryMax=512M", "TasksMax=256", "NoNewPrivileges=yes", "ReadOnlyPaths=", "ReadWritePaths="} {
		if !strings.Contains(unitText, required) {
			t.Fatalf("unit missing %q", required)
		}
	}
	for _, forbidden := range []string{"Environment" + "=", "ExecStart=/bin/sh", "PATH=", "system" + "ctl"} {
		if strings.Contains(unitText, forbidden) {
			t.Fatalf("unit contains forbidden %q", forbidden)
		}
	}
	stagedHelper, err := os.ReadFile(filepath.Join(base, "helper", "shipmates-cgroup-launcher"))
	if err != nil || len(stagedHelper) < 4 || string(stagedHelper[:4]) != "\x7fELF" {
		t.Fatalf("staged helper invalid: %v", err)
	}
	if _, err := ProvisionAt(Layout{Base: base, Helper: helper, Unit: unit}); err == nil {
		t.Fatal("replaced an existing profile")
	}
}
