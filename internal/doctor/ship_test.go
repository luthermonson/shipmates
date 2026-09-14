package doctor

import (
	"bytes"
	"path/filepath"
	"strings"
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

// TestFullOutputNeverLeaksCaptainToken is the N4 no-leak assertion for the
// captain bearer-token path: with a running captain and a token on disk,
// checkCaptainHealth reads that token and hands it to the /health probe, so a
// careless implementation could echo it into a Result. The full doctor output
// (every Result, and the rendered text) must contain the token nowhere.
func TestFullOutputNeverLeaksCaptainToken(t *testing.T) {
	_, home := isolate(t)

	const token = "captain-bearer-secret-DO-NOT-LEAK-0123456789abcdef"

	// Present a running captain so the bearer-token path is actually exercised.
	writeFile(t, filepath.Join(".", project.PortFile()), "5000\n")
	writeFile(t, filepath.Join(".", project.TokenFile()), token+"\n")

	var probed string
	e := baseEnv(home)
	e.HealthProbe = func(port int, tok string) bool {
		probed = tok
		return port == 5000
	}

	results := Run(e)

	// Sanity: the token really reached the probe, so the assertions below are
	// not vacuous.
	if probed != token {
		t.Fatalf("captain token did not reach the health probe (got %q) — the no-leak check would be vacuous", probed)
	}
	// It must appear in no Result field (scanning ALL results, not just one)...
	if containsSecretIn(results, token) {
		t.Fatalf("captain token present in a doctor Result field")
	}
	// ...and nowhere in the fully rendered output.
	var buf bytes.Buffer
	Render(&buf, results)
	if strings.Contains(buf.String(), token) {
		t.Fatalf("captain token leaked into rendered doctor output:\n%s", buf.String())
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
