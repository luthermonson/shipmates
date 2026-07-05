package project

import (
	"os/exec"
	"strings"
)

// RepoWebURL returns the project's browsable repository URL, derived from the
// git origin remote. Empty when the project isn't a git repo or has no origin.
// The bridge UI uses it to make bead external_refs (gh-<n>) clickable.
func RepoWebURL() string {
	out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return NormalizeGitURL(strings.TrimSpace(string(out)))
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
