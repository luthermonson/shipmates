package brig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

// FreezeMarkerPath returns the on-disk location of the freeze marker inside
// a project's control directory.
func FreezeMarkerPath(projectRoot string) string {
	return filepath.Join(projectRoot, project.Dir, "freeze")
}

// FreezeMarker is the on-disk shape of the freeze marker file.
type FreezeMarker struct {
	Reason    string    `json:"reason"`
	Admiral   string    `json:"admiral"`
	Timestamp time.Time `json:"timestamp"`
}

// CheckFreeze returns (true, marker) when the freeze marker exists and is a
// valid regular file. Returns (false, nil) when the file is absent. Any
// error at the stat/read/parse stage returns (true, nil) — the fail-closed
// default keeps the emergency stop conservative: a marker we cannot read is
// still a marker.
func CheckFreeze(projectRoot string) (bool, *FreezeMarker) {
	path := FreezeMarkerPath(projectRoot)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, nil
	}
	if !info.Mode().IsRegular() {
		return true, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return true, nil
	}
	var m FreezeMarker
	if err := json.Unmarshal(body, &m); err != nil {
		return true, nil
	}
	return true, &m
}

// SetFreeze writes the freeze marker file with the given reason and admiral,
// stamping the current time. Creates the .shipmates directory if missing.
// Overwrites an existing marker (re-freezing is a no-op semantic, but the
// new reason and timestamp replace the old ones).
func SetFreeze(projectRoot, reason, admiral string) error {
	if err := os.MkdirAll(filepath.Join(projectRoot, project.Dir), 0o755); err != nil {
		return fmt.Errorf("brig: create %s: %w", project.Dir, err)
	}
	m := FreezeMarker{Reason: reason, Admiral: admiral, Timestamp: time.Now().UTC()}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("brig: marshal freeze marker: %w", err)
	}
	body = append(body, '\n')
	return os.WriteFile(FreezeMarkerPath(projectRoot), body, 0o600)
}

// ClearFreeze removes the freeze marker if present. Missing marker is not
// an error — release is idempotent.
func ClearFreeze(projectRoot string) error {
	err := os.Remove(FreezeMarkerPath(projectRoot))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
