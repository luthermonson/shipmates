//go:build unix

package installer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type UninstallPhase string

const (
	UninstallPreflight     UninstallPhase = "preflight"
	UninstallRemovalIntent UninstallPhase = "removal_intent"
	UninstallCommitted     UninstallPhase = "committed"
	UninstallIncomplete    UninstallPhase = "incomplete"
)

type UninstallObject struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Digest   string `json:"sha256,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Mode     uint32 `json:"mode"`
	Owner    string `json:"owner"`
	Removed  bool   `json:"removed,omitempty"`
	Retained bool   `json:"retained,omitempty"`
	Existed  bool   `json:"existed,omitempty"`
}

type UninstallTransaction struct {
	Schema       string            `json:"schema"`
	ID           string            `json:"transaction_id"`
	ManifestHash string            `json:"manifest_sha256"`
	Phase        UninstallPhase    `json:"phase"`
	Objects      []UninstallObject `json:"objects"`
}

type Phase string

const (
	PhasePreflight          Phase = "preflight"
	PhaseStaging            Phase = "staging"
	PhaseVerification       Phase = "verification"
	PhaseActivationIntent   Phase = "activation_intent"
	PhaseCurrentSwap        Phase = "current_swap"
	PhasePostVerification   Phase = "post_verification"
	PhaseCommit             Phase = "commit"
	PhaseRollbackIntent     Phase = "rollback_intent"
	PhaseRolledBack         Phase = "rolled_back"
	PhaseRollbackIncomplete Phase = "rollback_incomplete"
)

type Transaction struct {
	Schema              string            `json:"schema"`
	ID                  string            `json:"transaction_id"`
	Release             string            `json:"release"`
	ManifestHash        string            `json:"manifest_sha256"`
	DescriptorHash      string            `json:"descriptor_sha256"`
	Descriptor          ReleaseDescriptor `json:"descriptor"`
	PriorRelease        string            `json:"prior_release,omitempty"`
	PriorManifestHash   string            `json:"prior_manifest_sha256,omitempty"`
	PriorDescriptorHash string            `json:"prior_descriptor_sha256,omitempty"`
	PriorPointer        string            `json:"prior_pointer,omitempty"`
	PriorInventory      []Inventory       `json:"prior_inventory,omitempty"`
	Phase               Phase             `json:"phase"`
	Candidate           string            `json:"candidate"`
	CurrentBefore       string            `json:"current_before,omitempty"`
	Inventory           []Inventory       `json:"inventory"`
	Report              Report            `json:"report"`
}

type Inventory struct {
	Destination string `json:"destination"`
	Digest      string `json:"sha256"`
	Size        int64  `json:"size"`
	Mode        uint32 `json:"mode"`
	Owner       string `json:"owner"`
}

var errAdministratorDrift = errors.New("administrator_drift_or_tamper")
var errDrift = errAdministratorDrift

func newTransaction(m Manifest, root, release string, report Report) (Transaction, error) {
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return Transaction{}, errors.New("transaction_identity_failed")
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		return Transaction{}, err
	}
	inv := inventoryForManifest(m)
	d := descriptorFor(m)
	return Transaction{Schema: "shipmates.install.transaction.v2", ID: hex.EncodeToString(idBytes[:]), Release: m.Release, ManifestHash: sha256Hex(canonical), DescriptorHash: descriptorHash(d), Descriptor: d, Phase: PhasePreflight, Candidate: rooted(root, release+".candidate-"+hex.EncodeToString(idBytes[:])), Inventory: inv, Report: report}, nil
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func transactionPath(root string) string { return rooted(root, journalPath) }

func uninstallTransactionPath(root string) string { return rooted(root, uninstallPath) }

func newUninstallTransaction(root string, installTx Transaction, m Manifest) (UninstallTransaction, error) {
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return UninstallTransaction{}, errors.New("transaction_identity_failed")
	}
	objects := make([]UninstallObject, 0, len(m.Assets)*2+2)
	release := rooted(root, filepath.Join(installRoot, "releases", m.Release))
	for _, a := range m.Assets {
		objects = append(objects, UninstallObject{Path: rooted(root, a.Destination), Kind: "fixed", Digest: a.Digest, Size: a.Size, Mode: a.Mode, Owner: a.Owner})
		objects = append(objects, UninstallObject{Path: filepath.Join(release, filepath.Base(a.Destination)), Kind: "release", Digest: a.Digest, Size: a.Size, Mode: a.Mode, Owner: a.Owner})
	}
	currentBytes := []byte(m.Release + "\n")
	objects = append(objects, UninstallObject{Path: rooted(root, filepath.Join(installRoot, "current")), Kind: "current", Digest: sha256Hex(currentBytes), Size: int64(len(currentBytes)), Mode: 0644, Owner: "root:root"})
	_ = installTx
	manifestBytes, _ := json.Marshal(m)
	descriptorBytes, _ := json.Marshal(descriptorFor(m))
	objects = append(objects,
		UninstallObject{Path: filepath.Join(release, releaseManifestName), Kind: "release-metadata", Digest: sha256Hex(manifestBytes), Size: int64(len(manifestBytes)), Mode: 0644, Owner: "root:root"},
		UninstallObject{Path: filepath.Join(release, releaseDescriptorName), Kind: "release-metadata", Digest: sha256Hex(descriptorBytes), Size: int64(len(descriptorBytes)), Mode: 0644, Owner: "root:root"},
		UninstallObject{Path: release, Kind: "directory", Mode: 0755, Owner: "root:root"})
	return UninstallTransaction{Schema: "shipmates.install.uninstall.v1", ID: hex.EncodeToString(idBytes[:]), ManifestHash: manifestHash(m), Phase: UninstallPreflight, Objects: objects}, nil
}

func loadUninstallTransaction(f FileSystem, root string) (UninstallTransaction, error) {
	b, err := f.ReadFile(uninstallTransactionPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return UninstallTransaction{}, nil
	}
	if err != nil {
		return UninstallTransaction{}, errAdministratorDrift
	}
	var tx UninstallTransaction
	if json.Unmarshal(b, &tx) != nil || tx.Schema != "shipmates.install.uninstall.v1" || !validTransactionID(tx.ID) || tx.ManifestHash == "" || !validUninstallPhase(tx.Phase) || len(tx.Objects) < 2 {
		return UninstallTransaction{}, errAdministratorDrift
	}
	return tx, nil
}

func validUninstallPhase(p UninstallPhase) bool {
	return p == UninstallPreflight || p == UninstallRemovalIntent || p == UninstallCommitted || p == UninstallIncomplete
}

func persistUninstallTransaction(f FileSystem, root string, tx UninstallTransaction) error {
	b, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	p := uninstallTransactionPath(root)
	if err := f.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	tmp := p + ".new"
	if _, err := f.Lstat(tmp); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return errAdministratorDrift
	}
	fd, err := f.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = fd.Write(b); err != nil {
		_ = fd.Close()
		return err
	}
	if err = fd.Sync(); err != nil {
		_ = fd.Close()
		return err
	}
	if err = fd.Close(); err != nil {
		return err
	}
	if err = f.Rename(tmp, p); err != nil {
		return err
	}
	return f.SyncDir(filepath.Dir(p))
}

func verifyUninstallObject(f FileSystem, obj *UninstallObject, allowCurrentOwner bool) error {
	info, err := f.Lstat(obj.Path)
	if errors.Is(err, fs.ErrNotExist) {
		obj.Removed = true
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errAdministratorDrift
	}
	obj.Existed = true
	if obj.Kind == "directory" {
		if !info.IsDir() || info.Mode().Perm() != fs.FileMode(obj.Mode) || (!allowCurrentOwner && !ownerIsRoot(info)) {
			return errAdministratorDrift
		}
		return nil
	}
	a := Asset{Digest: obj.Digest, Size: obj.Size, Mode: obj.Mode, Owner: obj.Owner}
	return verifyAsset(f, obj.Path, a, allowCurrentOwner)
}

func removeUninstallObject(f FileSystem, obj *UninstallObject) error {
	if obj.Removed {
		return nil
	}
	if err := f.Remove(obj.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if obj.Kind == "release" || obj.Kind == "directory" || obj.Kind == "current" {
		return f.SyncDir(filepath.Dir(obj.Path))
	}
	return f.SyncDir(filepath.Dir(obj.Path))
}

func uninstallReport(tx UninstallTransaction) Report {
	r := Report{Schema: "shipmates.install.report.v1", Action: "uninstall", Release: ReleaseVersion, Retained: []string{journalPath, uninstallPath}}
	for _, obj := range tx.Objects {
		if obj.Retained {
			r.Retained = append(r.Retained, obj.Path)
		}
		if obj.Removed && obj.Existed {
			r.Changed = true
		}
	}
	return r
}

func loadTransaction(f FileSystem, root string) (Transaction, error) {
	b, err := f.ReadFile(transactionPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return Transaction{}, nil
	}
	if err != nil {
		return Transaction{}, errAdministratorDrift
	}
	var tx Transaction
	if json.Unmarshal(b, &tx) != nil || tx.Schema != "shipmates.install.transaction.v2" || !validTransactionID(tx.ID) || tx.Release == "" || tx.ManifestHash == "" || tx.DescriptorHash == "" || tx.Descriptor.Schema != "shipmates.install.descriptor.v1" || tx.Descriptor.Release != tx.Release || tx.Descriptor.Manifest != tx.ManifestHash || descriptorHash(tx.Descriptor) != tx.DescriptorHash || !validPhase(tx.Phase) || len(tx.Inventory) < 2 {
		return Transaction{}, errAdministratorDrift
	}
	return tx, nil
}

func validTransactionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(b) == id
}

func validPhase(p Phase) bool {
	switch p {
	case PhasePreflight, PhaseStaging, PhaseVerification, PhaseActivationIntent,
		PhaseCurrentSwap, PhasePostVerification, PhaseCommit, PhaseRollbackIntent,
		PhaseRolledBack, PhaseRollbackIncomplete:
		return true
	default:
		return false
	}
}

func persistTransaction(f FileSystem, root string, tx Transaction) error {
	b, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	p := transactionPath(root)
	if err := f.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	tmp := p + ".new"
	if _, statErr := f.Lstat(tmp); statErr == nil {
		return errAdministratorDrift
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return errAdministratorDrift
	}
	fd, err := f.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if _, err = fd.Write(b); err != nil {
		fd.Close()
		return err
	}
	if err = fd.Sync(); err != nil {
		fd.Close()
		return err
	}
	if err = fd.Close(); err != nil {
		return err
	}
	if err = f.Rename(tmp, p); err != nil {
		return err
	}
	return f.SyncDir(filepath.Dir(p))
}

func writeReleaseMetadata(f FileSystem, root string, m Manifest, d ReleaseDescriptor) error {
	if err := validateDescriptor(d, m); err != nil {
		return err
	}
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return err
	}
	descriptorBytes, err := json.Marshal(d)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{releaseManifestName: manifestBytes, releaseDescriptorName: descriptorBytes} {
		p := filepath.Join(root, name)
		fd, err := f.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0644)
		if err != nil {
			return err
		}
		if _, err := fd.Write(data); err != nil {
			_ = fd.Close()
			return err
		}
		if err := fd.Sync(); err != nil {
			_ = fd.Close()
			return err
		}
		if err := fd.Close(); err != nil {
			return err
		}
	}
	return f.SyncDir(root)
}

func acquireLock(root string) (func(), error) {
	p := rooted(root, filepath.Join(stateRoot, "install.lock"))
	if err := checkNativeParents(filepath.Dir(p)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX); err != nil {
		fd.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(fd.Fd()), unix.LOCK_UN); _ = fd.Close() }, nil
}

func checkNativeParents(p string) error {
	clean := filepath.Clean(p)
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("state_unsafe")
		}
	}
	return nil
}

func manifestHash(m Manifest) string { b, _ := json.Marshal(m); return sha256Hex(b) }

func recoverTransaction(f FileSystem, root string, tx Transaction, m Manifest, allowCurrentOwner bool) error {
	if tx.ManifestHash != manifestHash(m) || tx.DescriptorHash != descriptorHash(tx.Descriptor) || validateDescriptor(tx.Descriptor, m) != nil || tx.Release != m.Release || !inventoryMatches(tx, m) {
		return errAdministratorDrift
	}
	if tx.PriorRelease != "" {
		if tx.CurrentBefore != tx.PriorPointer || tx.PriorRelease != tx.PriorPointer || tx.PriorManifestHash == "" || tx.PriorDescriptorHash == "" || len(tx.PriorInventory) == 0 {
			return errAdministratorDrift
		}
		prior, err := loadCommittedReleaseIdentity(f, root, tx.PriorPointer)
		if err != nil || prior.Descriptor.Manifest != tx.PriorManifestHash || descriptorHash(prior.Descriptor) != tx.PriorDescriptorHash || !sameInventory(prior.Inventory, tx.PriorInventory) {
			return errAdministratorDrift
		}
		if err := ValidateUpgrade(prior, ReleaseIdentity{Descriptor: tx.Descriptor, Manifest: m, Pointer: tx.Release, Inventory: tx.Inventory}); err != nil {
			return errAdministratorDrift
		}
	}
	if tx.Candidate != rooted(root, filepath.Join(installRoot, "releases", m.Release+".candidate-"+tx.ID)) {
		return errAdministratorDrift
	}
	if tx.Phase == PhaseCommit || tx.Phase == PhaseRolledBack {
		return nil
	}
	release := rooted(root, filepath.Join(installRoot, "releases", m.Release))
	current := rooted(root, filepath.Join(installRoot, "current"))
	// A crash can occur after the candidate is renamed but before the
	// activation-intent record is durable. Treat an exact release as the
	// transaction's object, never as an invitation to accept arbitrary state.
	releaseInfo, releaseErr := f.Lstat(release)
	if tx.Phase == PhaseVerification && errors.Is(releaseErr, fs.ErrNotExist) {
		if candidateInfo, candidateErr := f.Lstat(tx.Candidate); candidateErr == nil && candidateInfo.IsDir() {
			if verifyRelease(f, tx.Candidate, m, allowCurrentOwner) != nil || f.Rename(tx.Candidate, release) != nil || f.SyncDir(filepath.Dir(release)) != nil {
				return errAdministratorDrift
			}
			releaseInfo, releaseErr = f.Lstat(release)
		}
	}
	activate := tx.Phase == PhaseActivationIntent || tx.Phase == PhaseCurrentSwap || tx.Phase == PhasePostVerification || (tx.Phase == PhaseVerification && releaseErr == nil)
	if activate {
		if releaseErr != nil || !releaseInfo.IsDir() || verifyRelease(f, release, m, allowCurrentOwner) != nil {
			return errAdministratorDrift
		}
		if old, err := f.ReadFile(current); err == nil {
			value := strings.TrimSpace(string(old))
			if value != "" && value != tx.CurrentBefore && value != tx.Release {
				return errAdministratorDrift
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return errAdministratorDrift
		}
		if err := writeAtomicText(f, current, ReleaseVersion, 0644); err != nil {
			return errAdministratorDrift
		}
		for _, a := range m.Assets {
			if err := copyFixed(f, filepath.Join(release, filepath.Base(a.Destination)), rooted(root, a.Destination), a, allowCurrentOwner); err != nil {
				return errAdministratorDrift
			}
		}
		tx.Phase = PhasePostVerification
		if err := persistTransaction(f, root, tx); err != nil {
			return errAdministratorDrift
		}
		tx.Phase = PhaseCommit
		if err := persistTransaction(f, root, tx); err != nil {
			return errAdministratorDrift
		}
		return nil
	}
	// A candidate is disposable only when it is the exact transaction-bound
	// path and its full inventory matches. Never remove a substituted path.
	if tx.Candidate == "" || filepath.Dir(filepath.Clean(tx.Candidate)) != rooted(root, filepath.Join(installRoot, "releases")) {
		return errAdministratorDrift
	}
	if info, err := f.Lstat(tx.Candidate); err == nil {
		if !info.IsDir() || verifyRelease(f, tx.Candidate, m, allowCurrentOwner) != nil {
			return errAdministratorDrift
		}
		if err := removeReleaseTree(f, tx.Candidate, m); err != nil {
			return errAdministratorDrift
		}
	}
	tx.Phase = PhaseRollbackIntent
	if err := persistTransaction(f, root, tx); err != nil {
		return errAdministratorDrift
	}
	tx.Phase = PhaseRolledBack
	if err := persistTransaction(f, root, tx); err != nil {
		return errAdministratorDrift
	}
	return nil
}

func removeReleaseTree(f FileSystem, root string, m Manifest) error {
	for _, name := range []string{releaseManifestName, releaseDescriptorName} {
		if err := f.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, a := range m.Assets {
		p := filepath.Join(root, filepath.Base(a.Destination))
		if err := f.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := f.Remove(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return f.SyncDir(filepath.Dir(root))
}

func inventoryMatches(tx Transaction, m Manifest) bool {
	return sameInventory(tx.Inventory, inventoryForManifest(m))
}

func sameInventory(got, want []Inventory) bool {
	if len(got) != len(want) {
		return false
	}
	for i, a := range want {
		if got[i].Destination != a.Destination || got[i].Digest != a.Digest || got[i].Size != a.Size || got[i].Mode != a.Mode || got[i].Owner != a.Owner {
			return false
		}
	}
	return true
}
