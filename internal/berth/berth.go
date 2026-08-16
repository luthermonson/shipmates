// Package berth manages per-persona git worktrees — a persona's persistent
// home ("berth") at .shipmates/berths/<persona> on branch berth/<persona>.
//
// A berth is a launch cwd, not filesystem isolation: the persona's Claude Code
// session runs inside its berth (cmd.Dir = berthPath) rather than sharing the
// repo root with every other session. See docs/persona-berths.md for the
// design and its guardrails; the ones this package enforces mechanically are:
//
//   - R1a: manifest-mutating commands must run from the canonical root, not a
//     berth (RefuseIfInBerth is the gate).
//   - Remove must refuse a dirty berth or one with nested per-issue worktrees.
//   - Non-git projects degrade to no berth — the caller uses the repo root.
package berth

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/personaname"
	"github.com/luthermonson/shipmates/internal/project"
)

// Policy is a persona's berth policy, resolved from frontmatter + crew overrides.
type Policy string

const (
	// PolicyOff runs the persona at the repo root (today's behavior). Fleet default.
	PolicyOff Policy = "off"
	// PolicyAuto creates the worktree from origin/main if missing, uses it.
	// Default for the captain/coordinator persona only.
	PolicyAuto Policy = "auto"
	// PolicyRequire errors if a berth is absent. Explicit opt-in for personas
	// that must have isolation.
	PolicyRequire Policy = "require"
)

// ParsePolicy resolves a raw frontmatter/config string to a Policy value.
// Unknown / empty values become PolicyOff — the safe fleet default.
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

// ErrInvalidPersona is returned by the berth operations that would otherwise
// hand an unchecked persona name to `git worktree add`.
var ErrInvalidPersona = errors.New("invalid persona name")

// invalidPersonaSegment is the quarantine segment Dir and Branch substitute
// for a name that is not a legal persona name. Same reasoning, and the same
// NUL trick, as project.invalidPersonaSegment: the result stays inside
// .shipmates/berths (nothing lands somewhere surprising) and cannot be opened
// or passed to git, because both unix and Windows reject a path or an argv
// entry containing a NUL byte. Ensure and Remove refuse with ErrInvalidPersona
// before it gets that far; this is the backstop for any other caller.
const invalidPersonaSegment = "\x00invalid-persona"

func personaSegment(persona string) string {
	if !personaname.Valid(persona) {
		return invalidPersonaSegment
	}
	return persona
}

// Dir returns the conventional berth path for a persona, relative to the repo
// root: .shipmates/berths/<persona>. This is the *conventional* path; a
// persona may override cwd in frontmatter to point elsewhere.
//
// A berth path is not merely read: it is the argument to `git worktree add`,
// so an unchecked "../.." here creates a checkout wherever the caller's
// attacker pointed. An illegal persona name therefore yields a contained,
// unopenable path rather than a joined one.
func Dir(persona string) string {
	return filepath.Join(project.Dir, "berths", personaSegment(persona))
}

// Branch returns the conventional berth branch name — one worktree per branch
// is a git rule, so each berth needs its own ref. An illegal persona name
// yields a ref git will refuse rather than one it might resolve.
func Branch(persona string) string {
	return "berth/" + personaSegment(persona)
}

// IsGitRepo reports whether the current directory is inside a git working tree.
// A non-git project degrades to no berth (spawn at root), so this is the gate
// callers use before attempting any git-worktree operation.
func IsGitRepo() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// baseRef returns the git ref to base a new berth on. Prefers origin/main,
// falls back to origin/master, then to HEAD if neither remote branch exists.
// The berth branch is meant to stay short-divergence (guardrail R1b), so
// tracking origin's default is the right anchor.
func baseRef() string {
	for _, ref := range []string{"origin/main", "origin/master"} {
		if err := exec.Command("git", "rev-parse", "--verify", ref).Run(); err == nil {
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
	if policy == PolicyOff {
		return "", nil
	}
	// Ahead of IsGitRepo so the refusal is the same whether or not the
	// project happens to be a git checkout.
	if err := personaname.Validate(persona); err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidPersona, err)
	}
	if !IsGitRepo() {
		if policy == PolicyRequire {
			return "", fmt.Errorf("persona %q requires a berth but this project is not a git repo", persona)
		}
		return "", nil // degrade to no berth
	}

	rel := Dir(persona)
	abs, err := filepath.Abs(rel)
	if err != nil {
		return "", fmt.Errorf("resolve berth path: %w", err)
	}

	// Already a registered worktree? Reuse.
	if isWorktree(abs) {
		if dirty, _ := IsDirty(abs); dirty {
			// Warn-and-reuse (per §Open Questions #2). Never auto-reset.
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

	// Create.
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return "", fmt.Errorf("mkdir berths root: %w", err)
	}
	base := baseRef()
	branch := Branch(persona)
	// If the branch already exists (an earlier berth was removed but the ref
	// stuck around), reuse it: `git worktree add <path> <existing-branch>`.
	// Otherwise create it fresh from the base ref: `git worktree add -B ...`.
	var out []byte
	if branchExists(branch) {
		out, err = exec.Command("git", "worktree", "add", rel, branch).CombinedOutput()
	} else {
		out, err = exec.Command("git", "worktree", "add", "-b", branch, rel, base).CombinedOutput()
	}
	if err != nil {
		return "", fmt.Errorf("git worktree add %s: %w\n%s", rel, err, string(out))
	}
	return abs, nil
}

// branchExists reports whether the given local branch ref is known to git.
func branchExists(branch string) bool {
	return exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch).Run() == nil
}

// isWorktree reports whether abs is registered as a git worktree of the
// current repo. Compared as absolute paths so a Windows drive-letter variance
// or a trailing slash doesn't cause a false negative.
func isWorktree(abs string) bool {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false
	}
	target := comparablePath(abs)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimPrefix(line, "worktree ")
		if comparablePath(p) == target {
			return true
		}
	}
	return false
}

// comparablePath normalizes a path for comparison against what `git worktree
// list` reports.
//
// Resolving symlinks is the whole point. Git reports the fully resolved path,
// so comparing the string we happen to hold against git's answer says "not a
// worktree" for a berth that is plainly registered:
//
//   - on macOS the temp dir is /var/folders/…, which is a symlink to
//     /private/var/folders/…;
//   - on Windows a path can arrive in 8.3 short form (RUNNER~1) whose long
//     form is what git prints;
//   - anywhere, a user whose project sits under a symlinked home or a
//     directory junction hits the same mismatch.
//
// The failure mode is bad out of proportion to the cause: Ensure refuses with
// "exists but is not a git worktree" and Remove refuses to clean up, so berths
// are simply unusable on those paths.
//
// EvalSymlinks fails on a path that does not exist yet, which is normal here —
// Ensure calls this before creating the berth. Fall back to the lexical form
// in that case; a nonexistent path is not a registered worktree anyway.
func comparablePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return strings.ToLower(filepath.Clean(p))
}

// IsDirty reports whether the given worktree has uncommitted changes.
// A nil error with dirty=false means the tree is clean; errors surface as
// dirty=false (the caller may decide to warn rather than fail).
func IsDirty(worktreePath string) (bool, error) {
	cmd := exec.Command("git", "-C", worktreePath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// HasNestedWorktree reports whether the berth holds a live per-issue worktree
// under .claude/worktrees/ (the routing-catalog convention). Berth removal
// refuses while any nested worktree exists — deleting a parent while a nested
// tree is live would fracture routing work in flight.
func HasNestedWorktree(worktreePath string) (bool, error) {
	nested := filepath.Join(worktreePath, ".claude", "worktrees")
	entries, err := os.ReadDir(nested)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Any non-hidden entry counts — the routing convention creates one dir per
	// live issue, so presence is enough to signal "in flight".
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		return true, nil
	}
	return false, nil
}

// Remove tears down a persona's berth via `git worktree remove`. It refuses
// (returns an error) if the berth is dirty or holds a nested per-issue
// worktree; the caller may re-invoke with force=true to bypass the dirty
// check (nested-worktree refusal stands regardless — that's mid-flight work).
// A non-existent berth is not an error.
func Remove(persona string, force bool) error {
	if err := personaname.Validate(persona); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidPersona, err)
	}
	rel := Dir(persona)
	if _, err := os.Stat(rel); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !IsGitRepo() {
		// Not a git repo — just remove the directory tree.
		return os.RemoveAll(rel)
	}

	abs, err := filepath.Abs(rel)
	if err != nil {
		return err
	}
	if !isWorktree(abs) {
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
	args = append(args, rel)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w\n%s", err, string(out))
	}
	return nil
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
	if !IsGitRepo() {
		return false, ""
	}
	// A berth is a checkout under .shipmates/berths — look for that segment
	// in the current path. Case-insensitive on Windows.
	sep := string(os.PathSeparator)
	needle := strings.ToLower(sep + filepath.Join(project.Dir, "berths") + sep)
	hay := strings.ToLower(wd + sep)
	if idx := strings.Index(hay, needle); idx >= 0 {
		// Extract the persona segment for a helpful error message.
		tail := wd[idx+len(needle)-1:]
		parts := strings.Split(strings.Trim(tail, sep), sep)
		if len(parts) > 0 {
			return true, parts[0]
		}
		return true, ""
	}
	return false, ""
}

// RefuseIfInBerth is R1a's guard: manifest-mutating commands (add/remove/
// update) MUST run in the canonical root, never inside a berth. Returns a
// descriptive error when invoked from a berth; nil otherwise.
func RefuseIfInBerth(command string) error {
	inBerth, persona := CurrentIsBerth()
	if !inBerth {
		return nil
	}
	msg := "run this from the repo root, not from a berth"
	if persona != "" {
		msg = fmt.Sprintf("shipmates %s must run from the repo root, not from the %q berth (running it in a berth would fracture .shipmates/manifest.json)", command, persona)
	}
	return errors.New(msg)
}
