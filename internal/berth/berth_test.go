package berth

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func TestParsePolicy(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Policy
	}{
		{"", PolicyOff},
		{"off", PolicyOff},
		{"OFF", PolicyOff},
		{"auto", PolicyAuto},
		{"Auto", PolicyAuto},
		{"require", PolicyRequire},
		{"bogus", PolicyOff}, // unknown falls back to off — safe default
	} {
		if got := ParsePolicy(tt.in); got != tt.want {
			t.Errorf("ParsePolicy(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDirAndBranch(t *testing.T) {
	want := filepath.Join(".shipmates", "berths", "captain")
	if got := Dir("captain"); got != want {
		t.Errorf("Dir(captain) = %q, want %q", got, want)
	}
	if got := Branch("captain"); got != "berth/captain" {
		t.Errorf("Branch(captain) = %q, want berth/captain", got)
	}
}

// initTmpGitRepo builds a minimal usable git repo at t.TempDir(): init +
// commit + rename-to-main + fake `origin/main` so Ensure has a base ref.
// Returns the repo path (also t.Chdir's into it). Skips the test if `git`
// isn't on PATH.
func initTmpGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	// A commit so worktree add has something to point at.
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	// A fake `origin/main` — Ensure looks up origin/main first.
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

func TestIsGitRepo(t *testing.T) {
	// Not-git dir first.
	nongit := t.TempDir()
	t.Chdir(nongit)
	if IsGitRepo() {
		t.Fatal("IsGitRepo() = true in non-git dir")
	}

	initTmpGitRepo(t)
	if !IsGitRepo() {
		t.Fatal("IsGitRepo() = false in initialized git repo")
	}
}

func TestEnsurePolicyOff(t *testing.T) {
	initTmpGitRepo(t)
	path, err := Ensure("nobody", PolicyOff)
	if err != nil {
		t.Fatalf("Ensure(off): %v", err)
	}
	if path != "" {
		t.Errorf("Ensure(off) returned %q, want empty", path)
	}
	// No berth dir should have been touched.
	if _, err := os.Stat(Dir("nobody")); !os.IsNotExist(err) {
		t.Errorf("PolicyOff created berth dir (%v)", err)
	}
}

func TestEnsurePolicyRequireNonGit(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := Ensure("captain", PolicyRequire)
	if err == nil {
		t.Fatal("Ensure(require) in non-git dir returned nil error, want failure")
	}
}

func TestEnsurePolicyAutoNonGit(t *testing.T) {
	// Auto degrades to no-berth in a non-git dir (the "non-git project"
	// fallback path — spawn at root as today).
	t.Chdir(t.TempDir())
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure(auto) non-git: %v", err)
	}
	if path != "" {
		t.Errorf("Ensure(auto) non-git returned %q, want empty (degrade)", path)
	}
}

func TestEnsureCreatesAndReusesBerth(t *testing.T) {
	dir := initTmpGitRepo(t)
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure(auto): %v", err)
	}
	want, _ := filepath.Abs(Dir("captain"))
	if path != want {
		t.Errorf("Ensure(auto) = %q, want %q", path, want)
	}
	// A .git file (worktree pointer) should exist.
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatalf("berth .git missing: %v", err)
	}

	// Second call reuses without error.
	path2, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("second Ensure(auto): %v", err)
	}
	if path2 != path {
		t.Errorf("second Ensure returned different path: %q vs %q", path2, path)
	}

	// Sanity: the berth is a real worktree of this repo.
	out, err := exec.Command("git", "-C", dir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !containsPath(string(out), path) {
		t.Errorf("berth not registered in worktree list:\n%s", out)
	}
}

func containsPath(hay, needle string) bool {
	hay = filepath.ToSlash(hay)
	needle = filepath.ToSlash(needle)
	return len(hay) >= len(needle) && (indexOf(hay, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRemoveRefusesDirty(t *testing.T) {
	initTmpGitRepo(t)
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Dirty the berth: an untracked file counts as dirty.
	if err := os.WriteFile(filepath.Join(path, "junk.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove("captain", false); err == nil {
		t.Fatal("Remove(dirty, force=false) succeeded, want refusal")
	}
	// force=true bypasses.
	if err := Remove("captain", true); err != nil {
		t.Fatalf("Remove(force=true) on dirty berth: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("berth path still exists after force remove: %v", err)
	}
}

func TestRemoveRefusesNestedWorktree(t *testing.T) {
	initTmpGitRepo(t)
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Simulate a routing-created per-issue worktree dir at
	// <berth>/.claude/worktrees/issue-1/. Presence alone signals "in flight".
	nested := filepath.Join(path, ".claude", "worktrees", "issue-1")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove("captain", true); err == nil {
		t.Fatal("Remove with nested worktree succeeded, want refusal (even under --force)")
	}
}

func TestRemoveMissing(t *testing.T) {
	initTmpGitRepo(t)
	// No berth exists — Remove should be a no-op, not an error.
	if err := Remove("ghost", false); err != nil {
		t.Fatalf("Remove(missing): %v", err)
	}
}

func TestCurrentIsBerth(t *testing.T) {
	initTmpGitRepo(t)
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// From repo root: not in a berth.
	if in, _ := CurrentIsBerth(); in {
		t.Errorf("CurrentIsBerth = true at repo root")
	}

	// From inside the berth: detected.
	t.Chdir(path)
	in, persona := CurrentIsBerth()
	if !in {
		t.Errorf("CurrentIsBerth = false inside berth")
	}
	if persona != "captain" {
		t.Errorf("CurrentIsBerth persona = %q, want captain", persona)
	}
}

func TestRefuseIfInBerth(t *testing.T) {
	initTmpGitRepo(t)
	if err := RefuseIfInBerth("update"); err != nil {
		t.Errorf("RefuseIfInBerth at repo root: %v", err)
	}
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Chdir(path)
	if err := RefuseIfInBerth("update"); err == nil {
		t.Fatal("RefuseIfInBerth in berth returned nil, want error")
	}
}

func TestResolveSpawnCWD_Resume_PreservesStoredCWD(t *testing.T) {
	// Guardrail: existing sessions keep their creation-cwd on resume — a
	// pre-berth meta with CWD="" stays at repo root even when the persona
	// now has berth: auto.
	initTmpGitRepo(t)
	// Simulate a pre-berth session by writing meta with empty CWD.
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteSessionMeta("captain", "cap", "uuid-1", "hash-1", ""); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	cfg := project.PersonaConfig{Berth: "auto"}
	got, err := ResolveSpawnCWD("captain", cfg, false /* resume */)
	if err != nil {
		t.Fatalf("ResolveSpawnCWD(resume): %v", err)
	}
	if got != "" {
		t.Errorf("ResolveSpawnCWD(pre-berth resume) = %q, want empty (repo-root)", got)
	}
	// And the berth wasn't created just from asking.
	if _, err := os.Stat(Dir("captain")); !os.IsNotExist(err) {
		t.Errorf("resume path created berth dir")
	}
}

func TestResolveSpawnCWD_Create_UsesBerth(t *testing.T) {
	initTmpGitRepo(t)
	cfg := project.PersonaConfig{Berth: "auto"}
	got, err := ResolveSpawnCWD("captain", cfg, true /* creating */)
	if err != nil {
		t.Fatalf("ResolveSpawnCWD(create): %v", err)
	}
	want, _ := filepath.Abs(Dir("captain"))
	if got != want {
		t.Errorf("ResolveSpawnCWD(create,auto) = %q, want %q", got, want)
	}
}

func TestResolveSpawnCWD_FrontmatterOverride(t *testing.T) {
	initTmpGitRepo(t)
	cfg := project.PersonaConfig{Berth: "auto", CWD: "custom/dir"}
	got, err := ResolveSpawnCWD("captain", cfg, true)
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	if got != "custom/dir" {
		t.Errorf("frontmatter CWD ignored: got %q, want custom/dir", got)
	}
	// Berth should NOT have been created — frontmatter cwd wins.
	if _, err := os.Stat(Dir("captain")); !os.IsNotExist(err) {
		t.Errorf("frontmatter CWD path still created berth dir")
	}
}

func TestResolveSpawnCWD_PolicyOff(t *testing.T) {
	initTmpGitRepo(t)
	cfg := project.PersonaConfig{Berth: "off"}
	got, err := ResolveSpawnCWD("crew", cfg, true)
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	if got != "" {
		t.Errorf("ResolveSpawnCWD(off) = %q, want empty", got)
	}
}
