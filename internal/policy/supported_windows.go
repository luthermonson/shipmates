//go:build windows

package policy

// SecureLoadSupported reports whether Load can capture a policy snapshot on
// this platform. See supported_unix.go for the contract.
//
// Windows qualifies: loader_windows.go refuses reparse points on every path
// component, pins each component's handle against replacement, serializes
// against policy mutations with LockFileEx, and revalidates file identity
// (volume serial + file index + size + write/change time) after the read. A
// claude approval on Windows is therefore mediated against real policy rather
// than denied for want of a snapshot.
func SecureLoadSupported() bool { return true }
