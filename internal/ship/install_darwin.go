//go:build darwin

package ship

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// label is the launchd job label; the plist file is named after it.
const label = "cc.shipmates.ship"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// Install registers the supervisor as a launchd user agent (runs in the
// user's login session with their environment — the Mac analogue of the
// Windows logon Scheduled Task). KeepAlive makes launchd itself restart the
// supervisor if it dies.
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
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>ship</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, exe, logPath, logPath)

	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	// reload if a previous version was loaded
	_ = exec.Command("launchctl", "unload", path).Run()
	if out, err := exec.Command("launchctl", "load", "-w", path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall unloads the agent and removes the plist.
func Uninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", "-w", path).Run() // best-effort stop
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
