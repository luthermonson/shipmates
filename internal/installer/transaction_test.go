package installer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallCommitsDurableTransactionRecord(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	tx, err := loadTransaction(osFS{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Phase != PhaseCommit || tx.ManifestHash == "" || len(tx.Inventory) < 2 {
		t.Fatalf("transaction=%+v", tx)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/shipmates/install.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptOrStaleTransactionStateFailsClosed(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "var/lib/shipmates/install.json")
	if err := os.WriteFile(journal, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errAdministratorDrift) {
		t.Fatalf("corrupt err=%v", err)
	}
	if err := os.WriteFile(journal, []byte(`{"schema":"shipmates.install.transaction.v1","transaction_id":"x","release":"shipmates-runtime-v1","manifest_sha256":"x","phase":"commit","inventory":[{},{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errAdministratorDrift) {
		t.Fatalf("identity err=%v", err)
	}
}

func TestRecoveryCompletesExactReleaseAfterPreActivationCrash(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(root, "var/lib/shipmates/install.json")
	var tx Transaction
	b, err := os.ReadFile(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &tx); err != nil {
		t.Fatal(err)
	}
	tx.Phase = PhaseVerification
	b, err = json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatalf("recovery err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "usr/libexec/shipmates/current")); err != nil || string(got) != ReleaseVersion+"\n" {
		t.Fatalf("current=%q err=%v", got, err)
	}
}

func TestUnknownTransactionPhaseFailsClosed(t *testing.T) {
	root := t.TempDir()
	j := filepath.Join(root, "var/lib/shipmates/install.json")
	if err := os.MkdirAll(filepath.Dir(j), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j, []byte(`{"schema":"shipmates.install.transaction.v1","transaction_id":"x","release":"shipmates-runtime-v1","manifest_sha256":"x","phase":"future","inventory":[{},{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errAdministratorDrift) {
		t.Fatalf("unknown phase err=%v", err)
	}
}

func TestLoadTransactionAcceptsCommittedPriorReleaseDescriptor(t *testing.T) {
	root := t.TempDir()
	m, err := ManifestFor()
	if err != nil {
		t.Fatal(err)
	}
	m.Release = "shipmates-runtime-v0"
	m.Sequence = 1
	d := descriptorFor(m)
	tx := Transaction{
		Schema: "shipmates.install.transaction.v2", ID: "00112233445566778899aabbccddeeff",
		Release: m.Release, ManifestHash: manifestHash(m), DescriptorHash: descriptorHash(d),
		Descriptor: d, Phase: PhaseCommit, Inventory: inventoryForManifest(m),
	}
	b, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	p := transactionPath(root)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTransaction(osFS{}, root)
	if err != nil || loaded.Release != m.Release || loaded.Phase != PhaseCommit {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}

	tx.Phase = PhaseStaging
	b, _ = json.Marshal(tx)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadTransaction(osFS{}, root)
	if err != nil || loaded.Phase != PhaseStaging {
		t.Fatalf("loader must preserve valid interrupted predecessor for caller rejection: loaded=%+v err=%v", loaded, err)
	}
}

func TestRecoveryRejectsJournalCandidatePointingAtCommittedRelease(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "var/lib/shipmates/install.json")
	b, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var tx Transaction
	if err := json.Unmarshal(b, &tx); err != nil {
		t.Fatal(err)
	}
	tx.Phase = PhasePreflight
	tx.Candidate = filepath.Join(root, "usr/libexec/shipmates/releases", ReleaseVersion)
	b, err = json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); !errors.Is(err, errAdministratorDrift) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr/libexec/shipmates/releases", ReleaseVersion)); err != nil {
		t.Fatalf("committed release changed: %v", err)
	}
}

func TestUninstallRejectsJournalObjectTargetSubstitution(t *testing.T) {
	root := t.TempDir()
	if _, err := Install(Options{Root: root, EffectiveUID: 0}); err != nil {
		t.Fatal(err)
	}
	installTx, err := loadTransaction(osFS{}, root)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ManifestFor()
	if err != nil {
		t.Fatal(err)
	}
	utx, err := newUninstallTransaction(root, installTx, m)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "unrelated")
	if err := os.WriteFile(victim, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	utx.Objects[0].Path = victim
	b, err := json.Marshal(utx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var/lib/shipmates/uninstall.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(Options{Root: root, EffectiveUID: 0, Fence: inactiveFence{}}); !errors.Is(err, errAdministratorDrift) {
		t.Fatalf("err=%v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "unrelated" {
		t.Fatalf("victim=%q err=%v", got, err)
	}
}
