package brig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

// DenialLogPath returns the on-disk location of the append-only JSONL
// denial log.
func DenialLogPath(projectRoot string) string {
	return filepath.Join(projectRoot, project.Dir, "brig.log")
}

// Denial is one parsed entry from brig.log.
type Denial struct {
	Timestamp time.Time `json:"ts"`
	Persona   string    `json:"persona"`
	Rule      int       `json:"rule"`
	Command   string    `json:"command"`
}

// LogDenial appends one JSONL entry to the project's brig.log, describing a
// Brig refusal. Creates the file if missing. Best-effort: this must never
// fail the enforcement path — an error is returned so the caller can
// surface it, but the caller must proceed to refuse the command either way.
//
// The log is history: disabling the brig stops new entries but never erases
// old ones.
func LogDenial(projectRoot, persona string, rule int, command string) error {
	if err := os.MkdirAll(filepath.Join(projectRoot, project.Dir), 0o755); err != nil {
		return fmt.Errorf("brig: create %s: %w", project.Dir, err)
	}
	entry := Denial{
		Timestamp: time.Now().UTC(),
		Persona:   persona,
		Rule:      rule,
		Command:   command,
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("brig: marshal denial: %w", err)
	}
	body = append(body, '\n')
	f, err := os.OpenFile(DenialLogPath(projectRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("brig: open denial log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("brig: append denial: %w", err)
	}
	return nil
}

// ReadDenials reads the project's brig.log and returns the parsed entries in
// file order. Returns an empty slice (no error) when the log is missing.
// Lines that don't parse are skipped silently — the log is append-only and
// old entries from a prior schema version should not stop the newer reader.
func ReadDenials(projectRoot string) ([]Denial, error) {
	body, err := os.ReadFile(DenialLogPath(projectRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Denial
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Denial
		if err := json.Unmarshal(line, &d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}
