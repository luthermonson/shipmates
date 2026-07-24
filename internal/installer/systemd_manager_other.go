//go:build unix && !linux

package installer

// productionSystemdManager on non-Linux unix (macOS) is the fail-closed
// unavailable manager: there is no systemd, so every qualifier-state query
// refuses and destructive operations stay fenced. This mirrors
// NewQualifierFence's nil-manager default and exists so fence.go
// (//go:build unix) compiles on darwin.
type productionSystemdManager = unavailableSystemdManager
