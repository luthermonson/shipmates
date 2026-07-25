//go:build windows

package project

import (
	"fmt"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
)

// policyLockName is the shared policy synchronization object inside
// .shipmates. Keep it identical to internal/policy/loader_windows.go: policy
// snapshot readers take this file's byte range shared, mutations take it
// exclusive, and a mismatch would silently remove all cross-process
// exclusion.
const policyLockName = ".policy.lock"

// PolicyWriteLock is the exclusive side of the policy loader's .shipmates
// lock. Keep it held across the complete policy mutation, including any
// rollback which can restore a previously visible policy name.
//
// Windows cannot take a byte-range lock on a directory handle the way unix
// flocks a directory descriptor, so the lock object is a zero-length file
// inside .shipmates rather than the directory itself. The directory chain is
// still opened symlink-safely and held open for the lifetime of the lock, so
// the .shipmates a mutation writes into is provably the one that was
// validated: handles are opened without FILE_SHARE_DELETE, which makes the
// verified directory un-renameable and un-deletable while the lock is held.
type PolicyWriteLock struct {
	dir  *winsec.DirChain
	lock windows.Handle
}

// AcquirePolicyWriteLock safely opens root/.shipmates without following
// either component and blocks until all policy snapshot readers and other
// writers have left their capture intervals.
func AcquirePolicyWriteLock(projectRoot string) (*PolicyWriteLock, error) {
	abs, err := filepath.Abs(projectRoot)
	if err != nil || !filepath.IsAbs(abs) || filepath.Clean(abs) != abs {
		return nil, fmt.Errorf("policy mutation root cannot be resolved safely")
	}
	root, err := winsec.OpenDirChain(abs)
	if err != nil {
		return nil, fmt.Errorf("policy mutation root cannot be opened safely")
	}
	// root.Path is derived from the pinned handle, so descending from it
	// cannot be redirected by a component swapped in after validation.
	dir, err := winsec.OpenDirChain(root.Path, Dir)
	root.Close()
	if err != nil {
		return nil, fmt.Errorf("policy directory cannot be opened safely for mutation")
	}
	lock, err := winsec.OpenLockFile(dir.Path, policyLockName)
	if err != nil {
		dir.Close()
		return nil, fmt.Errorf("policy directory cannot be locked safely for mutation")
	}
	if err := winsec.Lock(lock, true, true); err != nil {
		windows.CloseHandle(lock)
		dir.Close()
		return nil, fmt.Errorf("policy directory cannot be locked safely for mutation")
	}
	return &PolicyWriteLock{dir: dir, lock: lock}, nil
}

// Close releases the exclusive range and every handle the lock pinned. It is
// safe to call more than once, and closing the handle would release the lock
// even if UnlockFileEx were skipped, so an unwinding panic cannot strand it.
func (l *PolicyWriteLock) Close() error {
	if l == nil {
		return nil
	}
	var first error
	if l.lock != windows.InvalidHandle && l.lock != 0 {
		if err := winsec.Unlock(l.lock); err != nil {
			first = fmt.Errorf("policy mutation lock cannot be released safely")
		}
		if err := windows.CloseHandle(l.lock); err != nil && first == nil {
			first = fmt.Errorf("policy mutation lock cannot be closed safely")
		}
		l.lock = windows.InvalidHandle
	}
	if l.dir != nil {
		l.dir.Close()
		l.dir = nil
	}
	return first
}
