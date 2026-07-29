//go:build !windows && !darwin

package ship

// Install is unimplemented on this platform. On Linux, run `shipmates ship
// serve` from a systemd *user* unit (`systemctl --user`) so the supervisor
// inherits the login environment. Note that such a unit needs `loginctl
// enable-linger <user>` to survive logout and to start at boot — the Linux
// analogue of what InstallOptions.Unattended buys on Windows.
func Install(InstallOptions) error { return ErrUnsupported }

// Uninstall is unimplemented on this platform.
func Uninstall() error { return ErrUnsupported }
