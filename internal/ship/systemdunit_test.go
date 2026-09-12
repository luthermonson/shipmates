package ship

import (
	"strings"
	"testing"
)

// TestSystemdUnit checks the generated user unit end to end. It has no build
// tag so it runs on every OS, including the Windows dev box and CI, where the
// linux-only install path can't execute.
func TestSystemdUnit(t *testing.T) {
	const (
		exe     = "/usr/local/bin/shipmates"
		logPath = "%h/.shipmates/ship.log"
		pathEnv = "/usr/local/bin:/usr/bin:/bin"
	)
	unit := systemdUnit(exe, logPath, pathEnv)

	want := []string{
		"ExecStart=/usr/local/bin/shipmates ship serve",
		"Restart=always",
		"RestartSec=2",
		"WantedBy=default.target",
		"Environment=PATH=/usr/local/bin:/usr/bin:/bin",
		"StandardOutput=append:%h/.shipmates/ship.log",
		"StandardError=append:%h/.shipmates/ship.log",
	}
	for _, w := range want {
		if !strings.Contains(unit, w) {
			t.Errorf("generated unit missing %q\n---\n%s", w, unit)
		}
	}

	// ExecStart must invoke the ship serve subcommand, not just the exe.
	if !strings.Contains(unit, exe+" ship serve") {
		t.Errorf("ExecStart should run %q with `ship serve`, got:\n%s", exe, unit)
	}

	// Sanity: [Install] section must exist for enable to wire WantedBy.
	if !strings.Contains(unit, "[Install]") {
		t.Errorf("unit missing [Install] section:\n%s", unit)
	}
}
