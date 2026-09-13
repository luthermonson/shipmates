package doctor

import (
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func TestCheckShipSessionFilesAbsent(t *testing.T) {
	_, home := isolate(t)
	e := baseEnv(home)
	for _, r := range checkShipSessionFiles(e) {
		if r.Status != OK {
			t.Fatalf("absent session file %s should be OK, got %v", r.Name, r.Status)
		}
	}
}

func TestCheckShipSessionFilesPresent(t *testing.T) {
	_, home := isolate(t)
	// project.PortFile() etc. are cwd-relative; write one so it is "present".
	writeFile(t, filepath.Join(".", project.PortFile()), "5000\n")
	e := baseEnv(home)
	got := findResult(t, checkShipSessionFiles(e), "server.port")
	// On the linux GOOS seam with a 0644 file, this should Warn about perms;
	// the pure-mode matrix is covered by TestSessionFilePerm.
	if got.Status != Warn {
		t.Fatalf("present 0644 server.port on linux should Warn, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckCaptainHealth(t *testing.T) {
	_, home := isolate(t)

	// No port file -> informational OK, never Fail.
	e := baseEnv(home)
	if got := findResult(t, checkCaptainHealth(e), "captain /health"); got.Status != OK {
		t.Fatalf("no port file should be OK, got %v", got.Status)
	}

	// Port file present, probe says running -> OK.
	writeFile(t, filepath.Join(".", project.PortFile()), "5000\n")
	e.HealthProbe = func(port int, _ string) bool { return port == 5000 }
	if got := findResult(t, checkCaptainHealth(e), "captain /health"); got.Status != OK {
		t.Fatalf("running captain should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// Port present, probe says not running -> still OK (informational), never Fail.
	e.HealthProbe = func(int, string) bool { return false }
	got := findResult(t, checkCaptainHealth(e), "captain /health")
	if got.Status == Fail {
		t.Fatalf("not-running captain must never Fail, got %v", got.Status)
	}

	// Corrupt (non-numeric) port -> Warn.
	writeFile(t, filepath.Join(".", project.PortFile()), "not-a-port\n")
	if got := findResult(t, checkCaptainHealth(e), "captain /health"); got.Status != Warn {
		t.Fatalf("corrupt port file should Warn, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckSupervisorWiring(t *testing.T) {
	_, home := isolate(t)
	e := baseEnv(home)
	e.GOOS = "linux"
	e.Supervisor = func(string) SupervisorProbe { return SupervisorProbe{Supported: true, Out: "enabled\n"} }
	if got := findResult(t, checkSupervisor(e), "supervisor (ship)"); got.Status != OK {
		t.Fatalf("enabled supervisor should be OK, got %v (%s)", got.Status, got.Detail)
	}
}
