package project

import "testing"

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
