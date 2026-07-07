// Package permissions mirrors Claude Code's own settings-driven permission
// model so shipmates' PreToolUse gate can auto-decide anything Claude Code
// itself would have auto-decided. The goal is exact compatibility: rules
// written in `.claude/settings.json` behave the same whether Claude is
// running vanilla or under a shipmates captain.
//
// This file holds the pure pattern-matching primitives — three matchers
// keyed off the tool name in a rule:
//
//   - Bash matcher: glob-style with space-boundary semantics. `Bash(ls *)`
//     matches `ls -la` but NOT `lsof`. `Bash(ls*)` matches both.
//     `Bash(pattern:*)` is a documented synonym for `Bash(pattern *)`.
//   - Path matcher (Read/Edit/Write/etc.): gitignore-style path globs.
//     `Read(**/.env)` matches `.env` at any depth; `Read(src/**)` matches
//     everything under src.
//   - Domain matcher (WebFetch): host-glob. `WebFetch(domain:*.github.com)`
//     matches `github.com` and `api.github.com`.
//
// The matchers are intentionally dumb: they know nothing about read-only
// builtins, wrapper stripping, compound commands, or the deny→ask→allow
// precedence. All of that lives in evaluator.go and bash.go.
package permissions

import (
	"path/filepath"
	"strings"
)

// Rule is a single parsed entry from settings.json's permissions arrays. The
// original text like "Bash(git *)" is split into Tool="Bash" and Pattern="git *".
// A bare "Bash" (no parens) becomes Tool="Bash", Pattern="" — matches any Bash
// call.
type Rule struct {
	Raw     string // as written in settings, for reason strings
	Tool    string // "Bash", "Read", "Edit", "WebFetch", "*", …
	Pattern string // may be empty (bare tool name matches all)
}

// ParseRule splits "Tool(pattern)" into its parts. Whitespace inside the
// parens is preserved (patterns can and do contain spaces). A bare tool name
// like "Bash" parses to {Tool:"Bash", Pattern:""}.
func ParseRule(raw string) Rule {
	r := Rule{Raw: raw}
	s := strings.TrimSpace(raw)
	open := strings.Index(s, "(")
	if open < 0 || !strings.HasSuffix(s, ")") {
		r.Tool = s
		return r
	}
	r.Tool = strings.TrimSpace(s[:open])
	r.Pattern = s[open+1 : len(s)-1]
	return r
}

// MatchBash reports whether pattern matches command using Claude Code's Bash
// pattern semantics. Neither side is expected to be pre-tokenized. The empty
// pattern matches everything (used for the bare `Bash` rule).
//
// Semantics (per code.claude.com/docs/en/permissions):
//   - `pattern:*` is a synonym for `pattern *` (colon-star suffix).
//   - `*` in the pattern matches any run of characters. Word boundaries are
//     enforced by requiring the character right AFTER a `*` at a token edge
//     to sit at a space boundary in the command. `ls *` will NOT match
//     `lsof` because there's no space between `ls` and `of`.
//   - Patterns without a trailing `*` require an exact match of the entire
//     command (not a prefix).
func MatchBash(pattern, command string) bool {
	pattern = strings.TrimSpace(pattern)
	command = strings.TrimSpace(command)
	if pattern == "" {
		return true
	}
	// Colon-star suffix: `git:*` == `git *`.
	if strings.HasSuffix(pattern, ":*") {
		pattern = pattern[:len(pattern)-2] + " *"
	}
	return bashGlob(pattern, command)
}

// bashGlob implements the actual matching. It's a small recursive-descent
// matcher over the pattern; `*` is greedy but constrained by the next literal
// character in the pattern (which, in practice, is either end-of-pattern or
// a space — hence "word-boundary semantics").
func bashGlob(pattern, s string) bool {
	// Fast path: no wildcards — exact match.
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	// Anchor: patterns are always anchored at both ends. `Bash(git status)`
	// does NOT match `git status --short`; the user would have written
	// `Bash(git status*)` or `Bash(git *)`.
	return matchHere(pattern, s)
}

func matchHere(pattern, s string) bool {
	for {
		if pattern == "" {
			return s == ""
		}
		if pattern[0] == '*' {
			// Peek at the remaining pattern after the run of '*'s.
			rest := strings.TrimLeft(pattern, "*")
			if rest == "" {
				return true // trailing `*` matches whatever's left
			}
			// Try every position in s.
			for i := 0; i <= len(s); i++ {
				if matchHere(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		if s == "" {
			return false
		}
		if pattern[0] != s[0] {
			return false
		}
		pattern = pattern[1:]
		s = s[1:]
	}
}

// MatchPath reports whether pattern matches path using gitignore-style
// semantics. Paths are normalized to forward slashes. `**` matches any
// number of path components; `*` matches within a single component.
//
// A leading `**/` means "at any depth". A pattern like `.env` (no slashes)
// matches a file with that basename anywhere in the tree — matching gitignore.
func MatchPath(pattern, path string) bool {
	pattern = strings.TrimSpace(pattern)
	path = strings.TrimSpace(path)
	if pattern == "" {
		return true
	}
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	path = strings.TrimPrefix(path, "./")

	// Gitignore convention: patterns without a slash match by basename
	// anywhere. `.env` matches `foo/.env`.
	if !strings.Contains(pattern, "/") {
		return pathGlob(pattern, filepath.Base(path))
	}
	// A leading `**/` explicitly means "at any depth" — try matching the
	// tail at every depth.
	if strings.HasPrefix(pattern, "**/") {
		tail := pattern[3:]
		if pathGlob(tail, path) {
			return true
		}
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			if pathGlob(tail, strings.Join(parts[i:], "/")) {
				return true
			}
		}
		return false
	}
	return pathGlob(pattern, path)
}

// pathGlob is a small glob matcher with `**` (any path segments) and `*`
// (any non-slash chars) support.
func pathGlob(pattern, s string) bool {
	for {
		if pattern == "" {
			return s == ""
		}
		// `**` — matches any run, including slashes.
		if strings.HasPrefix(pattern, "**") {
			rest := strings.TrimLeft(pattern, "*")
			rest = strings.TrimPrefix(rest, "/")
			if rest == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if pathGlob(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		// `*` — matches within a single path component (no slash).
		if pattern[0] == '*' {
			rest := pattern[1:]
			if rest == "" {
				return !strings.Contains(s, "/")
			}
			for i := 0; i <= len(s); i++ {
				if i > 0 && s[i-1] == '/' {
					return false
				}
				if pathGlob(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		if s == "" {
			return false
		}
		if pattern[0] != s[0] {
			return false
		}
		pattern = pattern[1:]
		s = s[1:]
	}
}

// MatchDomain reports whether pattern matches host. Patterns look like
// `domain:example.com` or `domain:*.github.com`. If pattern doesn't carry the
// `domain:` prefix we accept it too (some rules just say `*.example.com`).
// Host is the URL host (no scheme, no path).
func MatchDomain(pattern, host string) bool {
	pattern = strings.TrimSpace(pattern)
	host = strings.ToLower(strings.TrimSpace(host))
	if pattern == "" {
		return true
	}
	pattern = strings.ToLower(pattern)
	pattern = strings.TrimPrefix(pattern, "domain:")
	// Strip an optional scheme so callers can pass a URL fragment.
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return domainGlob(pattern, host)
}

func domainGlob(pattern, s string) bool {
	// Domains are matched right-to-left in gitignore-ish terms. A leading
	// `*.` means "any subdomain", and it should also match the bare parent
	// (`*.github.com` matches `github.com`).
	if strings.HasPrefix(pattern, "*.") {
		tail := pattern[2:]
		if s == tail {
			return true
		}
		return strings.HasSuffix(s, "."+tail) && domainGlob("*", s[:len(s)-len(tail)-1])
	}
	// Reuse the bash-style matcher; there are no spaces in host names so
	// the space-boundary semantics don't come into play.
	return matchHere(pattern, s)
}
