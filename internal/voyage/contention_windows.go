//go:build windows

package voyage

import (
	"errors"
	"syscall"
)

const (
	errorAccessDenied     = syscall.Errno(5)
	errorSharingViolation = syscall.Errno(32)
)

// transientFSContention reports whether an error is the transient Windows
// face of two processes replacing/reading the same file at once: the loser of
// a MoveFileEx race observes the winner's delete-pending state as
// ERROR_ACCESS_DENIED, and open handles surface ERROR_SHARING_VIOLATION.
// Neither exists on unix, where rename(2) serializes in the kernel.
func transientFSContention(err error) bool {
	return errors.Is(err, errorAccessDenied) || errors.Is(err, errorSharingViolation)
}
