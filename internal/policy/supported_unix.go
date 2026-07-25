//go:build unix

package policy

// SecureLoadSupported reports whether Load can capture a policy snapshot on
// this platform. Loading needs openat/O_NOFOLLOW-class primitives to make
// the capture immune to symlink and TOCTOU substitution, so it exists only
// on unix; see loader_unsupported.go for the other half.
//
// Callers that mediate runtime approvals must consult this: with no
// snapshot there is no allow authority, and every request has to be refused
// rather than waved through.
func SecureLoadSupported() bool { return true }
