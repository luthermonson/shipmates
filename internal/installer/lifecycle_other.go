//go:build !linux

package installer

import "errors"

type FenceLease struct{}

func acquireQualifierLifecycleLease(string, bool) (*FenceLease, error) {
	return nil, errors.New("qualifier_lock_unsupported")
}
func AcquireQualifierLifecycleLease(string, bool) (*FenceLease, error) {
	return acquireQualifierLifecycleLease("", false)
}
func (*FenceLease) Recheck() error { return errors.New("qualifier_lock_unsupported") }
func (*FenceLease) Close() error   { return nil }
