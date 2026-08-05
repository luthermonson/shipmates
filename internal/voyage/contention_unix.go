//go:build !windows

package voyage

// transientFSContention is Windows-only: POSIX rename(2) serializes
// concurrent replacements in the kernel, so any error here is real.
func transientFSContention(error) bool { return false }
