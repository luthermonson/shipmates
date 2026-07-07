package permissions

import "testing"

func TestMatchBash_SpaceBoundary(t *testing.T) {
	// The single most important semantic: `ls *` matches `ls -la` (there's a
	// space between the literal `ls` and the wildcard's match) but does NOT
	// match `lsof` (no space, so `ls` isn't a token boundary).
	cases := []struct {
		pattern, command string
		want             bool
	}{
		{"ls *", "ls -la", true},
		{"ls *", "ls -la src/", true},
		{"ls *", "lsof", false},
		{"ls *", "ls", false}, // trailing star wants at least one char post-space
		{"ls*", "lsof", true}, // no space before * — no boundary
		{"ls*", "ls -la", true},
	}
	for _, tc := range cases {
		got := MatchBash(tc.pattern, tc.command)
		if got != tc.want {
			t.Errorf("MatchBash(%q, %q) = %v, want %v", tc.pattern, tc.command, got, tc.want)
		}
	}
}

func TestMatchBash_GitGlob(t *testing.T) {
	cases := []struct {
		pattern, command string
		want             bool
	}{
		{"git *", "git status", true},
		{"git *", "git log --oneline", true},
		{"git *", "git", false},
		{"git *", "gitk", false},
		{"git * main", "git checkout main", true},
		{"git * main", "git log main", true},
		{"git * main", "git checkout feature", false},
	}
	for _, tc := range cases {
		got := MatchBash(tc.pattern, tc.command)
		if got != tc.want {
			t.Errorf("MatchBash(%q, %q) = %v, want %v", tc.pattern, tc.command, got, tc.want)
		}
	}
}

func TestMatchBash_ColonStarSynonym(t *testing.T) {
	// `git:*` is documented as equivalent to `git *`.
	if !MatchBash("git:*", "git status") {
		t.Error("`git:*` should match `git status`")
	}
	if MatchBash("git:*", "gitk") {
		t.Error("`git:*` should NOT match `gitk`")
	}
	if !MatchBash("pnpm:*", "pnpm test --watch") {
		t.Error("`pnpm:*` should match `pnpm test --watch`")
	}
}

func TestMatchBash_ExactMatch(t *testing.T) {
	// Patterns without a wildcard require full-string exact match.
	if !MatchBash("git status", "git status") {
		t.Error("exact pattern should match")
	}
	if MatchBash("git status", "git status --short") {
		t.Error("exact pattern should NOT prefix-match")
	}
}

func TestMatchBash_EmptyPattern(t *testing.T) {
	// The bare `Bash` rule (empty pattern) matches everything.
	if !MatchBash("", "anything at all") {
		t.Error("empty pattern should match all")
	}
}

func TestMatchPath_BasenameAnywhere(t *testing.T) {
	// Gitignore-style: no slashes -> basename match at any depth.
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{".env", ".env", true},
		{".env", "foo/.env", true},
		{".env", "deep/nested/.env", true},
		{".env", ".environment", false}, // basename mismatch
		{".env", "foo/.envrc", false},
	}
	for _, tc := range cases {
		got := MatchPath(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestMatchPath_DoubleStar(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/.env", ".env", true},
		{"**/.env", "foo/.env", true},
		{"**/.env", "a/b/c/.env", true},
		{"src/**", "src/foo.go", true},
		{"src/**", "src/a/b/c.go", true},
		{"src/**", "lib/foo.go", false},
		{"src/**/*.go", "src/foo.go", true},
		{"src/**/*.go", "src/a/b/foo.go", true},
		{"src/**/*.go", "src/foo.txt", false},
	}
	for _, tc := range cases {
		got := MatchPath(tc.pattern, tc.path)
		if got != tc.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestMatchPath_SingleStarStopsAtSlash(t *testing.T) {
	// `*` should not cross path boundaries.
	if MatchPath("src/*.go", "src/a/b.go") {
		t.Error("single-star should not cross slashes")
	}
	if !MatchPath("src/*.go", "src/foo.go") {
		t.Error("single-star should match within a component")
	}
}

func TestMatchDomain_WildcardSubdomain(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"*.github.com", "github.com", true},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "raw.github.com", true},
		{"*.github.com", "githubusercontent.com", false},
		{"github.com", "github.com", true},
		{"github.com", "api.github.com", false},
		{"domain:*.github.com", "https://api.github.com/repos", true},
		{"domain:example.com", "http://example.com/foo?x=1", true},
	}
	for _, tc := range cases {
		got := MatchDomain(tc.pattern, tc.host)
		if got != tc.want {
			t.Errorf("MatchDomain(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}

func TestParseRule(t *testing.T) {
	cases := []struct {
		raw          string
		wantTool     string
		wantPattern  string
	}{
		{"Bash(git *)", "Bash", "git *"},
		{"Bash", "Bash", ""},
		{"Read(**/.env)", "Read", "**/.env"},
		{"WebFetch(domain:*.github.com)", "WebFetch", "domain:*.github.com"},
		// Spaces in pattern preserved.
		{"Bash(git log --oneline)", "Bash", "git log --oneline"},
	}
	for _, tc := range cases {
		r := ParseRule(tc.raw)
		if r.Tool != tc.wantTool || r.Pattern != tc.wantPattern {
			t.Errorf("ParseRule(%q) = {Tool:%q, Pattern:%q}, want {%q, %q}",
				tc.raw, r.Tool, r.Pattern, tc.wantTool, tc.wantPattern)
		}
	}
}
