package voyage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaxStateBytes = 4 << 20

type Status string

const (
	Pending    Status = "pending"
	Running    Status = "running"
	Completed  Status = "completed"
	Failed     Status = "failed"
	Blocked    Status = "blocked"
	NeedsInput Status = "needs_input"
)

type TaskState struct {
	Status                 Status    `json:"status"`
	Attempt                int       `json:"attempt"`
	StartedAt              time.Time `json:"started_at,omitempty"`
	FinishedAt             time.Time `json:"finished_at,omitempty"`
	Summary                string    `json:"summary,omitempty"`
	Error                  string    `json:"error,omitempty"`
	BeadID                 string    `json:"bead_id,omitempty"`
	BeadDependenciesLinked bool      `json:"bead_dependencies_linked,omitempty"`
}

type State struct {
	Version   int                  `json:"version"`
	PlanHash  string               `json:"plan_hash"`
	Title     string               `json:"title"`
	StartedAt time.Time            `json:"started_at"`
	UpdatedAt time.Time            `json:"updated_at"`
	Tasks     map[string]TaskState `json:"tasks"`
}

func NewState(plan *Plan, hash string) *State {
	now := time.Now().UTC()
	s := &State{Version: 1, PlanHash: hash, Title: plan.Title, StartedAt: now, UpdatedAt: now, Tasks: map[string]TaskState{}}
	for _, task := range plan.Tasks {
		s.Tasks[task.ID] = TaskState{Status: Pending}
	}
	return s
}

func LoadState(path string, plan *Plan, hash string) (*State, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(plan, hash), nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > MaxStateBytes {
		return nil, errors.New("voyage state must be a bounded regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
		return nil, errors.New("voyage state contains trailing content")
	}
	if state.Version != 1 || state.PlanHash != hash || state.Tasks == nil {
		return nil, errors.New("voyage state does not match approved plan")
	}
	if len(state.Tasks) != len(plan.Tasks) {
		return nil, errors.New("voyage state task set does not match approved plan")
	}
	for _, task := range plan.Tasks {
		if _, ok := state.Tasks[task.ID]; !ok {
			return nil, errors.New("voyage state is missing a task")
		}
		entry := state.Tasks[task.ID]
		switch entry.Status {
		case Pending, Running, Completed, Failed, Blocked, NeedsInput:
		default:
			return nil, fmt.Errorf("voyage state task %q has invalid status", task.ID)
		}
		if len(entry.Summary) > 8192 || len(entry.Error) > 2048 {
			return nil, fmt.Errorf("voyage state task %q exceeds text bounds", task.ID)
		}
		if len(entry.BeadID) > 128 {
			return nil, fmt.Errorf("voyage state task %q has invalid bead id", task.ID)
		}
		if entry.Attempt < 0 || entry.Attempt >= task.TierCount() {
			return nil, fmt.Errorf("voyage state task %q has invalid attempt", task.ID)
		}
		if entry.Status == Running {
			entry.Status = Pending
			entry.Error = "previous sail stopped while task was running"
			state.Tasks[task.ID] = entry
		}
	}
	return &state, nil
}

func SaveState(path string, state *State) error {
	state.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := ensureStateDirectory(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".voyage-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func ensureStateDirectory(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(os.PathSeparator)
	rest := strings.TrimPrefix(abs, current)
	for _, part := range strings.Split(rest, string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("voyage state directory must not contain symlinks: %s", current)
		}
	}
	return nil
}
