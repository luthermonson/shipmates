package project

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoWebURL returns the project's browsable repository URL, derived from the
// git origin remote. Empty when the project isn't a git repo or has no origin.
// The fleet UI uses it to make bead external_refs (gh-<n>) clickable.
//
// It reads .git/config directly rather than exec'ing git: supervised leads
// run in service-ish environments (Scheduled Task at logon) where the git
// subprocess silently failed while everything else worked.
func RepoWebURL() string {
	if u := originFromGitConfig("."); u != "" {
		return NormalizeGitURL(u)
	}
	// fall back to git itself for layouts the parser doesn't know (worktrees
	// with commondir indirection, includes, …)
	out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return NormalizeGitURL(strings.TrimSpace(string(out)))
}

// originFromGitConfig scans dir/.git/config for [remote "origin"]'s url.
// Handles the plain-clone case; returns "" for anything fancier.
func originFromGitConfig(dir string) string {
	gitPath := filepath.Join(dir, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if !fi.IsDir() { // worktree pointer file — let the git fallback handle it
		return ""
	}
	f, err := os.Open(filepath.Join(gitPath, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	inOrigin := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if inOrigin {
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "url" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// NormalizeGitURL converts a git remote URL (ssh, scp-like, git://, or https
// with .git suffix) to its https browse form. Unrecognized shapes return "".
func NormalizeGitURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// scp-like: git@github.com:user/repo(.git)
	if !strings.Contains(raw, "://") {
		if i := strings.Index(raw, "@"); i >= 0 {
			rest := raw[i+1:]
			host, path, ok := strings.Cut(rest, ":")
			if !ok || host == "" || path == "" {
				return ""
			}
			return "https://" + host + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git")
		}
		return ""
	}
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if !strings.HasPrefix(raw, scheme) {
			continue
		}
		rest := strings.TrimPrefix(raw, scheme)
		if i := strings.Index(rest, "@"); i >= 0 && i < strings.Index(rest+"/", "/") {
			rest = rest[i+1:] // drop user@ credential
		}
		host, path, ok := strings.Cut(rest, "/")
		if !ok || host == "" || path == "" {
			return ""
		}
		// ssh often carries the port (host:22) — the browse URL never does
		host, _, _ = strings.Cut(host, ":")
		return "https://" + host + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	}
	return ""
}
