package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

const (
	LiveContinuitySchema  = 1
	CodexAppServerBackend = "codex-app-server"
)

var liveThreadID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LiveContinuity is the complete durable live-session record. Runtime state,
// process IDs, turn IDs, prompts, and lock metadata are intentionally absent.
type LiveContinuity struct {
	SchemaVersion     int    `json:"schema_version"`
	Backend           string `json:"backend"`
	ThreadID          string `json:"thread_id"`
	ConfigFingerprint string `json:"config_fingerprint"`
}

func LiveContinuityMarker(persona string) string {
	return BackendSessionMarker(persona, CodexAppServerBackend)
}

// LiveContinuityMarkerFor is the marker path for a specific live backend.
// Each backend keeps its own marker file, so switching runtimes never
// resumes a thread the other runtime minted.
func LiveContinuityMarkerFor(persona, backend string) string {
	return BackendSessionMarker(persona, backend)
}

// liveBackendName bounds what may appear in a marker's backend field, so a
// hand-edited marker cannot name an arbitrary string.
var liveBackendName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func validateLiveContinuity(m LiveContinuity) error {
	return validateLiveContinuityFor(m, CodexAppServerBackend)
}

func validateLiveContinuityFor(m LiveContinuity, backend string) error {
	if m.SchemaVersion != LiveContinuitySchema || m.Backend != backend || !liveBackendName.MatchString(backend) ||
		!liveThreadID.MatchString(m.ThreadID) || !fingerprintRE.MatchString(m.ConfigFingerprint) {
		return errors.New("invalid live continuity marker")
	}
	return nil
}

// ReadLiveContinuity distinguishes absence from invalid durable state. Invalid
// state is never silently treated as a fresh session.
func ReadLiveContinuity(persona string) (LiveContinuity, bool, error) {
	return ReadLiveContinuityAt(".", persona)
}

func ReadLiveContinuityAt(root, persona string) (LiveContinuity, bool, error) {
	return ReadLiveContinuityBackendAt(root, persona, CodexAppServerBackend)
}

// ReadLiveContinuityBackendAt reads the marker for one live backend.
func ReadLiveContinuityBackendAt(root, persona, backend string) (LiveContinuity, bool, error) {
	if err := ValidatePersonaName(persona); err != nil {
		return LiveContinuity{}, false, err
	}
	if !liveBackendName.MatchString(backend) {
		return LiveContinuity{}, false, errors.New("invalid live backend name")
	}
	b, err := os.ReadFile(filepath.Join(root, LiveContinuityMarkerFor(persona, backend)))
	if errors.Is(err, os.ErrNotExist) {
		return LiveContinuity{}, false, nil
	}
	if err != nil {
		return LiveContinuity{}, false, err
	}
	var m LiveContinuity
	if json.Unmarshal(b, &m) != nil || validateLiveContinuityFor(m, backend) != nil {
		return LiveContinuity{}, false, fmt.Errorf("stored live continuity for %q is invalid", persona)
	}
	return m, true, nil
}

// WriteLiveContinuity atomically replaces the marker only with a fully
// validated, confirmed thread identity. The old marker survives every failure
// before rename; fsync makes the rename durable before success is returned.
func WriteLiveContinuity(persona string, m LiveContinuity) error {
	return WriteLiveContinuityAt(".", persona, m)
}

func WriteLiveContinuityAt(root, persona string, m LiveContinuity) error {
	return WriteLiveContinuityBackendAt(root, persona, CodexAppServerBackend, m)
}

// WriteLiveContinuityBackendAt writes the marker for one live backend.
func WriteLiveContinuityBackendAt(root, persona, backend string, m LiveContinuity) error {
	if err := ValidatePersonaName(persona); err != nil {
		return err
	}
	if err := validateLiveContinuityFor(m, backend); err != nil {
		return err
	}
	sessionsDir := filepath.Join(root, SessionsDir())
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(sessionsDir, ".live-continuity-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	fail := func(e error) error { _ = f.Close(); return e }
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, LiveContinuityMarkerFor(persona, backend))); err != nil {
		return err
	}
	d, err := os.Open(filepath.Clean(sessionsDir))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
