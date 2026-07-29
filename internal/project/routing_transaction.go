package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const routingTransactionName = "routing-transaction"

type RoutingArtifact struct {
	Name, OldHash string
	OldMode       os.FileMode
	OldDevice     uint64
	OldInode      uint64
	Output        []byte
}

type routingJournalEntry struct {
	Name, Path, Stage, Backup, OldHash, NewHash string
	OldMode                                     uint32
}

type routingJournal struct {
	Version                          int
	OldManifestHash, NewManifestHash string
	Entries                          []routingJournalEntry
}

// LoadManifestSnapshot returns one decoded manifest and the hash of the exact
// bytes it decoded, for a later transaction CAS.
func LoadManifestSnapshot(root string) (*Manifest, string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ManifestPath()))
	if err != nil {
		return nil, "", err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", err
	}
	if m.Files == nil {
		return nil, "", errors.New("manifest files map is missing")
	}
	return &m, SHA(raw), nil
}

func routingTxnDir(root string) string { return filepath.Join(root, Dir, routingTransactionName) }
func routingJournalPath(root string) string {
	return filepath.Join(routingTxnDir(root), "journal.json")
}

func RoutingTransactionsSupported() bool { return routingAtomicExchangeSupported }

// RecoverRoutingTransaction deterministically resolves an interrupted routing
// transaction. The manifest is the commit record: old means roll back, new
// means finish forward. Any third state is refused as externally modified.
func RecoverRoutingTransaction(root string) error {
	if !routingAtomicExchangeSupported {
		return errors.New("atomic routing transactions are unsupported on this platform")
	}
	jraw, err := routingReadArtifact(root, path.Join(Dir, routingTransactionName, "journal.json"))
	if errors.Is(err, os.ErrNotExist) {
		// The journal is written last, so a crash between creating the
		// transaction directory and making the journal durable leaves debris
		// with nothing committed. Clearing it here is the only thing that keeps
		// the next ApplyRoutingTransaction from failing forever at its mkdir on
		// a path this function used to return from without cleanup.
		if _, statErr := os.Lstat(routingTxnDir(root)); statErr == nil {
			return removeRoutingTxn(root)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read routing transaction journal: %w", err)
	}
	var j routingJournal
	dec := json.NewDecoder(strings.NewReader(string(jraw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil || dec.Decode(&struct{}{}) != io.EOF || validateRoutingJournal(root, &j) != nil {
		return errors.New("invalid routing transaction journal; recovery required")
	}
	mraw, err := os.ReadFile(filepath.Join(root, ManifestPath()))
	if err != nil {
		return fmt.Errorf("read manifest during routing recovery: %w", err)
	}
	switch SHA(mraw) {
	case j.OldManifestHash:
		if err := rollbackRouting(root, &j); err != nil {
			return fmt.Errorf("routing rollback incomplete: %w", err)
		}
	case j.NewManifestHash:
		if err := finishRouting(root, &j); err != nil {
			return fmt.Errorf("routing recovery incomplete: %w", err)
		}
	default:
		return errors.New("manifest changed during routing recovery; recovery required")
	}
	return removeRoutingTxn(root)
}

// ApplyRoutingTransaction installs all artifacts and their already-serialized
// manifest as one recoverable transaction. Callers must hold the project write lock.
func ApplyRoutingTransaction(root string, artifacts []RoutingArtifact, oldManifestHash string, newManifest []byte) error {
	if len(artifacts) == 0 {
		return errors.New("routing transaction has no artifacts")
	}
	if !routingAtomicExchangeSupported {
		return errors.New("atomic routing transactions are unsupported on this platform")
	}
	if err := RecoverRoutingTransaction(root); err != nil {
		return err
	}
	txn := routingTxnDir(root)
	if err := routingMkdir(root, path.Join(Dir, routingTransactionName), 0o700); err != nil {
		return fmt.Errorf("create routing transaction: %w", err)
	}
	if err := routingMkdir(root, path.Join(Dir, routingTransactionName, "stage"), 0o700); err != nil {
		return cleanupRoutingFailure(root, err)
	}
	if err := routingMkdir(root, path.Join(Dir, routingTransactionName, "backup"), 0o700); err != nil {
		return cleanupRoutingFailure(root, err)
	}
	j := routingJournal{Version: 1, OldManifestHash: oldManifestHash, NewManifestHash: SHA(newManifest)}
	for i, a := range artifacts {
		if err := ValidatePersonaName(a.Name); err != nil {
			return cleanupRoutingFailure(root, err)
		}
		if a.OldMode.Perm()&0o133 != 0 {
			return cleanupRoutingFailure(root, fmt.Errorf("persona %q must not be executable or group- or world-writable", a.Name))
		}
		if a.OldDevice == 0 || a.OldInode == 0 {
			return cleanupRoutingFailure(root, fmt.Errorf("persona %q is missing its filesystem identity", a.Name))
		}
		if _, err := ParseCodexAgent(a.Output); err != nil {
			return cleanupRoutingFailure(root, fmt.Errorf("persona %q output: %w", a.Name, err))
		}
		e := routingJournalEntry{Name: a.Name, Path: CodexAgentPath(a.Name), Stage: filepath.Join("stage", fmt.Sprintf("%03d.toml", i)), Backup: filepath.Join("backup", fmt.Sprintf("%03d.toml", i)), OldHash: a.OldHash, NewHash: SHA(a.Output), OldMode: uint32(a.OldMode.Perm())}
		j.Entries = append(j.Entries, e)
		if err := routingWriteArtifact(root, path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage)), a.Output); err != nil {
			return cleanupRoutingFailure(root, err)
		}
	}
	if err := assertRoutingStages(root, &j); err != nil {
		return cleanupRoutingFailure(root, err)
	}
	if err := routingWriteArtifact(root, path.Join(Dir, routingTransactionName, "manifest.new"), newManifest); err != nil {
		return cleanupRoutingFailure(root, err)
	}
	if err := writeRoutingJournal(root, &j); err != nil {
		return cleanupRoutingFailure(root, err)
	}
	// Final CAS after every output and the journal are durable.
	for _, e := range j.Entries {
		item, err := CanonicalPersonaAt(root, e.Name)
		if err != nil || SHA(item.Raw) != e.OldHash || item.Mode.Perm() != os.FileMode(e.OldMode).Perm() {
			if err == nil {
				err = errors.New("content or mode changed")
			}
			return cleanupRoutingFailure(root, fmt.Errorf("persona %q changed before routing commit: %w", e.Name, err))
		}
	}
	mraw, err := os.ReadFile(filepath.Join(root, ManifestPath()))
	if err != nil || SHA(mraw) != j.OldManifestHash {
		return cleanupRoutingFailure(root, errors.New("manifest changed before routing commit"))
	}
	for i, e := range j.Entries {
		if err := assertRoutingStage(root, e); err != nil {
			return recoverAfterRoutingFailure(root, err)
		}
		stageRel := path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage))
		if err := routingExchangeDestination(root, stageRel, e.Path, artifacts[i].OldDevice, artifacts[i].OldInode, e.OldHash, os.FileMode(e.OldMode)); err != nil {
			if errors.Is(err, errRoutingDestinationChanged) {
				err = fmt.Errorf("persona %q changed at routing commit: %w", e.Name, err)
			}
			// Never remove the transaction directory from inside this loop.
			// Entries before this one are already live at their destinations
			// and the only copies of their originals are the journal-bound
			// backups inside that directory; the journal is the only thing
			// that can put them back. This also covers a failed exchange-back,
			// where even the first entry's displaced original is still in
			// stage. Recovery is always the correct response here.
			return recoverAfterRoutingFailure(root, err)
		}
		if err := routingRename(root, stageRel, path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Backup))); err != nil {
			return recoverAfterRoutingFailure(root, err)
		}
		if err := syncRoutingDirs(filepath.Dir(filepath.Join(txn, e.Stage)), filepath.Dir(filepath.Join(root, e.Path))); err != nil {
			return recoverAfterRoutingFailure(root, err)
		}
	}
	if err := routingReplace(root, path.Join(Dir, routingTransactionName, "manifest.new"), ManifestPath()); err != nil {
		return recoverAfterRoutingFailure(root, err)
	}
	if err := syncRoutingDirs(txn, filepath.Dir(filepath.Join(root, ManifestPath()))); err != nil {
		return errors.New("routing manifest rename completed but durability is uncertain; recovery required")
	}
	if err := removeRoutingTxn(root); err != nil {
		return fmt.Errorf("routing committed; cleanup pending: %w", err)
	}
	return nil
}

func writeRoutingJournal(root string, j *routingJournal) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := assertRoutingStages(root, j); err != nil {
		return err
	}
	if err := routingWriteArtifact(root, path.Join(Dir, routingTransactionName, "journal.json"), raw); err != nil {
		return err
	}
	return syncRoutingDirs(routingTxnDir(root))
}

func assertRoutingStage(root string, e routingJournalEntry) error {
	rel := path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage))
	raw, err := routingReadArtifact(root, rel)
	if err != nil || SHA(raw) != e.NewHash {
		return fmt.Errorf("missing valid stage for %s", e.Path)
	}
	return nil
}

func assertRoutingStages(root string, j *routingJournal) error {
	for _, e := range j.Entries {
		if err := assertRoutingStage(root, e); err != nil {
			return err
		}
	}
	return nil
}

func validRoutingHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validateRoutingJournal(root string, j *routingJournal) error {
	if j.Version != 1 || len(j.Entries) == 0 || len(j.Entries) > MaxInstalledPersonas || !validRoutingHash(j.OldManifestHash) || !validRoutingHash(j.NewManifestHash) {
		return errors.New("invalid journal schema")
	}
	seen := map[string]bool{}
	for i, e := range j.Entries {
		if ValidatePersonaName(e.Name) != nil || seen[e.Name] || e.Path != filepath.ToSlash(CodexAgentPath(e.Name)) || e.Stage != path.Join("stage", fmt.Sprintf("%03d.toml", i)) || e.Backup != path.Join("backup", fmt.Sprintf("%03d.toml", i)) || !validRoutingHash(e.OldHash) || !validRoutingHash(e.NewHash) || os.FileMode(e.OldMode).Perm()&0o133 != 0 || e.OldMode != uint32(os.FileMode(e.OldMode).Perm()) {
			return errors.New("invalid journal entry")
		}
		seen[e.Name] = true
	}
	return validateRoutingFilesystem(root, j)
}

func rollbackRouting(root string, j *routingJournal) error {
	var out error
	for i := len(j.Entries) - 1; i >= 0; i-- {
		e := j.Entries[i]
		dst, backup := filepath.Join(root, e.Path), filepath.Join(routingTxnDir(root), e.Backup)
		if raw, err := routingReadArtifact(root, e.Path); err == nil && SHA(raw) == e.NewHash {
			out = errors.Join(out, routingUnlink(root, e.Path))
		} else if err == nil && SHA(raw) == e.OldHash {
			continue // this entry had not crossed its first rename
		} else if err == nil {
			out = errors.Join(out, fmt.Errorf("unexpected destination for %s", e.Path))
			continue
		}
		backupRel := path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Backup))
		if raw, err := routingReadArtifact(root, backupRel); err == nil {
			if SHA(raw) != e.OldHash {
				out = errors.Join(out, fmt.Errorf("backup changed for %s", e.Path))
				continue
			}
			if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
				out = errors.Join(out, routingRename(root, path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Backup)), e.Path))
			} else if err != nil {
				out = errors.Join(out, err)
			}
		} else if errors.Is(err, os.ErrNotExist) {
			// Immediately after the atomic exchange, the displaced original is
			// still at stage until the following rename makes it the backup.
			stageRel := path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage))
			if raw, stageErr := routingReadArtifact(root, stageRel); stageErr == nil && SHA(raw) == e.OldHash {
				if _, statErr := os.Lstat(dst); errors.Is(statErr, os.ErrNotExist) {
					out = errors.Join(out, routingRename(root, stageRel, e.Path))
				} else if statErr != nil {
					out = errors.Join(out, statErr)
				}
			} else if stageErr != nil {
				out = errors.Join(out, stageErr)
			} else {
				out = errors.Join(out, fmt.Errorf("missing original for %s", e.Path))
			}
		} else {
			out = errors.Join(out, err)
		}
		out = errors.Join(out, syncRoutingDirs(filepath.Dir(dst), filepath.Dir(backup)))
	}
	return out
}

func finishRouting(root string, j *routingJournal) error {
	var out error
	for _, e := range j.Entries {
		dst, stage := filepath.Join(root, e.Path), filepath.Join(routingTxnDir(root), e.Stage)
		raw, err := routingReadArtifact(root, e.Path)
		if err == nil && SHA(raw) == e.NewHash {
			continue
		}
		if err == nil {
			if SHA(raw) != e.OldHash {
				out = errors.Join(out, fmt.Errorf("committed persona changed: %s", e.Path))
				continue
			}
			// The manifest is already the commit record. A later persona may
			// still contain its exact old bytes if recovery starts between
			// persona exchanges. Remove only that journal-bound identity; a
			// retry can still install the durable stage if the rename fails.
			if unlinkErr := routingUnlink(root, e.Path); unlinkErr != nil {
				out = errors.Join(out, unlinkErr)
				continue
			}
		}
		stageRel := path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage))
		if sraw, serr := routingReadArtifact(root, stageRel); serr != nil || SHA(sraw) != e.NewHash {
			out = errors.Join(out, fmt.Errorf("missing valid stage for %s", e.Path))
			continue
		}
		out = errors.Join(out, routingRename(root, path.Join(Dir, routingTransactionName, filepath.ToSlash(e.Stage)), e.Path), syncRoutingDirs(filepath.Dir(stage), filepath.Dir(dst)))
	}
	return out
}

func recoverAfterRoutingFailure(root string, cause error) error {
	if err := RecoverRoutingTransaction(root); err != nil {
		return fmt.Errorf("routing commit failed: %v; recovery failed: %w", cause, err)
	}
	return fmt.Errorf("routing commit failed and was rolled back: %w", cause)
}
func cleanupRoutingFailure(root string, cause error) error {
	return errors.Join(cause, removeRoutingTxn(root))
}
func removeRoutingTxn(root string) error {
	err := os.RemoveAll(routingTxnDir(root))
	if err != nil {
		return err
	}
	return syncRoutingDirs(filepath.Join(root, Dir))
}
func syncRoutingDirs(dirs ...string) error {
	var out error
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		f, err := os.Open(d)
		if err == nil {
			err = f.Sync()
			err = errors.Join(err, f.Close())
		}
		out = errors.Join(out, err)
	}
	return out
}
