//go:build unix

package project

import (
	"errors"
	"os"
)

// ErrNotPrivate reports that a filesystem object is reachable by somebody other
// than its owner, or is not the kind of object it was required to be.
var ErrNotPrivate = errors.New("object is not private to its owner")

// createPrivateTemp creates a uniquely named temporary file in dir that no
// account other than the owner can read or write, and returns it open for
// writing. Callers stage a durable file here and commit it with DurableRename.
//
// On unix "private" is nine mode bits, so this is os.CreateTemp (which is
// already 0600) plus an explicit Chmod that says so rather than relying on it.
// See private_file_windows.go for the Windows equivalent, which needs a real
// DACL because the mode bits do not exist.
func createPrivateTemp(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		name := f.Name()
		_ = f.Close()
		_ = os.Remove(name)
		return nil, err
	}
	return f, nil
}

// VerifyPrivateFile requires path to name an existing regular file — not a
// symlink, not a directory — that grants no access to group or other.
func VerifyPrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return ErrNotPrivate
	}
	return nil
}

// VerifyPrivateDir is VerifyPrivateFile for a directory.
func VerifyPrivateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o077 != 0 {
		return ErrNotPrivate
	}
	return nil
}
