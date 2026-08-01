package berth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestComparablePathResolvesSymlinks pins the bug that made berths unusable on
// macOS and Windows CI: `git worktree list` reports fully resolved paths, so a
// berth reached through a symlink compared unequal to git's answer and Ensure
// refused with "exists but is not a git worktree".
//
// It asserts on comparablePath rather than on isWorktree because isWorktree
// shells out to git; the path comparison is the part that was wrong, and it can
// be checked exactly.
//
// This test could not have caught the original failure on a developer machine —
// C:\Users\<name> and /home/<name> are already real paths. It reproduces the
// condition explicitly instead of hoping the environment supplies it.
func TestComparablePathResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		// Windows needs Developer Mode or elevation to create symlinks. Skipping
		// is honest here: the assertion below is meaningless without one.
		t.Skipf("cannot create a symlink in this environment: %v", err)
	}

	if got, want := comparablePath(link), comparablePath(target); got != want {
		t.Errorf("comparablePath(symlink) = %q, want it to equal the target's %q\n"+
			"git reports resolved paths, so an unresolved comparison makes a registered berth look unregistered", got, want)
	}
}

// A path that does not exist yet must still compare cleanly: Ensure calls
// isWorktree before creating the berth, so EvalSymlinks fails by design on the
// common path and must not take the comparison down with it.
func TestComparablePathHandlesMissingPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created-yet")
	if got := comparablePath(missing); got == "" {
		t.Fatal("comparablePath returned empty for a nonexistent path; it must fall back to the lexical form")
	}
	if comparablePath(missing) != comparablePath(missing) {
		t.Fatal("comparablePath is not deterministic for a nonexistent path")
	}
}
