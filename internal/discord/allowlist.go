package discord

import "strings"

// Allowlist is the set of Discord user ids permitted to command the mate. It is
// fail-closed by construction: only ids explicitly present are allowed, and an
// empty Allowlist allows nobody (not everybody).
type Allowlist map[string]bool

// ParseAllowlist builds an Allowlist from a comma-separated list of Discord
// user ids (the form the operator writes into SHIPMATES_DISCORD_ALLOWED_USERS).
// Whitespace around each id is trimmed and empty fields are ignored. An empty
// or whitespace-only input yields an empty, non-nil set — which rejects
// everyone. There is no wildcard: "allow everyone" is deliberately not
// expressible.
func ParseAllowlist(raw string) Allowlist {
	out := Allowlist{}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		out[id] = true
	}
	return out
}

// Allows reports whether a message from userID may be dispatched as a tell. It
// is the single decision point for the fail-closed rule: an unlisted id, an
// empty id, or an empty allowlist all return false. A false result means the
// message is treated as read-only and never dispatched.
func (a Allowlist) Allows(userID string) bool {
	id := strings.TrimSpace(userID)
	if id == "" {
		return false
	}
	return a[id]
}
