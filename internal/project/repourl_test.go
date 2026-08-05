package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeGitURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git@github.com:luthermonson/shipmates.git", "https://github.com/luthermonson/shipmates"},
		{"git@github.com:luthermonson/shipmates", "https://github.com/luthermonson/shipmates"},
		{"https://github.com/luthermonson/shipmates.git", "https://github.com/luthermonson/shipmates"},
		{"https://github.com/luthermonson/shipmates", "https://github.com/luthermonson/shipmates"},
		{"http://gitea.local/org/repo.git", "https://gitea.local/org/repo"},
		{"ssh://git@github.com/luthermonson/shipmates.git", "https://github.com/luthermonson/shipmates"},
		{"ssh://git@gitea.local:2222/org/repo.git", "https://gitea.local/org/repo"},
		{"git://github.com/org/repo.git", "https://github.com/org/repo"},
		{"https://user:tok@github.com/org/repo.git", "https://github.com/org/repo"},
		{"", ""},
		{"C:/Users/luthe/some/dir", ""},
		{"file:///C:/Users/luthe/beads-remote", ""},
		{"git@github.com", ""},
	}
	for _, c := range cases {
		if got := NormalizeGitURL(c.in); got != c.want {
			t.Errorf("NormalizeGitURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOriginFromGitConfig(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, ".git")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/thing.git\n[remote \"origin\"]\n\turl = git@github.com:luthermonson/card-cannon.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(git, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := originFromGitConfig(dir); got != "git@github.com:luthermonson/card-cannon.git" {
		t.Fatalf("originFromGitConfig = %q", got)
	}
	if got := originFromGitConfig(t.TempDir()); got != "" {
		t.Fatalf("non-repo should be empty, got %q", got)
	}
}

func TestOriginFromGitConfigNoOriginRemote(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, ".git")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only a non-origin remote plus an origin-adjacent section: neither may be
	// mistaken for [remote "origin"].
	cfg := "[remote \"upstream\"]\n\turl = git@github.com:other/thing.git\n[branch \"origin\"]\n\turl = nope\n"
	if err := os.WriteFile(filepath.Join(git, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := originFromGitConfig(dir); got != "" {
		t.Fatalf("originFromGitConfig = %q, want empty", got)
	}
}

func TestOriginFromGitConfigMissingConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := originFromGitConfig(dir); got != "" {
		t.Fatalf("originFromGitConfig with no config file = %q, want empty", got)
	}
}

func TestOriginFromGitConfigWorktreePointer(t *testing.T) {
	dir := t.TempDir()
	// A linked worktree has .git as a FILE pointing at the real gitdir. The
	// parser must decline it so RepoWebURL falls through to the git fallback.
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := originFromGitConfig(dir); got != "" {
		t.Fatalf("originFromGitConfig on a worktree pointer = %q, want empty", got)
	}
}

func TestRepoWebURLFromGitConfig(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, ".git")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = git@github.com:luthermonson/shipmates.git\n"
	if err := os.WriteFile(filepath.Join(git, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := RepoWebURL(); got != "https://github.com/luthermonson/shipmates" {
		t.Fatalf("RepoWebURL = %q, want the normalised browse URL", got)
	}
}

func TestRepoWebURLLocalRemoteIsNotBrowsable(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, ".git")
	if err := os.MkdirAll(git, 0o755); err != nil {
		t.Fatal(err)
	}
	// A filesystem remote has no web UI; the fleet must not render a bogus link.
	cfg := "[remote \"origin\"]\n\turl = C:/Users/luthe/beads-remote\n"
	if err := os.WriteFile(filepath.Join(git, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := RepoWebURL(); got != "" {
		t.Fatalf("RepoWebURL = %q, want empty for a filesystem remote", got)
	}
}

func TestNormalizeGitURLMoreShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"   git@github.com:org/repo.git   ", "https://github.com/org/repo"},
		{"git@github.com:/org/repo/", "https://github.com/org/repo"},
		{"https://github.com/", ""},          // host but no path
		{"ftp://github.com/org/repo", ""},    // unknown scheme
		{"github.com/org/repo", ""},          // no scheme and no user@
		{"git@:org/repo", ""},                // empty host
		{"git@github.com:", ""},              // empty path
		{"ssh://github.com:22/org/repo.git", "https://github.com/org/repo"},
	}
	for _, c := range cases {
		if got := NormalizeGitURL(c.in); got != c.want {
			t.Errorf("NormalizeGitURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
