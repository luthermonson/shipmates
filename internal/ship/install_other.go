//go:build !windows && !darwin

package ship

// Install is unimplemented on this platform. On Linux, run `shipmates ship
// serve` from a systemd *user* unit (`systemctl --user`) so the supervisor
// inherits the login environment.
func Install() error { return ErrUnsupported }

// Uninstall is unimplemented on this platform.
func Uninstall() error { return ErrUnsupported }
