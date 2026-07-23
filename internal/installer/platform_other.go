//go:build !linux

package installer

import "errors"

func DetectPlatform() (CapabilitySnapshot, error) {
	return CapabilitySnapshot{}, errors.New("linux_required")
}
