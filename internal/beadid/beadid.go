// Package beadid holds shipmates' one spelling of a legal bead id.
//
// A bead id is not free text. It is interpolated into an HTTP path segment on
// the fleet side (/api/captain/{key}/bead/{id}/update, then /bead/%s/update
// through the tunnel) and it becomes argv on the ship side (`bd show <id>`).
// Each of those is a place where a separator, a parent-directory hop, a
// leading dash, or whitespace/CRLF turns an id into something else.
//
// The rule lives in its own package — rather than as an unexported helper in
// both internal/server and internal/fleet — because it was previously
// duplicated, the two copies drifted, and the ship-side copy still accepted
// ".." months after the fleet-side copy had been fixed. Two copies of one
// guard is how the original bug returns. This package is the same shape as
// internal/personaname and exists for the same reason.
package beadid

import "fmt"

// maxLen bounds an id. Real bd ids are prefix-hashes (proj-c03, with dotted
// epics like proj-a3f8.1); 64 bytes is already generous.
const maxLen = 64

// Valid reports whether id is a legal bead id.
//
// The alphabet alone is not enough. url.PathEscape leaves "." untouched, so
// "/bead/../update" would survive as a traversal into a different endpoint if
// the alphabet check were the only guard — and "." must stay in the alphabet
// because dotted epic ids use it. Real ids are hashes and never contain "..",
// so rejecting that sequence outright costs nothing.
//
// A leading "-" is refused separately: it is the difference between an
// argument and a flag once the id reaches `bd show <id>`.
func Valid(id string) bool {
	if id == "" || len(id) > maxLen {
		return false
	}
	if id[0] == '-' {
		return false
	}
	for i := 0; i+1 < len(id); i++ {
		if id[i] == '.' && id[i+1] == '.' {
			return false
		}
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

// Validate is Valid with an error message suitable for showing a human or an
// LLM that guessed an id.
func Validate(id string) error {
	if !Valid(id) {
		return fmt.Errorf("invalid bead id %q (letters, digits, '-', '.', '_'; no leading dash, no \"..\")", id)
	}
	return nil
}
