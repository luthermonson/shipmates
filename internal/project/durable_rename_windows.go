//go:build windows

package project

import (
	"golang.org/x/sys/windows"
)

// DurableRename atomically replaces newpath with oldpath and does not return
// success until the new directory entry is on stable storage. Both paths must
// live in the same directory.
//
// The unix implementation is rename(2) followed by fsync of the containing
// directory. Neither half of that translates:
//
//   - os.Rename on Windows is MoveFileEx without MOVEFILE_WRITE_THROUGH, so it
//     may return before the directory entry has reached the disk.
//   - the directory flush is not merely unnecessary, it is impossible.
//     FlushFileBuffers is the only directory-flush primitive Win32 exposes and
//     NTFS refuses it on a directory handle with ERROR_ACCESS_DENIED. Go
//     surfaces that as `sync <dir>: Access is denied.` Code that ignored the
//     error would silently drop the durability guarantee; code that returned it
//     after a successful rename — as project.WriteLiveContinuityBackendAt did —
//     reports failure for a write that actually landed.
//
// MOVEFILE_WRITE_THROUGH is the primitive that replaces both: it is documented
// to not return until the change has been flushed from the cache to disk, and
// on NTFS the rename is a single logged metadata transaction, so flushing it
// commits the directory entry along with it. Where unix needs two ordered
// operations to make one rename durable, Windows needs one.
//
// MOVEFILE_REPLACE_EXISTING keeps the replacement atomic for observers, which
// is what os.Rename would have given on POSIX. The move must be within one
// volume: MoveFileEx degrades a cross-volume move to copy-then-delete, which is
// neither atomic nor what any caller here wants. Every caller stages its
// temporary file in the destination directory, so that holds.
func DurableRename(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
