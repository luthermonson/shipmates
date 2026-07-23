package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	errRootRequired = errors.New("root_required")
)

type Options struct {
	// Root is only for deterministic tests. The CLI leaves it empty, meaning /
	// and therefore fixed absolute destinations are retained.
	Root         string
	EffectiveUID int
	DryRun       bool
	Uninstall    bool
	JSON         bool
	FS           FileSystem
	Fence        UnitFence
	Qualifier    QualifierFence
	Profile      string
	Capabilities *CapabilitySnapshot
}

// UnitFence is deliberately injected. The default installer does not invoke
// systemctl; a host integration may provide an already-authorized inactive
// check without changing the manifest or destination policy.
type UnitFence interface{ CheckInactive() error }

type Report struct {
	Schema      string      `json:"schema"`
	Action      string      `json:"action"`
	Release     string      `json:"release"`
	Changed     bool        `json:"changed"`
	DryRun      bool        `json:"dry_run"`
	Retained    []string    `json:"retained,omitempty"`
	Composition Composition `json:"composition"`
}

type FileSystem interface {
	MkdirAll(string, fs.FileMode) error
	Lstat(string) (fs.FileInfo, error)
	OpenFile(string, int, fs.FileMode) (FileHandle, error)
	Rename(string, string) error
	Remove(string) error
	ReadFile(string) ([]byte, error)
	WriteFile(string, []byte, fs.FileMode) error
	SyncDir(string) error
}

// FileHandle is the smallest file contract used by the installer. Keeping it
// separate from *os.File lets deterministic test filesystems observe and
// fault individual write/sync/stat/close boundaries without changing the
// production implementation or its no-follow flags.
type FileHandle interface {
	io.Writer
	io.Closer
	Chmod(fs.FileMode) error
	Sync() error
	Stat() (fs.FileInfo, error)
}

type osFS struct{}

func (osFS) MkdirAll(p string, m fs.FileMode) error                      { return os.MkdirAll(p, m) }
func (osFS) Lstat(p string) (fs.FileInfo, error)                         { return os.Lstat(p) }
func (osFS) OpenFile(p string, f int, m fs.FileMode) (FileHandle, error) { return os.OpenFile(p, f, m) }
func (osFS) Rename(a, b string) error                                    { return os.Rename(a, b) }
func (osFS) Remove(p string) error                                       { return os.Remove(p) }
func (osFS) ReadFile(p string) ([]byte, error)                           { return os.ReadFile(p) }
func (osFS) WriteFile(p string, b []byte, m fs.FileMode) error           { return os.WriteFile(p, b, m) }
func (osFS) SyncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func Install(o Options) (Report, error) {
	if o.Uninstall {
		return Uninstall(o)
	}
	if o.EffectiveUID != 0 {
		return Report{}, errRootRequired
	}
	root := o.Root
	if root == "" {
		root = "/"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Report{}, errors.New("install_root_invalid")
	}
	if o.DryRun {
		return installLocked(o, nil)
	}
	lease, err := acquirePublicQualifierLease(o, root)
	if err != nil {
		return Report{}, err
	}
	if lease != nil {
		defer lease.Close()
	}
	unlock, err := acquireLock(root)
	if err != nil {
		if err.Error() == "state_unsafe" {
			return Report{}, err
		}
		return Report{}, errors.New("install_lock_unavailable")
	}
	defer unlock()
	return installLocked(o, lease)
}

func acquirePublicQualifierLease(o Options, root string) (*FenceLease, error) {
	if o.DryRun {
		return nil, nil
	}
	if o.Qualifier != nil {
		return o.Qualifier.Acquire()
	}
	if o.Fence != nil {
		if err := o.Fence.CheckInactive(); err != nil {
			return nil, errors.New("active_unit_refused")
		}
		return nil, nil
	}
	// Direct package callers with a temporary test root retain the historical
	// fixture behavior. The public CLI always supplies Qualifier explicitly.
	if o.Root != "" {
		return nil, nil
	}
	return NewProductionQualifierFence(root).Acquire()
}

func installLocked(o Options, lease *FenceLease) (Report, error) {
	if o.EffectiveUID != 0 {
		return Report{}, errRootRequired
	}
	root := o.Root
	if root == "" {
		root = "/"
	}
	allowTestOwner := o.Root != ""
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Report{}, errors.New("install_root_invalid")
	}
	f := o.FS
	if f == nil {
		f = osFS{}
	}
	if !o.DryRun {
		if _, e := f.Lstat(transactionPath(root) + ".new"); e == nil {
			return Report{}, errAdministratorDrift
		} else if !errors.Is(e, fs.ErrNotExist) {
			return Report{}, errAdministratorDrift
		}
	}
	var caps CapabilitySnapshot
	if o.Capabilities != nil {
		caps = *o.Capabilities
	} else if detected, detectErr := DetectPlatform(); detectErr == nil {
		caps = detected
	} else {
		caps.Platform = PlatformUnknown
	}
	composition, compErr := DetectFromSnapshot(caps)
	if compErr != nil {
		composition = Composition{Mode: "ordinary-fallback", Platform: PlatformUnknown, CredentialLayout: "ordinary-user", Guidance: "platform capability unavailable; ordinary Shipmates operation retained"}
	}
	composition, compErr = ComposeProfile(composition, o.Profile)
	if compErr != nil {
		return Report{}, compErr
	}
	m, err := manifestFor(composition.Hardened)
	if err != nil {
		return Report{}, err
	}
	if err := validateManifest(m); err != nil {
		return Report{}, err
	}
	report := Report{Schema: "shipmates.install.report.v1", Action: "install", Release: m.Release, DryRun: o.DryRun, Composition: composition}
	release := rooted(root, filepath.Join(installRoot, "releases", m.Release))
	current := rooted(root, filepath.Join(installRoot, "current"))
	if tx, txErr := loadTransaction(f, root); txErr != nil && !o.DryRun {
		return Report{}, txErr
	} else if tx.ID != "" && !o.DryRun {
		if tx.Release == m.Release {
			if err := recoverTransaction(f, root, tx, m, allowTestOwner); err != nil {
				return Report{}, err
			}
		} else if tx.Phase != PhaseCommit {
			// Only the installer that created an interrupted transaction may
			// recover it. A newer release may consume a fully committed
			// predecessor after verifying its immutable release identity below.
			return Report{}, errAdministratorDrift
		}
	}
	if info, e := f.Lstat(release); e == nil && !info.IsDir() {
		return Report{}, errDrift
	}
	if info, e := f.Lstat(current); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return Report{}, errDrift
	}
	if o.DryRun {
		report.Changed = true
		return report, nil
	}
	checkLease := func() error {
		if lease == nil {
			return nil
		}
		return lease.Recheck()
	}
	if o.Qualifier != nil {
		if err := o.Qualifier.CheckInactive(); err != nil {
			return Report{}, errors.New("active_unit_refused")
		}
	} else if o.Fence != nil {
		if err := o.Fence.CheckInactive(); err != nil {
			return Report{}, errors.New("active_unit_refused")
		}
	}
	if info, e := f.Lstat(release); e == nil && info.IsDir() {
		if err := verifyRelease(f, release, m, allowTestOwner); err != nil {
			return Report{}, errDrift
		}
		if err := verifyFixed(f, root, release, m, allowTestOwner); err != nil {
			return Report{}, errDrift
		}
		return report, nil
	} else if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return Report{}, errors.New("install_state_unavailable")
	}
	if err := checkParents(f, rooted(root, filepath.Join(installRoot, "releases"))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Report{}, errors.New("install_parent_unsafe")
	}
	if err := f.MkdirAll(rooted(root, filepath.Join(installRoot, "releases")), 0755); err != nil {
		return Report{}, errors.New("install_parent_unavailable")
	}
	if err := checkParents(f, rooted(root, filepath.Join(installRoot, "releases"))); err != nil {
		return Report{}, errors.New("install_parent_unsafe")
	}
	stateReleases := rooted(root, filepath.Join(stateRoot, "releases"))
	if err := checkParents(f, stateReleases); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Report{}, errors.New("install_state_unsafe")
	}
	if err := f.MkdirAll(stateReleases, 0755); err != nil {
		return Report{}, errors.New("install_state_unavailable")
	}
	if err := checkParents(f, stateReleases); err != nil {
		return Report{}, errors.New("install_state_unsafe")
	}
	var predecessor *ReleaseIdentity
	priorPointer := ""
	if old, e := f.ReadFile(current); e == nil {
		priorPointer = strings.TrimSpace(string(old))
		if priorPointer != "" && priorPointer != ReleaseVersion {
			identity, err := loadCommittedReleaseIdentity(f, root, priorPointer)
			if err != nil {
				return Report{}, err
			}
			predecessor = &identity
			candidate := ReleaseIdentity{Descriptor: descriptorFor(m), Manifest: m, Pointer: ReleaseVersion, Inventory: inventoryForManifest(m)}
			if err := ValidateUpgrade(identity, candidate); err != nil {
				return Report{}, err
			}
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return Report{}, errAdministratorDrift
	}
	tx, err := newTransaction(m, root, filepath.Join(installRoot, "releases", ReleaseVersion), report)
	if err != nil {
		return Report{}, err
	}
	if predecessor != nil {
		tx.PriorRelease = predecessor.Descriptor.Release
		tx.PriorManifestHash = predecessor.Descriptor.Manifest
		tx.PriorDescriptorHash = descriptorHash(predecessor.Descriptor)
		tx.PriorPointer = predecessor.Pointer
		tx.PriorInventory = predecessor.Inventory
	}
	tx.CurrentBefore = priorPointer
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	tmp := tx.Candidate
	if _, e := f.Lstat(tmp); e == nil {
		return Report{}, errAdministratorDrift
	} else if !errors.Is(e, fs.ErrNotExist) {
		return Report{}, errAdministratorDrift
	}
	committed := false
	currentExisted := false
	if _, e := f.Lstat(current); e == nil {
		currentExisted = true
	}
	fixedExisted := make(map[string]bool, len(m.Assets))
	for _, a := range m.Assets {
		if _, e := f.Lstat(rooted(root, a.Destination)); e == nil {
			fixedExisted[a.Destination] = true
		}
	}
	defer func() {
		if !committed {
			_ = f.Remove(release)
			if !currentExisted {
				_ = f.Remove(current)
			}
			for _, a := range m.Assets {
				if !fixedExisted[a.Destination] {
					_ = f.Remove(rooted(root, a.Destination))
				}
			}
		}
	}()
	tx.Phase = PhaseStaging
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	if err := checkLease(); err != nil {
		return Report{}, errors.New("active_unit_refused")
	}
	if err := f.MkdirAll(tmp, 0755); err != nil {
		return Report{}, errors.New("install_stage_unavailable")
	}
	defer f.Remove(tmp)
	if err := writeReleaseMetadata(f, tmp, m, descriptorFor(m)); err != nil {
		return Report{}, errors.New("release_metadata_failed")
	}
	for _, a := range m.Assets {
		b, err := assetBytes(a.Source)
		if err != nil {
			return Report{}, errors.New("release_asset_unavailable")
		}
		if err := writeVerified(f, filepath.Join(tmp, filepath.Base(a.Destination)), b, a, allowTestOwner); err != nil {
			return Report{}, err
		}
	}
	if err := f.SyncDir(tmp); err != nil {
		return Report{}, errors.New("install_sync_failed")
	}
	tx.Phase = PhaseVerification
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	if err := checkLease(); err != nil {
		return Report{}, errors.New("active_unit_refused")
	}
	if err := f.Rename(tmp, release); err != nil {
		return Report{}, errors.New("install_activate_failed")
	}
	if err := f.SyncDir(filepath.Dir(release)); err != nil {
		return Report{}, errors.New("install_sync_failed")
	}
	tx.Phase = PhaseActivationIntent
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	if info, e := f.Lstat(current); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return Report{}, errDrift
	} else if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return Report{}, errors.New("install_current_failed")
	}
	if old, e := f.ReadFile(current); e == nil && strings.TrimSpace(string(old)) != tx.CurrentBefore {
		return Report{}, errDrift
	} else if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return Report{}, errDrift
	}
	if err := checkLease(); err != nil {
		return Report{}, errors.New("active_unit_refused")
	}
	if err := writeAtomicText(f, current, ReleaseVersion, 0644); err != nil {
		return Report{}, errors.New("install_current_failed")
	}
	tx.Phase = PhaseCurrentSwap
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	for _, a := range m.Assets {
		if err := checkLease(); err != nil {
			return Report{}, errors.New("active_unit_refused")
		}
		if err := copyFixed(f, filepath.Join(release, filepath.Base(a.Destination)), rooted(root, a.Destination), a, allowTestOwner); err != nil {
			return Report{}, err
		}
	}
	tx.Phase = PhasePostVerification
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	tx.Phase = PhaseCommit
	report.Changed = true
	tx.Report = report
	if err := persistTransaction(f, root, tx); err != nil {
		return Report{}, errors.New("install_journal_failed")
	}
	committed = true
	return report, nil
}

func Uninstall(o Options) (Report, error) {
	if o.EffectiveUID != 0 {
		return Report{}, errRootRequired
	}
	root := o.Root
	if root == "" {
		root = "/"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Report{}, errors.New("install_root_invalid")
	}
	f := o.FS
	if f == nil {
		f = osFS{}
	}
	if o.Fence == nil && o.Qualifier == nil && o.Root != "" {
		return Report{}, errors.New("uninstall_state_unknown")
	}
	if o.DryRun {
		return Report{Schema: "shipmates.install.report.v1", Action: "uninstall", Release: ReleaseVersion, DryRun: true, Retained: []string{journalPath}}, nil
	}
	lease, err := acquirePublicQualifierLease(o, root)
	if err != nil {
		return Report{}, errors.New("active_unit_refused")
	}
	if lease != nil {
		defer lease.Close()
	}
	unlock, err := acquireLock(root)
	if err != nil {
		if err.Error() == "state_unsafe" {
			return Report{}, err
		}
		return Report{}, errors.New("install_lock_unavailable")
	}
	defer unlock()
	return uninstallLocked(o, root, f, lease)
}

func uninstallLocked(o Options, root string, f FileSystem, lease *FenceLease) (Report, error) {
	installTx, err := loadTransaction(f, root)
	if err != nil || installTx.ID == "" || installTx.Phase != PhaseCommit {
		return Report{}, errAdministratorDrift
	}
	m, err := manifestFor(len(installTx.Inventory) == 3)
	if err != nil || installTx.ManifestHash != manifestHash(m) || installTx.DescriptorHash != descriptorHash(installTx.Descriptor) || validateDescriptor(installTx.Descriptor, m) != nil || !inventoryMatches(installTx, m) {
		return Report{}, errAdministratorDrift
	}
	utx, err := loadUninstallTransaction(f, root)
	if err != nil {
		return Report{}, err
	}
	if utx.ID != "" && !uninstallTransactionMatches(utx, root, installTx, m) {
		return Report{}, errAdministratorDrift
	}
	if utx.ID != "" && utx.Phase == UninstallCommitted {
		report := uninstallReport(utx)
		if len(report.Retained) > 2 {
			return report, errors.New("uninstall_incomplete")
		}
		return report, nil
	}
	if utx.ID == "" {
		utx, err = newUninstallTransaction(root, installTx, m)
		if err != nil {
			return Report{}, err
		}
		if err := persistUninstallTransaction(f, root, utx); err != nil {
			return Report{}, errors.New("uninstall_journal_failed")
		}
	}
	if utx.Phase == UninstallPreflight || utx.Phase == UninstallRemovalIntent || utx.Phase == UninstallIncomplete {
		utx.Phase = UninstallRemovalIntent
		if err := persistUninstallTransaction(f, root, utx); err != nil {
			return Report{}, errors.New("uninstall_journal_failed")
		}
	}
	for i := range utx.Objects {
		obj := &utx.Objects[i]
		if obj.Removed || obj.Retained {
			continue
		}
		if lease != nil {
			if err := lease.Recheck(); err != nil {
				return uninstallReport(utx), errors.New("active_unit_refused")
			}
		}
		if err := verifyUninstallObject(f, obj, o.Root != ""); err != nil {
			obj.Retained = true
			continue
		}
		if err := removeUninstallObject(f, obj); err != nil {
			utx.Phase = UninstallIncomplete
			_ = persistUninstallTransaction(f, root, utx)
			return uninstallReport(utx), errors.New("uninstall_incomplete")
		}
		obj.Removed = true
		if err := persistUninstallTransaction(f, root, utx); err != nil {
			return uninstallReport(utx), errors.New("uninstall_journal_failed")
		}
	}
	utx.Phase = UninstallCommitted
	if err := persistUninstallTransaction(f, root, utx); err != nil {
		return uninstallReport(utx), errors.New("uninstall_journal_failed")
	}
	report := uninstallReport(utx)
	if len(report.Retained) > 2 {
		return report, errors.New("uninstall_incomplete")
	}
	return report, nil
}

// uninstallTransactionMatches treats the persisted uninstall journal as a
// progress record only.  Its object identities are always re-derived from the
// committed install inventory; a modified journal must never select a new
// deletion target or claim that an existing object was removed.
func uninstallTransactionMatches(tx UninstallTransaction, root string, installTx Transaction, m Manifest) bool {
	expected, err := newUninstallTransaction(root, installTx, m)
	if err != nil || tx.ManifestHash != expected.ManifestHash || len(tx.Objects) != len(expected.Objects) {
		return false
	}
	for i, want := range expected.Objects {
		got := tx.Objects[i]
		if got.Path != want.Path || got.Kind != want.Kind || got.Digest != want.Digest || got.Size != want.Size || got.Mode != want.Mode || got.Owner != want.Owner {
			return false
		}
		if got.Removed {
			if _, err := os.Lstat(got.Path); !errors.Is(err, fs.ErrNotExist) {
				return false
			}
		}
	}
	return true
}

func writeVerified(f FileSystem, p string, b []byte, a Asset, allowCurrentOwner bool) error {
	s := sha256.Sum256(b)
	if int64(len(b)) != a.Size || hex.EncodeToString(s[:]) != a.Digest {
		return errors.New("asset_digest_mismatch")
	}
	fd, err := f.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, fs.FileMode(a.Mode))
	if err != nil {
		return errors.New("asset_write_failed")
	}
	defer fd.Close()
	if err := fd.Chmod(fs.FileMode(a.Mode)); err != nil {
		return errors.New("asset_mode_failed")
	}
	if _, err := fd.Write(b); err != nil {
		return errors.New("asset_write_failed")
	}
	if err := fd.Sync(); err != nil {
		return errors.New("asset_sync_failed")
	}
	st, err := fd.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != fs.FileMode(a.Mode) {
		return errors.New("asset_identity_failed")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 && !allowCurrentOwner {
		return errors.New("asset_owner_failed")
	}
	return nil
}

func copyFixed(f FileSystem, src, dst string, a Asset, allowCurrentOwner bool) error {
	b, err := f.ReadFile(src)
	if err != nil {
		return errors.New("release_read_failed")
	}
	if old, e := f.ReadFile(dst); e == nil {
		if info, statErr := f.Lstat(dst); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(a.Mode) || (!allowCurrentOwner && !ownerIsRoot(info)) {
			return errDrift
		}
		s := sha256.Sum256(old)
		if int64(len(old)) != a.Size || hex.EncodeToString(s[:]) != a.Digest {
			return errDrift
		}
		return nil
	} else if !errors.Is(e, fs.ErrNotExist) {
		return errDrift
	}
	if err := checkParents(f, filepath.Dir(dst)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("fixed_parent_unsafe")
	}
	if err := f.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return errors.New("fixed_parent_unavailable")
	}
	if err := checkParents(f, filepath.Dir(dst)); err != nil {
		return errors.New("fixed_parent_unsafe")
	}
	if err := writeVerified(f, dst+".new", b, a, allowCurrentOwner); err != nil {
		return err
	}
	if err := f.Rename(dst+".new", dst); err != nil {
		return errors.New("fixed_activate_failed")
	}
	return nil
}

func ownerIsRoot(info fs.FileInfo) bool {
	sys, ok := info.Sys().(*syscall.Stat_t)
	return ok && sys.Uid == 0
}

func verifyRelease(f FileSystem, root string, m Manifest, allowCurrentOwner bool) error {
	manifestBytes, err := f.ReadFile(filepath.Join(root, releaseManifestName))
	if err != nil || sha256Hex(manifestBytes) != manifestHash(m) {
		return errDrift
	}
	descriptorBytes, err := f.ReadFile(filepath.Join(root, releaseDescriptorName))
	if err != nil {
		return errDrift
	}
	var d ReleaseDescriptor
	if json.Unmarshal(descriptorBytes, &d) != nil || validateDescriptor(d, m) != nil {
		return errDrift
	}
	for _, a := range m.Assets {
		if err := verifyAsset(f, filepath.Join(root, filepath.Base(a.Destination)), a, allowCurrentOwner); err != nil {
			return errDrift
		}
	}
	return nil
}

func loadCommittedReleaseIdentity(f FileSystem, root, pointer string) (ReleaseIdentity, error) {
	if pointer == "" || filepath.Base(pointer) != pointer || strings.Contains(pointer, ".") && !strings.HasPrefix(pointer, "shipmates-") {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	release := rooted(root, filepath.Join(installRoot, "releases", pointer))
	manifestBytes, err := f.ReadFile(filepath.Join(release, releaseManifestName))
	if err != nil {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	descriptorBytes, err := f.ReadFile(filepath.Join(release, releaseDescriptorName))
	if err != nil {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	var m Manifest
	var d ReleaseDescriptor
	if json.Unmarshal(manifestBytes, &m) != nil || json.Unmarshal(descriptorBytes, &d) != nil || m.Release != pointer || validateDescriptor(d, m) != nil {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	if sha256Hex(manifestBytes) != manifestHash(m) || sha256Hex(descriptorBytes) != descriptorHash(d) {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	if err := verifyRelease(f, release, m, root != "/"); err != nil {
		return ReleaseIdentity{}, errAdministratorDrift
	}
	return ReleaseIdentity{Descriptor: d, Manifest: m, Pointer: pointer, Inventory: inventoryForManifest(m)}, nil
}

func verifyFixed(f FileSystem, root, release string, m Manifest, allowCurrentOwner bool) error {
	for _, a := range m.Assets {
		if err := verifyAsset(f, rooted(root, a.Destination), a, allowCurrentOwner); err != nil {
			return errDrift
		}
	}
	return nil
}

func verifyAsset(f FileSystem, p string, a Asset, allowCurrentOwner bool) error {
	info, err := f.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fs.FileMode(a.Mode) {
		return errDrift
	}
	if !allowCurrentOwner && !ownerIsRoot(info) {
		return errDrift
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return errDrift
	}
	b, err := f.ReadFile(p)
	if err != nil {
		return errDrift
	}
	s := sha256.Sum256(b)
	if int64(len(b)) != a.Size || hex.EncodeToString(s[:]) != a.Digest {
		return errDrift
	}
	return nil
}

func writeJournal(f FileSystem, root string, r Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p := rooted(root, journalPath)
	if err := checkParents(f, filepath.Dir(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.New("journal_parent_unsafe")
	}
	if err := f.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return errors.New("journal_parent_unavailable")
	}
	if err := checkParents(f, filepath.Dir(p)); err != nil {
		return errors.New("journal_parent_unsafe")
	}
	fd, err := f.OpenFile(p+".new", os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("journal_write_failed")
	}
	if _, err = io.Copy(fd, strings.NewReader(string(b))); err != nil {
		fd.Close()
		return errors.New("journal_write_failed")
	}
	if err = fd.Sync(); err != nil {
		fd.Close()
		return errors.New("journal_sync_failed")
	}
	fd.Close()
	if err = f.Rename(p+".new", p); err != nil {
		return errors.New("journal_commit_failed")
	}
	return f.SyncDir(filepath.Dir(p))
}

func writeAtomicText(f FileSystem, p, value string, mode fs.FileMode) error {
	if err := f.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	tmp := p + ".new"
	if _, e := f.Lstat(tmp); e == nil {
		return errAdministratorDrift
	} else if !errors.Is(e, fs.ErrNotExist) {
		return errAdministratorDrift
	}
	fd, err := f.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(fd, value+"\n"); err != nil {
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

func checkParents(f FileSystem, p string) error {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return errors.New("relative_parent")
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := f.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("unsafe_parent")
		}
	}
	return nil
}

func rooted(root, absolute string) string {
	return filepath.Join(root, strings.TrimPrefix(absolute, string(filepath.Separator)))
}
