//go:build !windows && !darwin && !linux

package ship

// Install is unimplemented on this platform. Windows (Scheduled Task), macOS
// (launchd user agent), and Linux (systemd --user unit) each have a real
// implementation; anything else returns ErrUnsupported.
func Install() error { return ErrUnsupported }

// Uninstall is unimplemented on this platform.
func Uninstall() error { return ErrUnsupported }
