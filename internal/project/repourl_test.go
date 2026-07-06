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
