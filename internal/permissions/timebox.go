package permissions

import (
	"strings"
	"sync"
	"time"
)

// TimeBox is a temporary "auto-allow this exact tool call for the next N
// minutes" grant, created when the operator approves a pending request with a
// --for flag. Scoped per persona so approving `pnpm test unit` for the
// backend crew doesn't quietly re-approve it for the security crew.
//
// Storage is in-memory: a ship restart wipes every time-box. That's a feature,
// not a bug — a fresh boot is a fresh trust boundary.
type TimeBox struct {
	Tool      string    // "Bash", "PowerShell", "Read", …
	Pattern   string    // canonical pattern (see canonicalPattern)
	ExpiresAt time.Time // absolute expiry; lazy-swept on read
}

// timeboxStore is the ship-wide keyring: persona -> canonical key -> TimeBox.
// Entries are looked up on every Bash evaluation, so the lock is a straight
// mutex (a sync.Map buys nothing at this scale and complicates iteration).
type timeboxStore struct {
	mu      sync.Mutex
	entries map[string]map[string]TimeBox
}

func newTimeboxStore() *timeboxStore {
	return &timeboxStore{entries: map[string]map[string]TimeBox{}}
}

// add registers a time-box for (persona, tool, command). command is the
// operator-visible summary the pending request carried (for Bash, the raw
// command line). Overwrites any prior grant for the same key.
func (t *timeboxStore) add(persona, tool, command string, duration time.Duration) TimeBox {
	pattern := canonicalPattern(tool, command)
	tb := TimeBox{
		Tool:      tool,
		Pattern:   pattern,
		ExpiresAt: time.Now().Add(duration),
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.entries[persona]
	if m == nil {
		m = map[string]TimeBox{}
		t.entries[persona] = m
	}
	m[timeboxKey(tool, pattern)] = tb
	return tb
}

// lookup returns an active time-box that matches this call, if one exists.
// Expired entries are dropped on the way through, so no separate sweeper is
// needed.
func (t *timeboxStore) lookup(persona, tool, command string) (TimeBox, bool) {
	pattern := canonicalPattern(tool, command)
	key := timeboxKey(tool, pattern)
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.entries[persona]
	if m == nil {
		return TimeBox{}, false
	}
	tb, ok := m[key]
	if !ok {
		return TimeBox{}, false
	}
	if time.Now().After(tb.ExpiresAt) {
		delete(m, key)
		if len(m) == 0 {
			delete(t.entries, persona)
		}
		return TimeBox{}, false
	}
	return tb, true
}

// canonicalPattern reduces a tool call to the key we use for time-box matching.
//
// For Bash/PowerShell we anchor on the EXACT command that was approved: the
// operator said yes to `pnpm test unit`, not to all of `pnpm *`. Broader
// time-boxes (allow all `pnpm test *` for 30 min) are a documented Phase 3
// extension; keeping Phase 2 tight avoids a "I said 5 minutes on THIS command,
// why did it also allow rm -rf" surprise.
//
// For path tools we anchor on the file path.
// For anything else we fall back to the raw command string.
func canonicalPattern(tool, command string) string {
	command = strings.TrimSpace(command)
	switch tool {
	case "Bash", "PowerShell":
		return normalizeWhitespace(command)
	default:
		return command
	}
}

// normalizeWhitespace collapses internal runs of whitespace to single spaces
// so `pnpm  test` and `pnpm test` share a time-box. Leading/trailing space is
// already trimmed by canonicalPattern's caller.
func normalizeWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

func timeboxKey(tool, pattern string) string {
	return tool + "\x00" + pattern
}
