//go:build windows

package project

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows byte-range locks are mandatory, not advisory like flock: a locked
// range is unreadable through every other handle, including handles owned by
// the process holding the lock. Locking the start of the file would therefore
// make the PID line the holder writes into that same file unreadable to
// diagnostics, to external tooling, and to stale-PID reclamation. Lock a single
// sentinel byte far past any payload instead, so ownership is still exclusive
// while the bytes stay readable — the advisory semantics the callers expect.
// The offset is well beyond the length of a PID line and is never written to,
// so no reader can ever collide with it.
const (
	dispatchLockOffsetLow  = 0
	dispatchLockOffsetHigh = 0x4000_0000 // byte 2^62 of the lock file
	dispatchLockBytes      = 1
)

func dispatchLockRange() windows.Overlapped {
	return windows.Overlapped{Offset: dispatchLockOffsetLow, OffsetHigh: dispatchLockOffsetHigh}
}

// openDispatchLockFile opens the lock file with FILE_SHARE_DELETE, which
// os.OpenFile omits on Windows. Without it the release path could not unlink
// the lock file while ownership is still held, and would have to drop the lock
// first — a window in which another process can acquire the lock and then have
// its lock path removed by us. Sharing delete keeps the unix ordering intact.
func openDispatchLockFile(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

func tryDispatchFileLock(f *os.File) (bool, error) {
	const lockfileFailImmediately = 0x1
	const lockfileExclusiveLock = 0x2
	overlapped := dispatchLockRange()
	err := windows.LockFileEx(windows.Handle(f.Fd()), lockfileFailImmediately|lockfileExclusiveLock, 0, dispatchLockBytes, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	} // ERROR_LOCK_VIOLATION
	return err == nil, err
}
func unlockDispatchFile(f *os.File) error {
	overlapped := dispatchLockRange()
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, dispatchLockBytes, 0, &overlapped)
}
func dispatchProcessAlive(pid int) (bool, error) {
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	} // ERROR_INVALID_PARAMETER
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true, nil
	} // ERROR_ACCESS_DENIED
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false, err
	}
	return exitCode == 259, nil // STILL_ACTIVE
}
