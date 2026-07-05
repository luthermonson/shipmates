//go:build windows

package ship

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// taskName is the Scheduled Task the supervisor runs under.
const taskName = "ShipmatesShip"

// Install registers the supervisor as a Scheduled Task that runs at the
// user's logon — deliberately NOT a session-0 Windows service: claude needs
// the user's environment, credentials, and profile to spawn mates.
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, ".shipmates", "ship.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	tr := fmt.Sprintf(`"%s" ship serve --log-file "%s"`, exe, logPath)
	if out, err := exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", taskName, "/TR", tr).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// start it now — logon-triggered tasks otherwise wait for the next logon
	if out, err := exec.Command("schtasks", "/Run", "/TN", taskName).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops and removes the Scheduled Task.
func Uninstall() error {
	_ = exec.Command("schtasks", "/End", "/TN", taskName).Run() // best-effort stop
	if out, err := exec.Command("schtasks", "/Delete", "/F", "/TN", taskName).CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks delete: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
