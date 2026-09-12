//go:build linux

package ship

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// unitName is the systemd user unit that supervises the ship.
const unitName = "shipmates-ship.service"

// unitPath returns the path to the per-user systemd unit file.
func unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", unitName), nil
}

// Install registers the supervisor as a systemd *user* unit (runs in the
// user's session with their credentials — the Linux analogue of the macOS
// launchd user agent). Restart=always makes systemd restart the supervisor if
// it dies; WantedBy=default.target starts it with the user session.
//
// Two Linux-specific wrinkles are handled here:
//
//   - A --user unit does NOT inherit the interactive shell's environment, so
//     `ship serve` might not find claude/git on PATH. We bake the install-time
//     PATH into the unit (Environment=PATH=...), which is durable across
//     manager restarts.
//   - Linger (loginctl enable-linger) lets the ship outlive logout. It is a
//     heavier system-level side effect than launchd's, so it is best-effort:
//     failure logs a warning and continues rather than failing Install().
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Create the log dir up front like darwin does. %h in the unit expands to
	// the user's home at runtime; mirror that concrete path here.
	if err := os.MkdirAll(filepath.Join(home, ".shipmates"), 0o755); err != nil {
		return err
	}

	unit := systemdUnit(exe, "%h/.shipmates/ship.log", os.Getenv("PATH"))

	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user enable --now: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Best-effort linger so the ship survives logout. Do NOT fail the install
	// if this doesn't work (no permissions, no loginctl) — just warn.
	uname := ""
	if u, err := user.Current(); err == nil {
		uname = u.Username
	}
	if out, err := exec.Command("loginctl", "enable-linger", uname).CombinedOutput(); err != nil {
		slog.Warn("ship: could not enable linger; the ship will stop when you log out. "+
			"To keep it running, run this manually as an admin",
			"cmd", "loginctl enable-linger "+uname,
			"err", err, "output", strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall disables and stops the unit, removes the unit file, and reloads
// the manager. Linger is left alone — the operator may want it for other
// services.
func Uninstall() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run() // best-effort stop

	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
