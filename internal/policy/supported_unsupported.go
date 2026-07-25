//go:build !unix

package policy

// SecureLoadSupported reports whether Load can capture a policy snapshot on
// this platform. See supported_unix.go for the contract.
func SecureLoadSupported() bool { return false }
