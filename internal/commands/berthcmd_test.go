package commands

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/berth"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// berthRepo makes the cwd a usable git repo with a shipmates.yaml carrying the
// given crew block, and returns the repo path. Berths are git worktrees, so
// every wiring test needs a real repo — there is no fake seam worth having
// here: the whole point of the guardrails is that git says no.
func berthRepo(t *testing.T, crew string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	run("update-ref", "refs/remotes/origin/main", "HEAD")

	body := "sessionPrefix: demo\n"
	if crew != "" {
		body += "crew:\n" + crew
	}
	if err := os.WriteFile(filepath.Join(dir, project.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runCLI drives one command through urfave/cli exactly as main.go does, so the
// test exercises flag parsing and the registered command tree rather than a
// hand-called action.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := &cli.Command{
		Name:      "shipmates",
		Writer:    &stdout,
		ErrWriter: &stderr,
		Commands:  []*cli.Command{Berth()},
	}
	err := cmd.Run(context.Background(), append([]string{"shipmates"}, args...))
	return stdout.String(), stderr.String(), err
}

// TestBerthEnsureProvisionsAndListsAndRemoves is the end-to-end operator loop
// through the real command tree: provision a berth from config, see it listed,
// tear it down.
func TestBerthEnsureProvisionsAndListsAndRemoves(t *testing.T) {
	repo := berthRepo(t, "  skipper:\n    berth: auto\n")

	stdout, _, err := runCLI(t, "berth", "ensure", "skipper")
	if err != nil {
		t.Fatalf("berth ensure: %v", err)
	}
	path := strings.TrimSpace(stdout)
	if want := filepath.Join(repo, berth.Dir("skipper")); path != want {
		t.Fatalf("berth ensure printed %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatalf("berth was not provisioned: %v", err)
	}

	stdout, _, err = runCLI(t, "berth", "list")
	if err != nil {
		t.Fatalf("berth list: %v", err)
	}
	if !strings.Contains(stdout, "skipper") || !strings.Contains(stdout, "clean") {
		t.Errorf("berth list = %q, want a clean skipper row", stdout)
	}

	if _, _, err = runCLI(t, "berth", "remove", "skipper"); err != nil {
		t.Fatalf("berth remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("berth survived removal: %v", err)
	}
}

// TestBerthEnsureHonorsPolicyOff: the fleet default provisions nothing and says
// so on stderr, leaving stdout empty so a shell caller can branch on it.
func TestBerthEnsureHonorsPolicyOff(t *testing.T) {
	berthRepo(t, "  backend:\n    berth: off\n")

	stdout, stderr, err := runCLI(t, "berth", "ensure", "backend")
	if err != nil {
		t.Fatalf("berth ensure: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("berth ensure (off) printed a path: %q", stdout)
	}
	if !strings.Contains(stderr, "no berth") {
		t.Errorf("berth ensure (off) stderr = %q", stderr)
	}
	if _, err := os.Stat(berth.Dir("backend")); !os.IsNotExist(err) {
		t.Errorf("berth: off provisioned a worktree: %v", err)
	}
}

// TestBerthRemoveRefusesDirtyWithoutForce mirrors the package-level guardrail
// through the CLI, including the --force escape hatch.
func TestBerthRemoveRefusesDirtyWithoutForce(t *testing.T) {
	berthRepo(t, "  skipper:\n    berth: auto\n")
	stdout, _, err := runCLI(t, "berth", "ensure", "skipper")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(stdout)
	if err := os.WriteFile(filepath.Join(path, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "berth", "remove", "skipper"); err == nil {
		t.Fatal("berth remove accepted a dirty berth without --force")
	}
	if _, _, err := runCLI(t, "berth", "remove", "skipper", "--force"); err != nil {
		t.Fatalf("berth remove --force: %v", err)
	}
}

// TestBerthPathCreatesNothing: `path` answers "where will this mate work?"
// without side effects, which is what makes it safe to call from a prompt.
func TestBerthPathCreatesNothing(t *testing.T) {
	repo := berthRepo(t, "  skipper:\n    berth: auto\n  tester:\n    cwd: sandbox\n")

	stdout, _, err := runCLI(t, "berth", "path", "skipper")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, berth.Dir("skipper")); strings.TrimSpace(stdout) != want {
		t.Errorf("berth path skipper = %q, want %q", strings.TrimSpace(stdout), want)
	}
	if _, err := os.Stat(berth.Dir("skipper")); !os.IsNotExist(err) {
		t.Error("berth path provisioned a worktree")
	}

	stdout, _, err = runCLI(t, "berth", "path", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, "sandbox"); strings.TrimSpace(stdout) != want {
		t.Errorf("berth path tester = %q, want the resolved cwd override %q", strings.TrimSpace(stdout), want)
	}

	stdout, _, err = runCLI(t, "berth", "path", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout); got != repo {
		t.Errorf("berth path backend = %q, want the project root %q", got, repo)
	}
}

func TestBerthRejectsAnIllegalPersonaName(t *testing.T) {
	berthRepo(t, "")
	for _, sub := range []string{"ensure", "path", "remove"} {
		if _, _, err := runCLI(t, "berth", sub, "../escape"); err == nil {
			t.Errorf("berth %s accepted an illegal persona name", sub)
		}
		if _, _, err := runCLI(t, "berth", sub); err == nil {
			t.Errorf("berth %s accepted a missing persona argument", sub)
		}
	}
}

// enterBerth provisions a berth and chdirs into it, returning its path. This
// is the R1a scenario: an agent working in its own berth reaches for a
// manifest-mutating command.
func enterBerth(t *testing.T, persona string) string {
	t.Helper()
	path, err := berth.Ensure(persona, berth.PolicyAuto)
	if err != nil {
		t.Fatalf("provision berth: %v", err)
	}
	t.Chdir(path)
	return path
}

// TestManifestCommandsRefuseToRunFromABerth is guardrail R1a. `update` in
// divergent berths is the one action that genuinely fractures the tracked
// .shipmates/manifest.json, and add/remove/init write it too.
func TestManifestCommandsRefuseToRunFromABerth(t *testing.T) {
	berthRepo(t, "  skipper:\n    berth: auto\n")
	enterBerth(t, "skipper")

	cat := lifecycleCatalog("role", "")
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"add", func() error { return addPersona(cat, "security") }},
		{"update", func() error { return runUpdate(cat, "", "") }},
		{"remove", func() error { return runRemove("security", false, false) }},
	} {
		err := tc.call()
		if err == nil {
			t.Errorf("%s ran from inside a berth, want refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "repo root") || !strings.Contains(err.Error(), "skipper") {
			t.Errorf("%s refusal does not explain itself: %v", tc.name, err)
		}
	}
}

// TestManifestCommandsRunAtTheRoot is the other half of R1a: the refusal is
// scoped to berths and does not fire in a normal checkout.
func TestManifestCommandsRunAtTheRoot(t *testing.T) {
	skipIfNoPolicyLock(t)
	berthRepo(t, "  skipper:\n    berth: auto\n")
	if _, err := berth.Ensure("skipper", berth.PolicyAuto); err != nil {
		t.Fatal(err)
	}
	if err := addPersona(lifecycleCatalog("role", ""), "security"); err != nil {
		t.Fatalf("add at the repo root was refused: %v", err)
	}
}

// TestRemoveTearsDownThePersonaBerth wires `shipmates remove` to the berth
// lifecycle: removing the persona removes its home, and a dirty home refuses
// until --force.
func TestRemoveTearsDownThePersonaBerth(t *testing.T) {
	skipIfNoPolicyLock(t)
	berthRepo(t, "  security:\n    berth: auto\n")
	if err := addPersona(lifecycleCatalog("role", ""), "security"); err != nil {
		t.Fatal(err)
	}
	path, err := berth.Ensure("security", berth.PolicyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dirty berth: the persona artifacts are gone (that part succeeded) but the
	// operator is told the berth was kept.
	err = runRemove("security", false, false)
	if err == nil || !strings.Contains(err.Error(), "remove berth") {
		t.Fatalf("remove on a dirty berth = %v, want a berth refusal", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refused removal deleted the berth anyway: %v", err)
	}

	// --force completes the teardown.
	if err := berth.Remove("security", true); err != nil {
		t.Fatalf("forced berth removal: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("berth survived forced removal: %v", err)
	}
}

// TestRemoveOfAPersonaWithoutABerthIsUnaffected: the teardown step must be a
// no-op for the overwhelmingly common berth-less persona.
func TestRemoveOfAPersonaWithoutABerthIsUnaffected(t *testing.T) {
	skipIfNoPolicyLock(t)
	berthRepo(t, "")
	if err := addPersona(lifecycleCatalog("role", ""), "security"); err != nil {
		t.Fatal(err)
	}
	if err := runRemove("security", false, false); err != nil {
		t.Fatalf("runRemove: %v", err)
	}
}

// TestAskLandsInTheBerth is the spawn seam: the working directory shipmates
// hands the runtime is the persona's berth, while the project root it reports
// stays canonical — memory, policy and the manifest must not move.
func TestAskLandsInTheBerth(t *testing.T) {
	repo := berthRepo(t, "  security:\n    berth: auto\n")
	installCodexPersona(t, "security")
	rt := newFakeRuntime(turnScript("berthed answer"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "review this", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.startCalls) != 1 {
		t.Fatalf("StartSession calls = %d, want 1", len(rt.startCalls))
	}
	spec := rt.startCalls[0]
	want := filepath.Join(repo, berth.Dir("security"))
	if spec.WorkingDir != want {
		t.Errorf("SessionSpec.WorkingDir = %q, want the berth %q", spec.WorkingDir, want)
	}
	if spec.ProjectDir != repo {
		t.Errorf("SessionSpec.ProjectDir = %q, want the canonical root %q", spec.ProjectDir, repo)
	}
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Errorf("ask did not provision the berth: %v", err)
	}
}

// TestAskWithoutABerthKeepsTodaysBehaviour: no berth configured means no
// WorkingDir override and a fingerprint identical to the pre-berth one, so
// nothing about an existing project changes.
func TestAskWithoutABerthKeepsTodaysBehaviour(t *testing.T) {
	berthRepo(t, "")
	installCodexPersona(t, "security")
	cfg, err := project.ResolvePersonaConfig("security")
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "claude", "security", "prior-session-id", cfg.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("resumed"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "again", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.resumeCalls) != 1 || rt.resumeCalls[0] != "prior-session-id" {
		t.Errorf("resume calls = %v; a berth-less project must still resume on the old fingerprint", rt.resumeCalls)
	}
	if len(rt.startCalls) != 0 {
		t.Errorf("berth-less project auto-freshed: %d StartSession calls", len(rt.startCalls))
	}
}

// TestGainingABerthFreshensRatherThanMigrates is the "berth only at session
// creation, never mid-session" guardrail in its modern spelling: a persona
// that gains a berth gets a NEW session created inside it, instead of an old
// session resumed into a directory it was never created in.
func TestGainingABerthFreshensRatherThanMigrates(t *testing.T) {
	repo := berthRepo(t, "  security:\n    berth: auto\n")
	installCodexPersona(t, "security")
	cfg, err := project.ResolvePersonaConfig("security")
	if err != nil {
		t.Fatal(err)
	}
	// A pre-berth session marker: fingerprinted without any working directory.
	if err := project.WriteBackendSessionMeta("security", "claude", "security", "pre-berth-session", cfg.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("fresh in the berth"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "go", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.resumeCalls) != 0 {
		t.Errorf("resumed %v into a berth it was not created in", rt.resumeCalls)
	}
	if len(rt.startCalls) != 1 {
		t.Fatalf("StartSession calls = %d, want 1 (a fresh session in the berth)", len(rt.startCalls))
	}
	if want := filepath.Join(repo, berth.Dir("security")); rt.startCalls[0].WorkingDir != want {
		t.Errorf("fresh session WorkingDir = %q, want %q", rt.startCalls[0].WorkingDir, want)
	}
	// …and the new marker records the berthed fingerprint, so the next turn
	// resumes rather than freshening again.
	meta, ok := project.ReadBackendSessionMeta("security", "claude")
	if !ok || meta.ConfigHash == cfg.Fingerprint() {
		t.Errorf("session marker was not re-fingerprinted for the berth: %#v", meta)
	}
}
