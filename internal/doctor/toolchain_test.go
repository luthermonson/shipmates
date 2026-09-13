package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckToolchainClaudeAndGit(t *testing.T) {
	_, home := isolate(t)

	// claude + git present.
	e := baseEnv(home)
	e.LookPath = lookPathFor("claude", "git")
	res := checkToolchain(e)
	if got := findResult(t, res, "claude CLI"); got.Status != OK {
		t.Fatalf("claude present should be OK, got %v", got.Status)
	}
	if got := findResult(t, res, "git"); got.Status != OK {
		t.Fatalf("git present should be OK, got %v", got.Status)
	}

	// claude missing -> Fail (hard dependency); git missing -> Warn.
	e.LookPath = lookPathFor()
	res = checkToolchain(e)
	if got := findResult(t, res, "claude CLI"); got.Status != Fail {
		t.Fatalf("claude missing should be Fail, got %v", got.Status)
	}
	if got := findResult(t, res, "git"); got.Status != Warn {
		t.Fatalf("git missing should be Warn, got %v", got.Status)
	}
}

func TestCheckToolchainBeads(t *testing.T) {
	proj, home := isolate(t)

	// bd missing, Beads not configured -> informational OK.
	e := baseEnv(home)
	e.LookPath = lookPathFor("claude", "git")
	if got := findResult(t, checkToolchain(e), "bd (Beads)"); got.Status != OK {
		t.Fatalf("bd missing + unconfigured should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// bd missing but voyage.tracker: beads configured -> Warn.
	writeFile(t, filepath.Join(proj, "shipmates.yaml"), "voyage:\n  tracker: beads\n")
	if got := findResult(t, checkToolchain(e), "bd (Beads)"); got.Status != Warn {
		t.Fatalf("bd missing + configured should be Warn, got %v (%s)", got.Status, got.Detail)
	}

	// bd present -> OK even when configured.
	e.LookPath = lookPathFor("claude", "git", "bd")
	if got := findResult(t, checkToolchain(e), "bd (Beads)"); got.Status != OK {
		t.Fatalf("bd present should be OK, got %v", got.Status)
	}
}

func TestBeadsConfiguredViaWorkspace(t *testing.T) {
	proj, _ := isolate(t)
	if ok, _ := beadsConfigured(); ok {
		t.Fatal("empty project should not be Beads-configured")
	}
	if err := os.MkdirAll(filepath.Join(proj, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, why := beadsConfigured(); !ok {
		t.Fatalf("a .beads workspace should count as configured (why=%q)", why)
	}
}
