//go:build !unix

package project

import "fmt"

// PolicyWriteLock exists so lifecycle code has one fail-closed platform
// contract. Secure policy loading is also unsupported on these platforms.
type PolicyWriteLock struct{}

func AcquirePolicyWriteLock(string) (*PolicyWriteLock, error) {
	return nil, fmt.Errorf("secure policy mutation locking is unsupported on this platform")
}

func (l *PolicyWriteLock) Close() error { return nil }
