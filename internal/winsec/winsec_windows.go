//go:build windows

package winsec

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileAllAccess is the file generic mapping of GENERIC_ALL. Windows maps the
// generic bits of an ACE onto object-specific rights when the ACE is stored,
// so a DACL written asking for GENERIC_ALL reads back carrying this mask
// instead. x/sys/windows does not export FILE_ALL_ACCESS.
const fileAllAccess = uint32(0x001F01FF)

// lockRegion is the byte range every shipmates lock file uses. The file is
// always zero length; a range beyond EOF is still a valid lock target and
// keeps the lock object free of any content that could be misread as data.
const lockRegion = uint32(1)

var (
	// ErrReparsePoint reports that a path component is a symlink, junction, or
	// any other reparse point. It is the Windows analogue of ELOOP from
	// O_NOFOLLOW.
	ErrReparsePoint = errors.New("winsec: path component is a reparse point")
	// ErrWrongType reports a file where a directory was required or the
	// reverse — the analogue of ENOTDIR / EISDIR.
	ErrWrongType = errors.New("winsec: path component has the wrong type")
	// ErrHardLink reports a file with more than one directory entry. A
	// hardlinked policy artifact can be mutated through a name outside the
	// project, so it is refused outright.
	ErrHardLink = errors.New("winsec: file has more than one link")
	// ErrPathMismatch reports that the object behind a handle does not
	// canonicalize back to the path the caller asked for.
	ErrPathMismatch = errors.New("winsec: handle does not name the intended path")
	// ErrBadComponent reports a path component that is not a single literal
	// name.
	ErrBadComponent = errors.New("winsec: unsafe path component")
	// ErrDACL reports that a private DACL could not be written or did not read
	// back as written.
	ErrDACL = errors.New("winsec: private DACL verification failed")
)

// fileBasicInfo mirrors FILE_BASIC_INFO. x/sys/windows exports the
// information class but not the struct. ChangeTime is the closest Windows
// analogue of unix st_ctim: it moves on any metadata change, including the
// ones that leave LastWriteTime alone.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32 // explicit tail padding, the struct is 8-byte aligned
}

// fileIDInfo mirrors FILE_ID_INFO. The 128-bit file id is unique per volume
// even on ReFS, where the 64-bit index in BY_HANDLE_FILE_INFORMATION can
// collide.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// Identity is the Windows analogue of the unix (st_dev, st_ino, st_size,
// st_nlink, st_mode, st_mtim, st_ctim) tuple the policy loader compares
// before and after every read.
//
// (VolumeSerial, Index) identifies the file object the way (dev, ino) does;
// FileID is the wider ReFS-safe form of the same thing and is left zero on
// filesystems that do not implement FileIdInfo. Change is the st_ctim
// analogue. The struct is deliberately comparable so callers use ==.
type Identity struct {
	VolumeSerial   uint64
	Index          uint64
	FileID         [16]byte
	FileIDVolume   uint64
	Size           uint64
	Links          uint32
	Attributes     uint32
	Write          windows.Filetime
	Create         windows.Filetime
	Change         int64
	HasExtendedIDs bool
}

// IsDir reports whether the identity describes a directory.
func (i Identity) IsDir() bool {
	return i.Attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}

// Identify captures the full identity of the object behind h.
func Identify(h windows.Handle) (Identity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return Identity{}, err
	}
	id := Identity{
		VolumeSerial: uint64(info.VolumeSerialNumber),
		Index:        uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		Size:         uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow),
		Links:        info.NumberOfLinks,
		Attributes:   info.FileAttributes,
		Write:        info.LastWriteTime,
		Create:       info.CreationTime,
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		// ChangeTime is the ctime analogue; without it a metadata-only
		// substitution could go unnoticed, so refuse rather than degrade.
		return Identity{}, err
	}
	id.Change = basic.ChangeTime
	var wide fileIDInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&wide)), uint32(unsafe.Sizeof(wide))); err == nil {
		id.FileID = wide.FileID
		id.FileIDVolume = wide.VolumeSerialNumber
		id.HasExtendedIDs = true
	}
	return id, nil
}

// Open opens exactly one filesystem object by fully qualified path.
//
// FILE_FLAG_OPEN_REPARSE_POINT makes the final component open as the reparse
// point itself rather than following it, and the attribute check below then
// refuses it — that is the O_NOFOLLOW equivalent. Ancestors are the caller's
// job: walk one component at a time with OpenDirChain and keep every handle
// open. Handles here are deliberately opened *without* FILE_SHARE_DELETE, so
// an ancestor that has been verified as a real directory cannot be renamed or
// deleted, and therefore cannot be swapped for a link, while the caller
// descends through it.
//
// A directory opened with dir=false is refused by CreateFile itself
// (ERROR_ACCESS_DENIED, for want of FILE_FLAG_BACKUP_SEMANTICS) rather than by
// the type check below, so a caller that needs to tell "it is a directory"
// apart from "permission denied" must follow up with Probe.
func Open(path string, dir bool, access, disposition uint32) (windows.Handle, Identity, error) {
	return OpenShared(path, dir, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition)
}

// OpenShared is Open with the sharing mode named explicitly.
//
// Open's refusal to share delete access is deliberate and load-bearing for
// *directory* handles, where it is what stops a component already proven to be a
// real directory from being renamed away and replaced by a junction. On a leaf
// file it is instead a behavioural difference from unix, where a file can always
// be unlinked while a descriptor is still open. A caller that holds a long-lived
// handle to something a peer may legitimately rotate — an append-only audit log,
// for instance — wants FILE_SHARE_DELETE so that stays true, and loses nothing
// by it: deleting a file grants no read access to its contents, and who may
// delete it is decided by the directory's DACL, not by this sharing mode.
func OpenShared(path string, dir bool, access, share, disposition uint32) (windows.Handle, Identity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, Identity{}, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if dir {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p, access, share, nil, disposition, flags, 0)
	if err != nil {
		return windows.InvalidHandle, Identity{}, err
	}
	id, err := Identify(h)
	if err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, Identity{}, err
	}
	if id.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return windows.InvalidHandle, Identity{}, ErrReparsePoint
	}
	if id.IsDir() != dir {
		windows.CloseHandle(h)
		return windows.InvalidHandle, Identity{}, ErrWrongType
	}
	if !dir && id.Links != 1 {
		windows.CloseHandle(h)
		return windows.InvalidHandle, Identity{}, ErrHardLink
	}
	return h, id, nil
}

// Probe opens path for attribute inspection only, refusing nothing: reparse
// points, directories, and files all come back. It exists so a failed Open
// can be classified the way unix code uses fstatat(AT_SYMLINK_NOFOLLOW) to
// tell "it is a symlink" and "it is a directory" apart from "permission
// denied".
func Probe(path string) (Identity, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Identity{}, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return Identity{}, err
	}
	defer windows.CloseHandle(h)
	return Identify(h)
}

// IsReparsePoint reports whether an identity describes a reparse point.
func (i Identity) IsReparsePoint() bool {
	return i.Attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// FinalPath returns the canonical `\\?\`-prefixed path of the object behind
// h. Windows resolves a whole path in the kernel, so this is how a caller
// asks "which object did I actually get" after the fact.
func FinalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0 /* VOLUME_NAME_DOS */)
	if err != nil {
		return "", err
	}
	if int(n) >= len(buf) {
		buf = make([]uint16, n+1)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
	}
	return windows.UTF16ToString(buf[:n]), nil
}

// VerifyFinalPath requires the object behind h to canonicalize back to want.
// Comparison is case-insensitive because Windows filesystems are.
func VerifyFinalPath(h windows.Handle, want string) error {
	got, err := FinalPath(h)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s != %s", ErrPathMismatch, got, want)
	}
	return nil
}

// Child appends one literal name to a canonical `\\?\` directory path.
//
// It refuses anything that is not a single name, which matters more on
// Windows than on unix: a `\\?\` path is handed to the object manager
// verbatim, so "." and ".." are not resolved and a separator or drive colon
// smuggled into a name would silently change which object is opened.
func Child(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." {
		return "", ErrBadComponent
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return "", ErrBadComponent
	}
	for _, r := range name {
		if r < 0x20 {
			return "", ErrBadComponent
		}
	}
	// Trailing dots and spaces are stripped by Win32 path parsing but not by
	// the `\\?\` form, so refusing them keeps the two spellings equivalent.
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return "", ErrBadComponent
	}
	return strings.TrimSuffix(dir, `\`) + `\` + name, nil
}

// DirChain is a directory path that has been opened one component at a time,
// with every intermediate handle still held.
//
// Windows has no openat, so the two available strategies are (a) open each
// component by handle and verify identity as you descend, or (b) open the
// final object and check GetFinalPathNameByHandle. DirChain does both,
// because on their own neither is sufficient:
//
//   - (a) alone still opens each component by full path, so the kernel
//     re-resolves the ancestors on every call. That is safe here only
//     because the handles are held without FILE_SHARE_DELETE, which makes an
//     already-verified ancestor un-renameable and un-deletable, hence
//     un-swappable. Holding the chain is what supplies openat's "resolve
//     relative to a descriptor I already validated" property.
//   - (b) alone tells you the object's canonical name but not that no
//     component was a reparse point at open time — GetFinalPathNameByHandle
//     reports the *resolved* path, so a followed junction would look
//     perfectly legitimate. FILE_FLAG_OPEN_REPARSE_POINT plus the attribute
//     refusal in Open is what rules that out.
//
// Together: every component is proven to be a real directory (not a link) at
// the moment it is opened, is pinned against replacement for as long as the
// chain lives, and the canonical path of the result is proven to be the one
// that was asked for.
type DirChain struct {
	// Path is the canonical `\\?\` path of the deepest component.
	Path    string
	handles []windows.Handle
}

// OpenDirChain opens root (which may be any fully qualified path spelling)
// and then each component below it. root is canonicalized from its own
// handle, so every path this package derives afterwards is already in the
// form GetFinalPathNameByHandle reports and comparisons cannot be defeated by
// 8.3 short names or case.
func OpenDirChain(root string, components ...string) (*DirChain, error) {
	h, _, err := Open(root, true, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		return nil, err
	}
	canonical, err := FinalPath(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	c := &DirChain{Path: canonical, handles: []windows.Handle{h}}
	for _, name := range components {
		next, err := Child(c.Path, name)
		if err != nil {
			c.Close()
			return nil, err
		}
		nh, _, err := Open(next, true, windows.GENERIC_READ, windows.OPEN_EXISTING)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := VerifyFinalPath(nh, next); err != nil {
			windows.CloseHandle(nh)
			c.Close()
			return nil, err
		}
		c.handles = append(c.handles, nh)
		c.Path = next
	}
	return c, nil
}

// Leaf returns the handle of the deepest component.
func (c *DirChain) Leaf() windows.Handle { return c.handles[len(c.handles)-1] }

// Close releases every handle in the chain, deepest first.
func (c *DirChain) Close() {
	if c == nil {
		return
	}
	for i := len(c.handles) - 1; i >= 0; i-- {
		windows.CloseHandle(c.handles[i])
	}
	c.handles = nil
}

// OpenLockFile opens, creating it if absent, the zero-length lock object
// `dir\name` and hardens it to the current user plus LOCAL SYSTEM.
//
// The handle is opened for read and write because LockFileEx needs write
// access to take LOCKFILE_EXCLUSIVE_LOCK; readers use the same handle rights
// so that both sides agree on one lock object. dir must already be a
// DirChain-verified canonical path.
func OpenLockFile(dir, name string) (windows.Handle, error) {
	path, err := Child(dir, name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, _, err := Open(path, false,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.OPEN_ALWAYS)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := VerifyFinalPath(h, path); err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	// Self-healing: a lock object planted with a permissive ACL is rewritten
	// to the private one before any caller trusts the exclusion it provides.
	if err := PrivateDACL(h, false); err != nil {
		windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

// Lock takes a byte-range lock on h. exclusive selects LOCKFILE_EXCLUSIVE_LOCK
// (the LOCK_EX analogue); otherwise the lock is shared (LOCK_SH). When block
// is false the call fails immediately with ERROR_LOCK_VIOLATION instead of
// waiting, which is the LOCK_NB analogue.
//
// Windows byte-range locks exclude other handles regardless of which process
// owns them, exactly as flock excludes other open file descriptions.
func Lock(h windows.Handle, exclusive, block bool) error {
	flags := uint32(0)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var overlapped windows.Overlapped
	return windows.LockFileEx(h, flags, 0, lockRegion, 0, &overlapped)
}

// Unlock releases the range Lock took. Closing the handle releases it too,
// so callers that only ever close may ignore this.
func Unlock(h windows.Handle) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(h, 0, lockRegion, 0, &overlapped)
}

// PrivateDACL replaces the object's DACL with a protected DACL granting full
// control to the process user and LOCAL SYSTEM and to nobody else. It is the
// Windows equivalent of creating a file 0600 or a directory 0700, and it is
// self-healing: inheritance is severed and any pre-existing grant is dropped.
//
// inherit additionally marks the entries inheritable, for a container whose
// future children must be private too.
//
// h must have been opened with READ_CONTROL|WRITE_DAC.
func PrivateDACL(h windows.Handle, inherit bool) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	inheritance := uint32(0)
	if inherit {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(u.User.Sid)}},
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: inheritance, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(system)}},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return err
	}
	return VerifyPrivateDACL(h, inherit)
}

// VerifyPrivateDACL reads the DACL back and requires it to be exactly what
// PrivateDACL writes: allow-only entries, full control, for the process user
// and LOCAL SYSTEM, and nobody else.
//
// A container written with SUB_CONTAINERS_AND_OBJECTS_INHERIT ends up with
// two entries per trustee rather than one: the effective entry, whose
// GENERIC_ALL has been mapped to FILE_ALL_ACCESS, plus an INHERIT_ONLY entry
// that keeps GENERIC_ALL unmapped so each child maps it against its own
// generic mapping.
func VerifyPrivateDACL(h windows.Handle, inherit bool) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return ErrDACL
	}
	perTrustee := 1
	if inherit {
		perTrustee = 2
	}
	if int(dacl.AceCount) != 2*perTrustee {
		return fmt.Errorf("%w: %d entries", ErrDACL, dacl.AceCount)
	}
	seenUser, seenSystem := 0, 0
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return ErrDACL
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrDACL
		}
		want := fileAllAccess
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			want = uint32(windows.GENERIC_ALL)
		}
		if uint32(ace.Mask) != want {
			return fmt.Errorf("%w: entry %d mask %#08x", ErrDACL, i, uint32(ace.Mask))
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(u.User.Sid):
			seenUser++
		case sid.Equals(system):
			seenSystem++
		default:
			return fmt.Errorf("%w: unexpected trustee %s", ErrDACL, sid)
		}
	}
	if seenUser != perTrustee || seenSystem != perTrustee {
		return ErrDACL
	}
	return nil
}
