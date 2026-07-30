// Package berth manages per-persona git worktrees — a persona's persistent
// home ("berth") at .shipmates/berths/<persona> on branch berth/<persona>.
//
// A berth is a launch cwd, not filesystem isolation: the persona's runtime
// session runs inside its berth (the child process's working directory)
// rather than sharing the repo root with every other session. See
// docs/persona-berths.md for the design and its guardrails; the ones this
// package enforces mechanically are:
//
//   - R1a: manifest-mutating commands must run from the canonical root, not a
//     berth (RefuseIfInBerth is the gate).
//   - Remove must refuse a dirty berth or one with nested per-issue worktrees.
//   - Non-git projects degrade to no berth — the caller uses the repo root.
//
// Everything here is portable: git worktrees behave the same on Linux, macOS
// and Windows, path comparison is case-folded so a drive-letter or 8.3
// variance cannot produce a false negative, and no unix-only syscall is used.
// There are deliberately no build tags on this package.
package berth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
)

// BerthsDirName is the directory under .shipmates/ that holds every berth.
const BerthsDirName = "berths"

// Policy is a persona's berth policy, resolved from crew overrides in
// shipmates.yaml (see project.CrewOverride.Berth).
type Policy string

const (
	// PolicyOff runs the persona at the repo root (today's behavior). Fleet default.
	PolicyOff Policy = "off"
	// PolicyAuto creates the worktree from origin/main if missing, uses it.
	PolicyAuto Policy = "auto"
	// PolicyRequire errors if a berth cannot be provisioned. Explicit opt-in
	// for personas that must have their own tree.
	PolicyRequire Policy = "require"
)

// ParsePolicy resolves a raw config string to a Policy value. Unknown / empty
// values become PolicyOff — the safe fleet default.
func ParsePolicy(s string) Policy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return PolicyAuto
	case "require":
		return PolicyRequire
	case "off", "":
		return PolicyOff
	}
	return PolicyOff
}

// Dir returns the conventional berth path for a persona, relative to the repo
// root: .shipmates/berths/<persona>.
func Dir(persona string) string {
	return filepath.Join(project.Dir, BerthsDirName, persona)
}

// Branch returns the conventional berth branch name — one worktree per branch
// is a git rule, so each berth needs its own ref.
func Branch(persona string) string {
	return "berth/" + persona
}

// git runs a git subcommand rooted at root ("" means the process cwd) and
// returns its combined output. One helper so every call site is root-explicit;
// `git -C` is the portable spelling on every platform.
func git(root string, args ...string) ([]byte, error) {
	full := args
	if root != "" && root != "." {
		full = append([]string{"-C", root}, args...)
	}
	return exec.Command("git", full...).CombinedOutput()
}

// IsGitRepo reports whether the current directory is inside a git working tree.
// A non-git project degrades to no berth (spawn at root), so this is the gate
// callers use before attempting any git-worktree operation.
func IsGitRepo() bool { return IsGitRepoAt(".") }

// IsGitRepoAt is IsGitRepo for an explicit project root.
func IsGitRepoAt(root string) bool {
	out, err := git(root, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// baseRef returns the git ref to base a new berth on. Prefers origin/main,
// falls back to origin/master, then to HEAD if neither remote branch exists.
// The berth branch is meant to stay short-divergence (guardrail R1b), so
// tracking origin's default is the right anchor.
func baseRef(root string) string {
	for _, ref := range []string{"origin/main", "origin/master"} {
		if _, err := git(root, "rev-parse", "--verify", ref); err == nil {
			return ref
		}
	}
	return "HEAD"
}

// Ensure guarantees a berth exists for the persona under the given policy.
// Returns the absolute berth path, or ("", nil) when policy is off / non-git.
//
// Behavior by policy:
//   - PolicyOff — no-op, returns ("", nil).
//   - PolicyAuto — creates a fresh worktree if missing; reuses a clean existing
//     one; warns-and-reuses a dirty one (never auto-resets).
//   - PolicyRequire — same as Auto, but errors instead of degrading when the
//     project isn't a git repo.
//
// "Dir present but not a worktree" is an error under any policy — refusing to
// touch a non-worktree directory is the least-surprising response.
func Ensure(persona string, policy Policy) (string, error) {
	return EnsureAt(".", persona, policy)
}

// EnsureAt is Ensure for an explicit project root.
func EnsureAt(root, persona string, policy Policy) (string, error) {
	if policy == PolicyOff {
		return "", nil
	}
	if err := project.ValidatePersonaName(persona); err != nil {
		return "", err
	}
	if !IsGitRepoAt(root) {
		if policy == PolicyRequire {
			return "", fmt.Errorf("persona %q requires a berth but this project is not a git repo", persona)
		}
		return "", nil // degrade to no berth
	}

	rel := Dir(persona)
	abs, err := filepath.Abs(filepath.Join(root, rel))
	if err != nil {
		return "", fmt.Errorf("resolve berth path: %w", err)
	}

	// Already a registered worktree? Reuse.
	if isWorktree(root, abs) {
		if dirty, _ := IsDirty(abs); dirty {
			// Warn-and-reuse. Never auto-reset: discarding an operator's
			// uncommitted work to tidy a launch path is not shipmates' call.
			fmt.Fprintf(os.Stderr, "shipmates: berth %s has uncommitted changes — reusing anyway (never auto-reset)\n", rel)
		}
		return abs, nil
	}

	// Path exists but not a registered worktree — refuse.
	if _, err := os.Stat(abs); err == nil {
		return "", fmt.Errorf("berth path %s exists but is not a git worktree; move or delete it before enabling a berth", rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir berths root: %w", err)
	}
	base := baseRef(root)
	branch := Branch(persona)
	// If the branch already exists (an earlier berth was removed but the ref
	// stuck around), reuse it: `git worktree add <path> <existing-branch>`.
	// Otherwise create it fresh from the base ref.
	var out []byte
	if branchExists(root, branch) {
		out, err = git(root, "worktree", "add", abs, branch)
	} else {
		out, err = git(root, "worktree", "add", "-b", branch, abs, base)
	}
	if err != nil {
		return "", fmt.Errorf("git worktree add %s: %w\n%s", rel, err, string(out))
	}
	return abs, nil
}

// branchExists reports whether the given local branch ref is known to git.
func branchExists(root, branch string) bool {
	_, err := git(root, "rev-parse", "--verify", "refs/heads/"+branch)
	return err == nil
}

// samePath compares two filesystem paths for berth purposes. Case-folded and
// cleaned so a Windows drive-letter case difference or a trailing separator
// never produces a false negative.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// worktreePaths lists every path git has registered as a worktree of this repo.
func worktreePaths(root string) []string {
	out, err := git(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		paths = append(paths, strings.TrimPrefix(line, "worktree "))
	}
	return paths
}

// isWorktree reports whether abs is registered as a git worktree of the repo
// at root.
func isWorktree(root, abs string) bool {
	for _, p := range worktreePaths(root) {
		if samePath(p, abs) {
			return true
		}
	}
	return false
}

// IsDirty reports whether the given worktree has uncommitted changes.
// A nil error with dirty=false means the tree is clean; errors surface as
// dirty=false (the caller may decide to warn rather than fail).
func IsDirty(worktreePath string) (bool, error) {
	out, err := exec.Command("git", "-C", worktreePath, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// nestedWorktreeDirs are the per-issue worktree locations the routing catalog
// has used. `.shipmates/worktrees/` is what catalog/routing/github.md emits
// today; `.claude/worktrees/` is the older spelling (and the convention this
// repo's own agents follow), so both are treated as "routing work in flight".
//
// Keeping both is deliberate rather than tidy: the two conventions genuinely
// coexist on disk in projects that were routed before the rename, and berth
// removal is the wrong place to decide the winner. See docs/persona-berths.md
// §"Nested per-issue worktrees" for the inconsistency this papers over.
var nestedWorktreeDirs = [][]string{
	{project.Dir, "worktrees"},
	{".claude", "worktrees"},
}

// HasNestedWorktree reports whether the berth holds a live per-issue worktree.
// Berth removal refuses while any nested worktree exists — deleting a parent
// while a nested tree is live would fracture routing work in flight.
//
// Two signals, either of which counts:
//
//  1. git itself has a worktree registered at a path underneath the berth.
//     This is authoritative and location-agnostic, so a routing convention
//     that moves does not silently disable the guardrail.
//  2. a non-empty conventional worktrees directory inside the berth. This
//     catches a half-created or already-unregistered tree whose files are
//     still on disk.
func HasNestedWorktree(worktreePath string) (bool, error) {
	if nested, err := hasRegisteredNestedWorktree(worktreePath); err != nil {
		return false, err
	} else if nested {
		return true, nil
	}
	for _, parts := range nestedWorktreeDirs {
		dir := filepath.Join(append([]string{worktreePath}, parts...)...)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		// Any non-hidden entry counts — the routing convention creates one dir
		// per live issue, so presence is enough to signal "in flight".
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

// hasRegisteredNestedWorktree asks git whether any registered worktree lives
// underneath worktreePath (excluding worktreePath itself).
func hasRegisteredNestedWorktree(worktreePath string) (bool, error) {
	abs, err := filepath.Abs(worktreePath)
	if err != nil {
		return false, err
	}
	prefix := strings.ToLower(filepath.Clean(abs)) + string(os.PathSeparator)
	for _, p := range worktreePaths(worktreePath) {
		candidate, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(filepath.Clean(candidate))+string(os.PathSeparator), prefix) &&
			!samePath(candidate, abs) {
			return true, nil
		}
	}
	return false, nil
}

// Remove tears down a persona's berth via `git worktree remove`. It refuses
// (returns an error) if the berth is dirty or holds a nested per-issue
// worktree; the caller may re-invoke with force=true to bypass the dirty
// check (nested-worktree refusal stands regardless — that's mid-flight work).
// A non-existent berth is not an error.
func Remove(persona string, force bool) error {
	return RemoveAt(".", persona, force)
}

// RemoveAt is Remove for an explicit project root.
func RemoveAt(root, persona string, force bool) error {
	if err := project.ValidatePersonaName(persona); err != nil {
		return err
	}
	rel := Dir(persona)
	path := filepath.Join(root, rel)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !IsGitRepoAt(root) {
		// Not a git repo — just remove the directory tree.
		return os.RemoveAll(path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if !isWorktree(root, abs) {
		// Directory exists but isn't a worktree — refuse to avoid nuking
		// something a human parked there.
		return fmt.Errorf("%s is not a registered git worktree; refusing to remove", rel)
	}

	if nested, err := HasNestedWorktree(abs); err != nil {
		return err
	} else if nested {
		return fmt.Errorf("berth %s holds a nested per-issue worktree — clean routing work in flight before removing the berth", rel)
	}

	if !force {
		if dirty, err := IsDirty(abs); err != nil {
			return err
		} else if dirty {
			return fmt.Errorf("berth %s has uncommitted changes — commit/discard them first, or pass --force", rel)
		}
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, abs)
	if out, err := git(root, args...); err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, string(out))
	}
	return nil
}

// List returns the personas that currently have a berth registered as a git
// worktree of the repo at root, in git's own order.
func List(root string) []string {
	if !IsGitRepoAt(root) {
		return nil
	}
	base, err := filepath.Abs(filepath.Join(root, project.Dir, BerthsDirName))
	if err != nil {
		return nil
	}
	prefix := strings.ToLower(filepath.Clean(base)) + string(os.PathSeparator)
	var out []string
	for _, p := range worktreePaths(root) {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		lower := strings.ToLower(filepath.Clean(abs))
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		// Only direct children are berths; a nested per-issue worktree under a
		// berth is not one.
		tail := filepath.Clean(abs)[len(filepath.Clean(base))+1:]
		if strings.ContainsRune(tail, os.PathSeparator) {
			continue
		}
		out = append(out, tail)
	}
	return out
}

// CurrentIsBerth reports whether the shipmates process's cwd is inside one of
// this repo's berths — used by R1a to refuse manifest-mutating commands from
// a berth (running `update` in divergent berths would fracture the tracked
// .shipmates/manifest.json).
func CurrentIsBerth() (bool, string) {
	wd, err := os.Getwd()
	if err != nil {
		return false, ""
	}
	// A berth is a checkout under .shipmates/berths — look for that segment
	// in the current path. Case-insensitive on Windows. The cheap string test
	// comes first so the overwhelmingly common "not in a berth" answer costs
	// no subprocess: R1a runs on every add/init/update/remove.
	sep := string(os.PathSeparator)
	needle := strings.ToLower(sep + filepath.Join(project.Dir, BerthsDirName) + sep)
	hay := strings.ToLower(wd + sep)
	idx := strings.Index(hay, needle)
	if idx < 0 {
		return false, ""
	}
	if !IsGitRepoAt(wd) {
		return false, ""
	}
	// Extract the persona segment for a helpful error message.
	tail := wd[idx+len(needle)-1:]
	parts := strings.Split(strings.Trim(tail, sep), sep)
	if len(parts) > 0 {
		return true, parts[0]
	}
	return true, ""
}

// RefuseIfInBerth is R1a's guard: manifest-mutating commands (init/add/
// remove/update) MUST run in the canonical root, never inside a berth.
// Returns a descriptive error when invoked from a berth; nil otherwise.
func RefuseIfInBerth(command string) error {
	inBerth, persona := CurrentIsBerth()
	if !inBerth {
		return nil
	}
	if persona != "" {
		return fmt.Errorf("shipmates %s must run from the repo root, not from the %q berth (running it in a berth would fracture .shipmates/manifest.json)", command, persona)
	}
	return errors.New("run this from the repo root, not from a berth")
}
