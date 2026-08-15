package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The captain's coordination server binds loopback only, but loopback is not
// a permission: every process on the box can reach it, and so can any web page
// the operator happens to visit. The port is not a secret either — it is
// written into the repo tree. What actually gates the API is a bearer token
// minted fresh for each server run and left here for local clients to read.
//
// TokenFile sits next to the port and pid files and has the same lifetime:
// written at startup, removed on shutdown.
func TokenFile() string { return filepath.Join(SessionsDir(), "server.token") }

// NewAPIToken mints a 256-bit token from crypto/rand. Callers must treat a
// non-nil error as fatal: a server with no token must refuse to start rather
// than serve its API unauthenticated.
func NewAPIToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// WriteAPIToken records the running server's token for local clients.
func WriteAPIToken(tok string) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return err
	}
	return WritePrivateFile(TokenFile(), []byte(tok))
}

// ReadAPIToken returns the running server's token. A missing file means no
// captain is running (or it is mid-startup); an empty one is corruption and
// is reported as an error rather than handed on as a credential.
func ReadAPIToken() (string, error) {
	b, err := os.ReadFile(TokenFile())
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("empty api token in %s", TokenFile())
	}
	return tok, nil
}

// WritePrivateFile writes data with 0600, replacing any existing file rather
// than writing through it — os.WriteFile does not re-apply the mode to a file
// that already exists, so a stale world-readable copy left by a crashed run
// would otherwise survive the rewrite.
//
// 0600 is honest on unix. On Windows Go synthesizes permission bits and only
// maps the write bit onto the read-only attribute, so the mode argument buys
// nothing there; access is decided by the inherited DACL of the .shipmates
// directory, which in any normal profile layout is already owner-only. That
// is the same position internal/recovery takes for its journal
// (journal_perm_windows.go) — state the limitation rather than pretend the
// mode applies.
func WritePrivateFile(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
