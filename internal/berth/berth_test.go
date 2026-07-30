package berth

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		{" off ", PolicyOff},
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
	// EvalSymlinks because macOS hands out /var/folders/... TempDirs that are
	// really /private/var/folders/..., and git reports the resolved path in
	// `worktree list` — an unresolved expectation would fail there only.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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

// TestEnsureRejectsAnIllegalPersonaName guards the path/branch injection seam:
// a berth name becomes a directory segment AND a git ref, so it goes through
// the same validator every other persona-scoped path uses.
func TestEnsureRejectsAnIllegalPersonaName(t *testing.T) {
	initTmpGitRepo(t)
	for _, name := range []string{"../escape", "Captain", "", "a/b"} {
		if _, err := Ensure(name, PolicyAuto); err == nil {
			t.Errorf("Ensure(%q) accepted an illegal persona name", name)
		}
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
	// The berth is on its own branch — one worktree per branch is a git rule.
	out, err := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != Branch("captain") {
		t.Errorf("berth branch = %q, want %q", got, Branch("captain"))
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
	list, err := exec.Command("git", "-C", dir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(string(list)), filepath.ToSlash(path)) {
		t.Errorf("berth not registered in worktree list:\n%s", list)
	}
	// …and List reports it.
	if got := List("."); len(got) != 1 || got[0] != "captain" {
		t.Errorf("List() = %v, want [captain]", got)
	}
}

// TestEnsureAtOperatesOnAnExplicitRoot proves the root-explicit variants work
// from an unrelated cwd — the shape every caller inside shipmates uses, since
// commands resolve project.CanonicalRoot rather than trusting the cwd.
func TestEnsureAtOperatesOnAnExplicitRoot(t *testing.T) {
	repo := initTmpGitRepo(t)
	elsewhere, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	path, err := EnsureAt(repo, "captain", PolicyAuto)
	if err != nil {
		t.Fatalf("EnsureAt: %v", err)
	}
	want := filepath.Join(repo, Dir("captain"))
	if path != want {
		t.Errorf("EnsureAt = %q, want %q", path, want)
	}
	if got := List(repo); len(got) != 1 || got[0] != "captain" {
		t.Errorf("List(repo) = %v, want [captain]", got)
	}
	if err := RemoveAt(repo, "captain", false); err != nil {
		t.Fatalf("RemoveAt: %v", err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("berth survived RemoveAt: %v", err)
	}
}

// TestEnsureRefusesANonWorktreeDirectory: a directory a human parked at the
// berth path is never adopted or deleted.
func TestEnsureRefusesANonWorktreeDirectory(t *testing.T) {
	initTmpGitRepo(t)
	if err := os.MkdirAll(Dir("captain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure("captain", PolicyAuto); err == nil {
		t.Fatal("Ensure adopted a non-worktree directory, want refusal")
	}
}

// TestEnsureReusesAnOrphanedBranch: removing a berth leaves berth/<persona>
// behind, and `git worktree add -b` would fail on the second provisioning.
func TestEnsureReusesAnOrphanedBranch(t *testing.T) {
	initTmpGitRepo(t)
	if _, err := Ensure("captain", PolicyAuto); err != nil {
		t.Fatal(err)
	}
	if err := Remove("captain", false); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+Branch("captain")).CombinedOutput(); err != nil {
		t.Skipf("git pruned the berth branch on remove, nothing to re-adopt: %s", out)
	}
	if _, err := Ensure("captain", PolicyAuto); err != nil {
		t.Fatalf("Ensure did not re-adopt the orphaned berth branch: %v", err)
	}
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

// TestRemoveRefusesNestedWorktreeDirs covers the convention-directory signal
// for BOTH routing spellings: catalog/routing/github.md emits
// .shipmates/worktrees/ today and .claude/worktrees/ historically. Either one
// means routing work is in flight.
func TestRemoveRefusesNestedWorktreeDirs(t *testing.T) {
	for _, convention := range []string{
		filepath.Join(project.Dir, "worktrees", "issue-1"),
		filepath.Join(".claude", "worktrees", "issue-1"),
	} {
		t.Run(filepath.ToSlash(convention), func(t *testing.T) {
			initTmpGitRepo(t)
			path, err := Ensure("captain", PolicyAuto)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(path, convention), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := Remove("captain", true); err == nil {
				t.Fatal("Remove with nested worktree succeeded, want refusal (even under --force)")
			}
		})
	}
}

// TestRemoveRefusesARegisteredNestedWorktree is the authoritative signal: a
// real git worktree nested under the berth, at a path neither convention
// names. Location-agnostic detection is what keeps the guardrail honest when
// the routing catalog moves its worktree directory again.
func TestRemoveRefusesARegisteredNestedWorktree(t *testing.T) {
	initTmpGitRepo(t)
	path, err := Ensure("captain", PolicyAuto)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	nested := filepath.Join(path, "some", "other", "place")
	if out, err := exec.Command("git", "-C", path, "worktree", "add", "-b", "issue-1", nested, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("nested worktree add: %v\n%s", err, out)
	}
	if nestedFound, err := HasNestedWorktree(path); err != nil || !nestedFound {
		t.Fatalf("HasNestedWorktree = %v, %v; want true, nil", nestedFound, err)
	}
	if err := Remove("captain", true); err == nil {
		t.Fatal("Remove with a registered nested worktree succeeded, want refusal")
	}
}

func TestRemoveMissing(t *testing.T) {
	initTmpGitRepo(t)
	// No berth exists — Remove should be a no-op, not an error.
	if err := Remove("ghost", false); err != nil {
		t.Fatalf("Remove(missing): %v", err)
	}
}

func TestRemoveRefusesANonWorktreeDirectory(t *testing.T) {
	initTmpGitRepo(t)
	if err := os.MkdirAll(Dir("captain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Remove("captain", true); err == nil {
		t.Fatal("Remove deleted a directory that is not a worktree, want refusal")
	}
	if _, err := os.Stat(Dir("captain")); err != nil {
		t.Errorf("refused Remove still touched the directory: %v", err)
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

	// …and from a subdirectory of the berth, which is where an agent
	// realistically is when it runs a shipmates command.
	sub := filepath.Join(path, "deep", "inside")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if in, persona := CurrentIsBerth(); !in || persona != "captain" {
		t.Errorf("CurrentIsBerth from berth subdir = %v, %q; want true, captain", in, persona)
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
	err = RefuseIfInBerth("update")
	if err == nil {
		t.Fatal("RefuseIfInBerth in berth returned nil, want error")
	}
	if !strings.Contains(err.Error(), "update") || !strings.Contains(err.Error(), "captain") {
		t.Errorf("refusal does not name the command and persona: %v", err)
	}
}

func TestResolveSpawnCWD_PolicyOff(t *testing.T) {
	initTmpGitRepo(t)
	got, err := ResolveSpawnCWD("crew", project.PersonaConfig{Berth: "off"})
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	if got != "" {
		t.Errorf("ResolveSpawnCWD(off) = %q, want empty (project root)", got)
	}
}

func TestResolveSpawnCWD_AutoUsesBerth(t *testing.T) {
	initTmpGitRepo(t)
	got, err := ResolveSpawnCWD("captain", project.PersonaConfig{Berth: "auto"})
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	want, _ := filepath.Abs(Dir("captain"))
	if got != want {
		t.Errorf("ResolveSpawnCWD(auto) = %q, want %q", got, want)
	}
}

func TestResolveSpawnCWD_ExplicitCWDWins(t *testing.T) {
	repo := initTmpGitRepo(t)
	got, err := ResolveSpawnCWD("captain", project.PersonaConfig{Berth: "auto", CWD: "custom/dir"})
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	if want := filepath.Join(repo, "custom", "dir"); got != want {
		t.Errorf("cwd override = %q, want %q", got, want)
	}
	// The berth must NOT have been created — the override wins outright.
	if _, err := os.Stat(Dir("captain")); !os.IsNotExist(err) {
		t.Errorf("cwd override still provisioned a berth: %v", err)
	}
}

func TestResolveSpawnCWD_AbsoluteCWDPassesThrough(t *testing.T) {
	initTmpGitRepo(t)
	abs, err := filepath.Abs(filepath.Join(t.TempDir(), "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSpawnCWD("captain", project.PersonaConfig{CWD: abs})
	if err != nil {
		t.Fatalf("ResolveSpawnCWD: %v", err)
	}
	if got != abs {
		t.Errorf("absolute cwd override = %q, want %q", got, abs)
	}
}

// TestSessionFingerprintOnlyMovesWithTheDirectory is the modern spelling of
// the "berth only at session creation, never mid-session" guardrail: a project
// with no berth keeps byte-identical fingerprints (so nothing existing is
// auto-freshed), and a directory change drifts the fingerprint (so a session
// is re-created in the new directory instead of resumed into it).
func TestSessionFingerprintOnlyMovesWithTheDirectory(t *testing.T) {
	base := project.PersonaConfig{Model: "gpt-5.6-sol"}.Fingerprint()
	if got := SessionFingerprint(base, ""); got != base {
		t.Errorf("SessionFingerprint(base, \"\") = %q, want the base %q", got, base)
	}
	berthed := SessionFingerprint(base, filepath.Join("repo", ".shipmates", "berths", "captain"))
	if berthed == base {
		t.Error("a berthed session shares the root session's fingerprint; a resume would migrate its cwd")
	}
	if again := SessionFingerprint(base, filepath.Join("repo", ".shipmates", "berths", "captain")); again != berthed {
		t.Error("SessionFingerprint is not stable for the same directory")
	}
	if other := SessionFingerprint(base, filepath.Join("repo", ".shipmates", "berths", "skipper")); other == berthed {
		t.Error("two berths share one fingerprint")
	}
}
