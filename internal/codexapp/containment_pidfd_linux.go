//go:build linux

package codexapp

// extractContainmentPidfd exposes the launcher-owned pidfd that the Linux
// containment path opens for its child. It lives alongside the containment
// struct so the adapter can consume it without directly touching the
// unexported field from another OS-agnostic file.
func extractContainmentPidfd(c *ExecutionContainment) int {
	if c == nil {
		return 0
	}
	return c.fd
}
