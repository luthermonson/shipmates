// Package personaname holds shipmates' one spelling of a legal persona name.
//
// A persona name is not free text: it is joined into filesystem paths
// (.claude/agents/<name>.md), passed to child processes as a flag value
// (--agent <name>), and interpolated into HTTP paths proxied over the fleet
// tunnel (/tell/<name>, /pty/<name>/start). Each of those is a place where a
// separator, a parent-directory hop, a leading dash, or whitespace/CRLF turns
// a name into something else entirely.
//
// The rule lives here — rather than as a private regexp in each package that
// needs it — so the packages that gate on it cannot drift apart. Both
// internal/runtime/claude and internal/runtime/codex still carry their own
// copies of this regexp with a comment saying they should delegate to a
// canonical validator once one exists; this package is that validator, and
// collapsing them onto it is a follow-up.
package personaname

import (
	"fmt"
	"regexp"
)

// nameRE is the canonical rule: lowercase-first, then lowercase letters,
// digits, hyphens and underscores. Nothing that can be a path separator, a
// parent-directory hop, a leading flag dash, whitespace, or HTTP request-line
// framing (CR/LF) survives it.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Valid reports whether persona is a legal persona name.
func Valid(persona string) bool {
	return nameRE.MatchString(persona)
}

// Validate is Valid with an error message suitable for showing a human or an
// LLM that guessed a name.
func Validate(persona string) error {
	if !Valid(persona) {
		return fmt.Errorf("invalid persona %q (use lowercase letters, digits, hyphens, or underscores)", persona)
	}
	return nil
}
