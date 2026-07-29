//go:build unix

package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// applyRoutingFixture builds an n-persona project ready for one routing
// transaction and returns the old and new artifact bytes plus the serialized
// manifests.
type applyRoutingFixture struct {
	root                     string
	names                    []string
	old, next                [][]byte
	oldManifest, newManifest []byte
	artifacts                []RoutingArtifact
}

func newApplyRoutingFixture(t *testing.T, names ...string) *applyRoutingFixture {
	t.Helper()
	f := &applyRoutingFixture{root: t.TempDir(), names: names}
	oldFiles, newFiles := map[string]string{}, map[string]string{}
	for _, name := range names {
		old := []byte(fmt.Sprintf("name = %q\ndeveloper_instructions = \"old %s\"\n", name, name))
		next := []byte(fmt.Sprintf("name = %q\ndeveloper_instructions = \"new %s\"\n", name, name))
		f.old, f.next = append(f.old, old), append(f.next, next)
		writeM10Agent(t, f.root, name, old)
		rel := filepath.ToSlash(CodexAgentPath(name))
		oldFiles[rel], newFiles[rel] = SHA(old), SHA(next)
	}
	f.oldManifest, _ = SerializeManifestV2(&Manifest{Version: ManifestVersion, Files: oldFiles})
	f.newManifest, _ = SerializeManifestV2(&Manifest{Version: ManifestVersion, Files: newFiles})
	if err := os.MkdirAll(filepath.Join(f.root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, ManifestPath()), f.oldManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		item, err := CanonicalPersonaAt(f.root, name)
		if err != nil {
			t.Fatal(err)
		}
		f.artifacts = append(f.artifacts, RoutingArtifact{Name: name, OldHash: SHA(f.old[i]), OldMode: 0o600, OldDevice: item.Device, OldInode: item.Inode, Output: f.next[i]})
	}
	return f
}

func (f *applyRoutingFixture) assertAllOriginal(t *testing.T) {
	t.Helper()
	for i, name := range f.names {
		got, err := os.ReadFile(filepath.Join(f.root, CodexAgentPath(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != string(f.old[i]) {
			t.Fatalf("%s = %q, want its original %q", name, got, f.old[i])
		}
	}
	manifest, err := os.ReadFile(filepath.Join(f.root, ManifestPath()))
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != string(f.oldManifest) {
		t.Fatal("manifest diverged from the uncommitted filesystem state")
	}
}

// A destination that changes identity partway through the commit loop must roll
// the whole transaction back. The regression this guards is the transaction
// directory being deleted at that point: entries already exchanged are live at
// their destinations and the only copies of their originals are the backups
// inside that directory, so deleting it loses them permanently and leaves the
// manifest describing content that is no longer on disk.
func TestApplyRoutingTransactionRollsBackCommittedEntriesOnMidLoopDestinationChange(t *testing.T) {
	if !RoutingTransactionsSupported() {
		t.Skip("atomic routing exchange unsupported")
	}
	f := newApplyRoutingFixture(t, "alpha", "beta")

	// Entry 0 commits normally. When entry 1 reaches its exchange, replace its
	// destination with byte-identical content at a fresh inode — exactly what a
	// concurrent writer does — so identity verification refuses the commit.
	original := routingRenameExchange
	t.Cleanup(func() { routingRenameExchange = original })
	perturbed := false
	routingRenameExchange = func(oldFD int, oldName string, newFD int, newName string) error {
		if oldName == "001.toml" && !perturbed {
			perturbed = true
			dst := filepath.Join(f.root, CodexAgentPath("beta"))
			tmp := dst + ".concurrent"
			if err := os.WriteFile(tmp, f.old[1], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, dst); err != nil {
				t.Fatal(err)
			}
		}
		return original(oldFD, oldName, newFD, newName)
	}

	err := ApplyRoutingTransaction(f.root, f.artifacts, SHA(f.oldManifest), f.newManifest)
	if err == nil {
		t.Fatal("mid-loop destination change was committed")
	}
	if !perturbed {
		t.Fatal("the exchange for entry 1 never ran; the test did not exercise the bug")
	}
	if !strings.Contains(err.Error(), "changed at routing commit") {
		t.Fatalf("error = %v, want the destination-changed cause", err)
	}
	// Entry 0 is the whole point: it had already been exchanged.
	f.assertAllOriginal(t)
	if _, statErr := os.Stat(routingTxnDir(f.root)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("transaction retained after a complete rollback: %v", statErr)
	}
}

// A single-entry transaction whose exchange-back fails leaves the new output at
// the destination and the displaced original in stage. That state is only
// recoverable through the journal, so the transaction directory must survive.
func TestApplyRoutingTransactionKeepsJournalWhenExchangeBackFails(t *testing.T) {
	if !RoutingTransactionsSupported() {
		t.Skip("atomic routing exchange unsupported")
	}
	f := newApplyRoutingFixture(t, "alpha")

	original := routingRenameExchange
	t.Cleanup(func() { routingRenameExchange = original })
	calls := 0
	routingRenameExchange = func(oldFD int, oldName string, newFD int, newName string) error {
		calls++
		switch calls {
		case 1:
			// Commit the exchange, then make the destination fail verification.
			if err := original(oldFD, oldName, newFD, newName); err != nil {
				return err
			}
			return nil
		case 2:
			// The exchange-back fails: the new output stays live and the
			// original is stranded in stage.
			return errors.New("simulated exchange-back failure")
		}
		return original(oldFD, oldName, newFD, newName)
	}
	// Force verification to fail by changing the expected inode.
	f.artifacts[0].OldInode++

	err := ApplyRoutingTransaction(f.root, f.artifacts, SHA(f.oldManifest), f.newManifest)
	if err == nil {
		t.Fatal("failed exchange-back reported success")
	}
	if calls < 2 {
		t.Fatalf("exchange-back never attempted (calls = %d)", calls)
	}
	// Either recovery restored the original outright, or it could not and the
	// journal plus the stranded original must still be on disk. What must never
	// happen is the original being unrecoverable.
	got, readErr := os.ReadFile(filepath.Join(f.root, CodexAgentPath("alpha")))
	if readErr == nil && string(got) == string(f.old[0]) {
		return
	}
	if _, statErr := os.Stat(routingJournalPath(f.root)); statErr != nil {
		t.Fatalf("original not restored (%q, %v) and the journal was deleted: %v", got, readErr, statErr)
	}
	stage, stageErr := os.ReadFile(filepath.Join(routingTxnDir(f.root), "stage", "000.toml"))
	backup, backupErr := os.ReadFile(filepath.Join(routingTxnDir(f.root), "backup", "000.toml"))
	if (stageErr != nil || string(stage) != string(f.old[0])) && (backupErr != nil || string(backup) != string(f.old[0])) {
		t.Fatalf("original bytes exist nowhere: destination %q, stage %q/%v, backup %q/%v", got, stage, stageErr, backup, backupErr)
	}
}

// The journal is written last, so a crash before it is durable leaves a
// transaction directory that recovery cannot interpret. Recovery must clear it,
// or the next transaction fails forever at its mkdir.
func TestRecoverRoutingTransactionClearsPreJournalDebris(t *testing.T) {
	if !RoutingTransactionsSupported() {
		t.Skip("atomic routing exchange unsupported")
	}
	f := newApplyRoutingFixture(t, "alpha")
	// Reproduce the crash window: directories and staged output exist, no journal.
	if err := os.MkdirAll(filepath.Join(routingTxnDir(f.root), "stage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(routingTxnDir(f.root), "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(routingTxnDir(f.root), "stage", "000.toml"), f.next[0], 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RecoverRoutingTransaction(f.root); err != nil {
		t.Fatalf("recover pre-journal debris: %v", err)
	}
	if _, err := os.Stat(routingTxnDir(f.root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("debris retained: %v", err)
	}
	// And the next transaction must now be able to run at all.
	if err := ApplyRoutingTransaction(f.root, f.artifacts, SHA(f.oldManifest), f.newManifest); err != nil {
		t.Fatalf("apply after debris cleanup: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(f.root, CodexAgentPath("alpha")))
	if err != nil || string(got) != string(f.next[0]) {
		t.Fatalf("alpha = %q, %v", got, err)
	}
}
