//go:build linux

package m3runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/m3provision"
)

func TestRuntimeCredentialsConsumeOnlyExactDeliveredRecords(t *testing.T) {
	root, err := os.MkdirTemp(".", ".m3-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, "profile")), 0700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, []byte("\x7fELF fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "unit")
	result, err := m3provision.ProvisionAt(m3provision.Layout{Base: filepath.Join(root, "profile"), Helper: helper, Unit: unit})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFromDirectory(filepath.Join(root, "profile", "credentials"), filepath.Join(root, "profile", "fleet.json"), filepath.Join(root, "profile", "trust"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Profile.Config.FleetID != result.FleetID || loaded.Profile.Ship.ShipID != result.ShipID || loaded.Profile.Commander.Capability != "fleet.commander.delegate.v1" {
		t.Fatalf("wrong runtime binding: %+v", loaded.Profile)
	}
	if err := loaded.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCredentialsRejectFakeDirectoriesExtraFilesAndUnsafeModes(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name  string
		setup func(string) error
	}{
		{"missing", func(string) error { return nil }},
		{"extra", func(dir string) error { return os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0600) }},
		{"mode", func(dir string) error { return os.Chmod(filepath.Join(dir, ShipCredentialName), 0660) }},
	} {
		dir := filepath.Join(root, tc.name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if tc.name != "missing" {
			if err := os.WriteFile(filepath.Join(dir, ShipCredentialName), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, CommanderCredentialName), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := tc.setup(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFromDirectory(dir, filepath.Join(root, "fleet.json"), filepath.Join(root, "trust")); err == nil {
			t.Fatalf("accepted unsafe credential directory %s", tc.name)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "missing"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromDirectory(link, filepath.Join(root, "fleet.json"), filepath.Join(root, "trust")); err == nil {
		t.Fatal("accepted symlink credential directory")
	}
}

func TestRunnerInvocationRefusesOverrides(t *testing.T) {
	if err := ValidateInvocation(nil, []string{"CREDENTIALS_DIRECTORY=/run/credentials/x"}); err != nil {
		t.Fatal(err)
	}
	for _, env := range [][]string{{}, {"SHIPMATES_CGROUP_ROOT=/tmp/x", "CREDENTIALS_DIRECTORY=/run/x"}, {"M3_SECRET=x", "CREDENTIALS_DIRECTORY=/run/x"}} {
		if err := ValidateInvocation(nil, env); err == nil {
			t.Fatalf("accepted environment %+v", env)
		}
	}
	if err := ValidateInvocation([]string{"--config", "x"}, []string{"CREDENTIALS_DIRECTORY=/run/x"}); err == nil {
		t.Fatal("accepted runner args")
	}
	if err := ValidateInvocation(nil, []string{"CREDENTIALS_DIRECTORY=/tmp/credentials/x"}); err == nil {
		t.Fatal("accepted arbitrary credential directory")
	}
	if err := ValidateInvocation(nil, []string{"CREDENTIALS_DIRECTORY=/run/credentials/x", "CREDENTIALS_DIRECTORY=/run/credentials/y"}); err == nil {
		t.Fatal("accepted duplicate credential directory")
	}
}
