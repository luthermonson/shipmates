package ship

import "fmt"

// systemdUnit renders the systemd *user* unit that supervises the ship.
//
// It is deliberately untagged (no //go:build) so it compiles and is unit
// tested on every OS, including the Windows dev box and CI — install_linux.go
// is the only file that actually shells out to systemctl.
//
// pathEnv is baked in as Environment=PATH=... because a systemd --user unit
// does NOT inherit the interactive login shell's environment: without this the
// supervised `ship serve` may fail to find claude/git on PATH. Baking it into
// the unit is durable across manager restarts, unlike `systemctl --user
// import-environment`, which is ephemeral.
func systemdUnit(exePath, logPath, pathEnv string) string {
	return fmt.Sprintf(`[Unit]
Description=Shipmates ship supervisor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s ship serve
Restart=always
RestartSec=2
Environment=PATH=%s
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, exePath, pathEnv, logPath, logPath)
}
