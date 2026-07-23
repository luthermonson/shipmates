package installer

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type inactiveFence struct{ err error }

func (f inactiveFence) CheckInactive() error { return f.err }

func TestManifestIsClosedAndPinned(t *testing.T) {
	m, err := ManifestFor()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
	if len(m.Assets) != 3 {
		t.Fatalf("assets=%d", len(m.Assets))
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "credential") && strings.Contains(string(b), "token") {
		t.Fatal("manifest leaks secret-shaped data")
	}
	for _, a := range m.Assets {
		if !fixedDestination(a.Destination) || a.Size == 0 || len(a.Digest) != 64 {
			t.Fatalf("bad asset: %+v", a)
		}
		if strings.HasPrefix(a.Source, "payloads/") {
			b, err := PayloadFor(a.Role)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(b)) != a.Size {
				t.Fatal("payload size mismatch")
			}
			if _, err := elf.NewFile(bytes.NewReader(b)); err != nil {
				t.Fatal(err)
			}
			if _, err := executableBuildID(b); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestTextAndWrongTargetPayloadsAreRejected(t *testing.T) {
	if _, err := executableBuildID([]byte("#!/bin/sh\nexit 0\n")); err == nil {
		t.Fatal("script accepted")
	}
	if _, err := executableBuildID([]byte("not an elf")); err == nil {
		t.Fatal("text accepted")
	}
}

func TestEmbeddedPayloadMatchesGeneratedOutput(t *testing.T) {
	for _, role := range []string{"qualifier-runner", "cgroup-launcher"} {
		got, err := PayloadFor(role)
		if err != nil {
			t.Fatal(err)
		}
		name := "shipmates-m3-qualifier-run"
		if role == "cgroup-launcher" {
			name = "shipmates-cgroup-launcher"
		}
		want, err := os.ReadFile("payloads/" + payloadArch + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("embedded %s differs from generated payload", role)
		}
	}
}

func TestValidateManifestRejectsRoleSourceAndModeSubstitution(t *testing.T) {
	m, err := ManifestFor()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Asset){
		"role":   func(a *Asset) { a.Role = "other" },
		"source": func(a *Asset) { a.Source = "assets/other" },
		"mode":   func(a *Asset) { a.Mode = 0777 },
		"digest": func(a *Asset) { a.Digest = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := m
			candidate.Assets = append([]Asset(nil), m.Assets...)
			mutate(&candidate.Assets[0])
			if err := ValidateManifest(candidate); err == nil {
				t.Fatal("substituted manifest accepted")
			}
		})
	}
}

func TestInstallDryRunIsImmutable(t *testing.T) {
	root := t.TempDir()
	r, err := Install(Options{Root: root, EffectiveUID: 0, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.DryRun || r.Action != "install" {
		t.Fatalf("report=%+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "usr")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run mutated root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created lock/state: %v", err)
	}
}

func TestInstallIdempotentAndRefusesDrift(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "usr/libexec/shipmates/shipmates-m3-qualifier-run")
	if err := os.WriteFile(path, []byte("drift"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errDrift) {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallRejectsNonRootAndUninstallRetainsJournal(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 1000}); !errors.Is(err, errRootRequired) {
		t.Fatalf("err=%v", err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(Options{Root: root, EffectiveUID: 0, Fence: inactiveFence{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/shipmates/install.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/shipmates/credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential retention changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/libexec/shipmates/current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/systemd/system/shipmates-m3-qualifier.service")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit retained: %v", err)
	}
}

func TestUninstallRequiresKnownInactiveState(t *testing.T) {
	root := t.TempDir()
	if _, err := Uninstall(Options{Root: root, EffectiveUID: 0}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown fence err=%v", err)
	}
	if _, err := Uninstall(Options{Root: root, EffectiveUID: 0, Fence: inactiveFence{err: errors.New("active")}}); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("active fence err=%v", err)
	}
}

func TestUninstallReportsDriftWithoutClaimingSuccess(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "usr/libexec/shipmates/shipmates-m3-qualifier-run")
	if err := os.WriteFile(path, []byte("drift"), 0755); err != nil {
		t.Fatal(err)
	}
	report, err := Uninstall(Options{Root: root, EffectiveUID: 0, Fence: inactiveFence{}})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("drift err=%v report=%+v", err, report)
	}
	found := false
	for _, retained := range report.Retained {
		if strings.HasSuffix(retained, "/usr/libexec/shipmates/shipmates-m3-qualifier-run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drift not reported: %+v", report)
	}
}

func TestInstallRefusesSymlinkedDestinationParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "usr")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "libexec")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped write: %v", err)
	}
}

func TestInstallRefusesSymlinkedStateParentBeforeWrite(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "var")); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err == nil || !strings.Contains(err.Error(), "state_unsafe") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "lib")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped state write: %v", err)
	}
}

func TestInstallRefusesHardLinkedManagedAssetOnRepeat(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(root, "usr/libexec/shipmates/shipmates-m3-qualifier-run")
	copy := filepath.Join(root, "linked-copy")
	if err := os.Link(managed, copy); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errDrift) {
		t.Fatalf("err=%v", err)
	}
}
