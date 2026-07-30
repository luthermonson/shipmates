//go:build windows

package project

import (
	"errors"
	"os"

	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
)

// ErrNotPrivate reports that a filesystem object is reachable by somebody other
// than its owner, or is not the kind of object it was required to be.
var ErrNotPrivate = errors.New("object is not private to its owner")

// createPrivateTemp creates a uniquely named temporary file in dir that no
// account other than the owner and LOCAL SYSTEM can read or write, and returns
// it open for writing.
//
// The unix implementation is os.CreateTemp plus Chmod(0600). Chmod is not the
// Windows equivalent of anything useful: Go maps it onto
// FILE_ATTRIBUTE_READONLY, so Chmod(0600) is a no-op and os.Stat reports the
// synthesized 0666 for the result. What actually decides who may open the file
// is its DACL, which a newly created file inherits from its parent directory.
//
// So the file is created, closed, and reopened through winsec.Open asking for
// READ_CONTROL|WRITE_DAC — os.CreateTemp requests neither, and SetSecurityInfo
// on a handle without WRITE_DAC fails with ERROR_ACCESS_DENIED — and then given
// a protected DACL naming only the process user and LOCAL SYSTEM. winsec writes
// that DACL and reads it back before returning, so a file this function hands
// out has had its permissions proven, not assumed. Reopening is also where the
// reparse-point, directory, and hard-link refusals in winsec.Open apply.
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	staged, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	name := staged.Name()
	if err := staged.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	h, _, err := winsec.Open(name, false,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.OPEN_EXISTING)
	if err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	f := os.NewFile(uintptr(h), name)
	if err := winsec.PrivateDACL(windows.Handle(f.Fd()), false); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return nil, err
	}
	return f, nil
}

// VerifyPrivateFile requires path to name an existing regular file — not a
// reparse point, not a directory, not hard-linked elsewhere — whose DACL grants
// full control to the process user and LOCAL SYSTEM and to nobody else.
//
// This is the Windows counterpart of the unix `perm&0o077 == 0` check. It is not
// the same statement, and the difference is worth being precise about: the unix
// check reads nine bits and concludes nothing about ACLs, while this one
// enumerates every ACE and refuses any trustee it does not recognize. It is
// therefore strictly the stronger of the two on its own platform, but it says
// nothing about *inherited* permissive entries surviving elsewhere in the tree —
// there are none, because the DACL it demands is protected, which severs
// inheritance.
func VerifyPrivateFile(path string) error {
	h, _, err := winsec.Open(path, false, windows.GENERIC_READ|windows.READ_CONTROL, windows.OPEN_EXISTING)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := winsec.VerifyPrivateDACL(h, false); err != nil {
		return errors.Join(ErrNotPrivate, err)
	}
	return nil
}

// VerifyPrivateDir is VerifyPrivateFile for a directory. Containers carry the
// inheritable form of the same DACL, so their children are private from birth.
func VerifyPrivateDir(path string) error {
	h, _, err := winsec.Open(path, true, windows.GENERIC_READ|windows.READ_CONTROL, windows.OPEN_EXISTING)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := winsec.VerifyPrivateDACL(h, true); err != nil {
		return errors.Join(ErrNotPrivate, err)
	}
	return nil
}
