package claude

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// mintSessionID returns a UUIDv4-shaped opaque id suitable for Claude
// Code's --session-id flag. Format: 8-4-4-4-12 lowercase hex.
func mintSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

// mintTurnID returns a compact random hex string for internal correlation
// (not passed to Claude Code — it doesn't have per-turn identifiers on the
// CLI surface today).
func mintTurnID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
