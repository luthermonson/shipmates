//go:build unix

package project

import (
	"os"
	"path/filepath"
)

// DurableRename atomically replaces newpath with oldpath and does not return
// success until the new directory entry is on stable storage. Both paths must
// live in the same directory.
//
// On unix that is two operations, and the second one is the part that is easy
// to forget: rename(2) is atomic with respect to other observers, but the
// directory entry it produces is only metadata in the page cache until the
// containing *directory* is fsynced. ext4's default data=ordered mode does not
// promise otherwise. A crash between the rename and the directory flush can
// therefore lose a marker that was reported as written, which is exactly the
// failure mode the callers of this function exist to prevent.
//
// See durable_rename_windows.go for why the Windows implementation is a single
// call and what it guarantees.
func DurableRename(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	d, err := os.Open(filepath.Clean(filepath.Dir(newpath)))
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
