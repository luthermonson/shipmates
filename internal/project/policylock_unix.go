//go:build unix

package project

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// PolicyWriteLock is the exclusive side of the policy loader's .shipmates
// directory lock. Keep it held across the complete policy mutation, including
// any rollback which can restore a previously visible policy name.
type PolicyWriteLock struct {
	root int
	dir  int
}

// AcquirePolicyWriteLock safely opens root/.shipmates without following either
// component and blocks until all policy snapshot readers and other writers have
// left their capture intervals.
func AcquirePolicyWriteLock(projectRoot string) (*PolicyWriteLock, error) {
	abs, err := filepath.Abs(projectRoot)
	if err != nil || !filepath.IsAbs(abs) || filepath.Clean(abs) != abs {
		return nil, fmt.Errorf("policy mutation root cannot be resolved safely")
	}
	root, err := unix.Open(abs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("policy mutation root cannot be opened safely")
	}
	dir, err := unix.Openat(root, Dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Close(root)
		return nil, fmt.Errorf("policy directory cannot be opened safely for mutation")
	}
	if err := unix.Flock(dir, unix.LOCK_EX); err != nil {
		_ = unix.Close(dir)
		_ = unix.Close(root)
		return nil, fmt.Errorf("policy directory cannot be locked safely for mutation")
	}
	return &PolicyWriteLock{root: root, dir: dir}, nil
}

func (l *PolicyWriteLock) Close() error {
	if l == nil {
		return nil
	}
	var first error
	if l.dir >= 0 {
		if err := unix.Flock(l.dir, unix.LOCK_UN); err != nil {
			first = fmt.Errorf("policy mutation lock cannot be released safely")
		}
		if err := unix.Close(l.dir); err != nil && first == nil {
			first = fmt.Errorf("policy mutation lock cannot be closed safely")
		}
		l.dir = -1
	}
	if l.root >= 0 {
		if err := unix.Close(l.root); err != nil && first == nil {
			first = fmt.Errorf("policy mutation root cannot be closed safely")
		}
		l.root = -1
	}
	return first
}
