//go:build unix

package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type m10RoutingFixture struct {
	root, rel                           string
	old, next, oldManifest, newManifest []byte
	journal                             routingJournal
}

func newM10RoutingFixture(t *testing.T) *m10RoutingFixture {
	t.Helper()
	root := t.TempDir()
	old := []byte("name = \"tester\"\ndeveloper_instructions = \"old\"\n")
	next := []byte("name = \"tester\"\ndeveloper_instructions = \"new\"\n")
	rel := filepath.ToSlash(CodexAgentPath("tester"))
	writeM10Agent(t, root, "tester", old)
	om := &Manifest{Version: ManifestVersion, Files: map[string]string{rel: SHA(old)}}
	nm := &Manifest{Version: ManifestVersion, Files: map[string]string{rel: SHA(next)}}
	oldManifest, _ := SerializeManifestV2(om)
	newManifest, _ := SerializeManifestV2(nm)
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestPath()), oldManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	j := routingJournal{Version: 1, OldManifestHash: SHA(oldManifest), NewManifestHash: SHA(newManifest), Entries: []routingJournalEntry{{Name: "tester", Path: rel, Stage: "stage/000.toml", Backup: "backup/000.toml", OldHash: SHA(old), NewHash: SHA(next), OldMode: 0o600}}}
	return &m10RoutingFixture{root: root, rel: rel, old: old, next: next, oldManifest: oldManifest, newManifest: newManifest, journal: j}
}

func (f *m10RoutingFixture) writeTxn(t *testing.T, stage, backup []byte) {
	t.Helper()
	txn := routingTxnDir(f.root)
	if err := os.MkdirAll(filepath.Join(txn, "stage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(txn, "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if stage != nil {
		if err := os.WriteFile(filepath.Join(txn, "stage/000.toml"), stage, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if backup != nil {
		if err := os.WriteFile(filepath.Join(txn, "backup/000.toml"), backup, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(f.journal)
	if err := os.WriteFile(routingJournalPath(f.root), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestM10RoutingRecoveryEveryDurableCrashPointAndIdempotence(t *testing.T) {
	points := []struct {
		name                                 string
		destination, stage, backup, manifest []byte
		want                                 []byte
	}{
		{"journal durable", nil, []byte("stage"), nil, nil, nil}, // sentinel values replaced below
		{"exchange before backup", []byte("new"), []byte("old stage"), nil, nil, nil},
		{"old moved to backup", nil, nil, []byte("backup"), nil, nil},
		{"new installed before manifest", []byte("new"), nil, []byte("backup"), nil, nil},
		{"manifest committed", []byte("new"), nil, []byte("backup"), []byte("new manifest"), []byte("new")},
	}
	for _, tt := range points {
		t.Run(tt.name, func(t *testing.T) {
			f := newM10RoutingFixture(t)
			stage, backup := tt.stage, tt.backup
			if stage != nil {
				stage = f.next
				if tt.name == "exchange before backup" {
					stage = f.old
				}
			}
			if backup != nil {
				backup = f.old
			}
			f.writeTxn(t, stage, backup)
			dst := filepath.Join(f.root, f.rel)
			if tt.destination == nil && tt.name == "old moved to backup" {
				if err := os.Remove(dst); err != nil {
					t.Fatal(err)
				}
			}
			if tt.destination != nil {
				if err := os.WriteFile(dst, f.next, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.manifest != nil {
				if err := os.WriteFile(filepath.Join(f.root, ManifestPath()), f.newManifest, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverRoutingTransaction(f.root); err != nil {
				t.Fatal(err)
			}
			want := f.old
			if tt.want != nil {
				want = f.next
			}
			got, err := os.ReadFile(dst)
			if err != nil || string(got) != string(want) {
				t.Fatalf("destination = %q, %v", got, err)
			}
			if err := RecoverRoutingTransaction(f.root); err != nil {
				t.Fatalf("idempotent recovery: %v", err)
			}
			if _, err := os.Stat(routingTxnDir(f.root)); !os.IsNotExist(err) {
				t.Fatalf("transaction retained: %v", err)
			}
		})
	}
}

func TestM10RoutingRecoveryTwoPersonasAfterFirstCommit(t *testing.T) {
	for _, firstState := range []string{"exchange_before_backup", "backup_durable"} {
		for _, manifestState := range []string{"old", "new"} {
			t.Run(firstState+"_"+manifestState, func(t *testing.T) {
				root := t.TempDir()
				names := []string{"alpha", "beta"}
				old := [][]byte{
					[]byte("name = \"alpha\"\ndeveloper_instructions = \"old alpha\"\n"),
					[]byte("name = \"beta\"\ndeveloper_instructions = \"old beta\"\n"),
				}
				next := [][]byte{
					[]byte("name = \"alpha\"\ndeveloper_instructions = \"new alpha\"\n"),
					[]byte("name = \"beta\"\ndeveloper_instructions = \"new beta\"\n"),
				}
				oldFiles, newFiles := map[string]string{}, map[string]string{}
				j := routingJournal{Version: 1}
				for i, name := range names {
					rel := filepath.ToSlash(CodexAgentPath(name))
					writeM10Agent(t, root, name, old[i])
					oldFiles[rel], newFiles[rel] = SHA(old[i]), SHA(next[i])
					j.Entries = append(j.Entries, routingJournalEntry{Name: name, Path: rel, Stage: filepath.ToSlash(filepath.Join("stage", fmt.Sprintf("%03d.toml", i))), Backup: filepath.ToSlash(filepath.Join("backup", fmt.Sprintf("%03d.toml", i))), OldHash: SHA(old[i]), NewHash: SHA(next[i]), OldMode: 0o600})
				}
				oldManifest, _ := SerializeManifestV2(&Manifest{Version: ManifestVersion, Files: oldFiles})
				newManifest, _ := SerializeManifestV2(&Manifest{Version: ManifestVersion, Files: newFiles})
				j.OldManifestHash, j.NewManifestHash = SHA(oldManifest), SHA(newManifest)
				if err := os.MkdirAll(filepath.Join(routingTxnDir(root), "stage"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(routingTxnDir(root), "backup"), 0o700); err != nil {
					t.Fatal(err)
				}
				// Persona zero crossed the atomic exchange. Persona one has not.
				if err := os.WriteFile(filepath.Join(root, j.Entries[0].Path), next[0], 0o600); err != nil {
					t.Fatal(err)
				}
				if firstState == "exchange_before_backup" {
					if err := os.WriteFile(filepath.Join(routingTxnDir(root), j.Entries[0].Stage), old[0], 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(routingTxnDir(root), j.Entries[0].Backup), old[0], 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(routingTxnDir(root), j.Entries[1].Stage), next[1], 0o600); err != nil {
					t.Fatal(err)
				}
				manifest := oldManifest
				if manifestState == "new" {
					manifest = newManifest
				}
				if err := os.MkdirAll(filepath.Join(root, Dir), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ManifestPath()), manifest, 0o600); err != nil {
					t.Fatal(err)
				}
				journalRaw, _ := json.Marshal(j)
				if err := os.WriteFile(routingJournalPath(root), journalRaw, 0o600); err != nil {
					t.Fatal(err)
				}

				if err := RecoverRoutingTransaction(root); err != nil {
					t.Fatal(err)
				}
				want := old
				wantManifest := oldManifest
				if manifestState == "new" {
					want, wantManifest = next, newManifest
				}
				for i, name := range names {
					got, err := os.ReadFile(filepath.Join(root, CodexAgentPath(name)))
					if err != nil || string(got) != string(want[i]) {
						t.Fatalf("%s = %q, %v", name, got, err)
					}
				}
				gotManifest, err := os.ReadFile(filepath.Join(root, ManifestPath()))
				if err != nil || string(gotManifest) != string(wantManifest) {
					t.Fatalf("manifest inconsistent: %v", err)
				}
				if err := RecoverRoutingTransaction(root); err != nil {
					t.Fatalf("idempotent recovery: %v", err)
				}
				if _, err := os.Stat(routingTxnDir(root)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("transaction retained: %v", err)
				}
			})
		}
	}
}

func TestM10RoutingRefusesInvalidJournalTraversalAndUnexpectedDestination(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		f := newM10RoutingFixture(t)
		if err := os.MkdirAll(routingTxnDir(f.root), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(routingJournalPath(f.root), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RecoverRoutingTransaction(f.root); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("traversal", func(t *testing.T) {
		f := newM10RoutingFixture(t)
		f.journal.Entries[0].Stage = "../escape"
		f.writeTxn(t, nil, nil)
		if err := RecoverRoutingTransaction(f.root); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unexpected destination", func(t *testing.T) {
		f := newM10RoutingFixture(t)
		f.writeTxn(t, nil, f.old)
		if err := os.WriteFile(filepath.Join(f.root, f.rel), []byte("external edit"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := RecoverRoutingTransaction(f.root); err == nil || !strings.Contains(err.Error(), "unexpected destination") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestM10RoutingApplyManifestConsistencyModesAndConcurrentEditRefusal(t *testing.T) {
	f := newM10RoutingFixture(t)
	before, err := CanonicalPersonaAt(f.root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	a := RoutingArtifact{Name: "tester", OldHash: SHA(f.old), OldMode: 0o600, OldDevice: before.Device, OldInode: before.Inode, Output: f.next}
	if err := ApplyRoutingTransaction(f.root, []RoutingArtifact{a}, SHA(f.oldManifest), f.newManifest); err != nil {
		t.Fatal(err)
	}
	item, err := CanonicalPersonaAt(f.root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := LoadManifestSnapshot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if item.Mode.Perm() != 0o600 || SHA(item.Raw) != m.Files[f.rel] {
		t.Fatalf("mode/hash inconsistent: %o %#v", item.Mode.Perm(), m.Files)
	}

	g := newM10RoutingFixture(t)
	if err := os.WriteFile(filepath.Join(g.root, g.rel), []byte("developer_instructions = \"concurrent\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gBefore, err := CanonicalPersonaAt(g.root, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyRoutingTransaction(g.root, []RoutingArtifact{{Name: "tester", OldHash: SHA(g.old), OldMode: 0o600, OldDevice: gBefore.Device, OldInode: gBefore.Inode, Output: g.next}}, SHA(g.oldManifest), g.newManifest); err == nil || !strings.Contains(err.Error(), "changed before routing commit") {
		t.Fatalf("concurrent edit error = %v", err)
	}
	manifest, _ := os.ReadFile(filepath.Join(g.root, ManifestPath()))
	if string(manifest) != string(g.oldManifest) {
		t.Fatal("refusal changed manifest")
	}
}
