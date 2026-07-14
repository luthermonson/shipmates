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
const StateVersion = 2

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
	Status                 Status         `json:"status"`
	Attempt                int            `json:"attempt"`
	StartedAt              time.Time      `json:"started_at,omitempty"`
	FinishedAt             time.Time      `json:"finished_at,omitempty"`
	Summary                string         `json:"summary,omitempty"`
	Error                  string         `json:"error,omitempty"`
	BeadID                 string         `json:"bead_id,omitempty"`
	BeadDependenciesLinked bool           `json:"bead_dependencies_linked,omitempty"`
	TaskFingerprint        string         `json:"task_fingerprint,omitempty"`
	ClosureFingerprint     string         `json:"closure_fingerprint,omitempty"`
	EvidenceRefs           []string       `json:"evidence_refs,omitempty"`
	Inherited              *InheritedTask `json:"inherited,omitempty"`
}

// InheritedTask preserves the original completion evidence and opaque Bead
// while making the predecessor revision visible in successor status.
type InheritedTask struct {
	PredecessorPlanHash string    `json:"predecessor_plan_hash"`
	PredecessorTaskID   string    `json:"predecessor_task_id"`
	TaskFingerprint     string    `json:"task_fingerprint"`
	ClosureFingerprint  string    `json:"closure_fingerprint"`
	Summary             string    `json:"summary"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	FinishedAt          time.Time `json:"finished_at"`
	EvidenceRefs        []string  `json:"evidence_refs,omitempty"`
	OriginalBeadID      string    `json:"original_bead_id,omitempty"`
}

type Lineage struct {
	PredecessorPlanHash  string    `json:"predecessor_plan_hash"`
	PredecessorStateHash string    `json:"predecessor_state_hash"`
	CreatedAt            time.Time `json:"created_at"`
}

type State struct {
	Version           int                  `json:"version"`
	PlanHash          string               `json:"plan_hash"`
	Title             string               `json:"title"`
	StartedAt         time.Time            `json:"started_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	GlobalFingerprint string               `json:"global_fingerprint,omitempty"`
	Lineage           *Lineage             `json:"lineage,omitempty"`
	Tasks             map[string]TaskState `json:"tasks"`
}

func NewState(plan *Plan, hash string) *State {
	now := time.Now().UTC()
	local, closure, _ := TaskFingerprints(plan)
	s := &State{Version: StateVersion, PlanHash: hash, Title: plan.Title, StartedAt: now, UpdatedAt: now, GlobalFingerprint: GlobalFingerprint(plan), Tasks: map[string]TaskState{}}
	for _, task := range plan.Tasks {
		s.Tasks[task.ID] = TaskState{Status: Pending, TaskFingerprint: local[task.ID], ClosureFingerprint: closure[task.ID]}
	}
	return s
}

func LoadState(path string, plan *Plan, hash string) (*State, error) {
	return loadState(path, plan, hash, true)
}

// LoadStateStrict is used by lineage migration: a running predecessor is
// ambiguous evidence and must not be silently recovered to pending.
func LoadStateStrict(path string, plan *Plan, hash string) (*State, error) {
	return loadState(path, plan, hash, false)
}

func loadState(path string, plan *Plan, hash string, recoverRunning bool) (*State, error) {
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
	if (state.Version != 1 && state.Version != StateVersion) || state.PlanHash != hash || state.Tasks == nil {
		return nil, errors.New("voyage state does not match approved plan")
	}
	if state.Version == StateVersion {
		if len(state.GlobalFingerprint) != 64 {
			return nil, errors.New("voyage state has invalid global fingerprint")
		}
		if state.Lineage != nil && (len(state.Lineage.PredecessorPlanHash) != 64 || len(state.Lineage.PredecessorStateHash) != 64 || state.Lineage.CreatedAt.IsZero()) {
			return nil, errors.New("voyage state has invalid lineage")
		}
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
		if len(entry.Summary) > 8192 || len(entry.Error) > 2048 || len(entry.TaskFingerprint) > 64 || len(entry.ClosureFingerprint) > 64 || len(entry.EvidenceRefs) > 8 {
			return nil, fmt.Errorf("voyage state task %q exceeds text bounds", task.ID)
		}
		if len(entry.BeadID) > 128 {
			return nil, fmt.Errorf("voyage state task %q has invalid bead id", task.ID)
		}
		if entry.Attempt < 0 || entry.Attempt >= task.TierCount() {
			return nil, fmt.Errorf("voyage state task %q has invalid attempt", task.ID)
		}
		for _, ref := range entry.EvidenceRefs {
			if strings.TrimSpace(ref) == "" || len(ref) > 512 {
				return nil, fmt.Errorf("voyage state task %q has invalid evidence reference", task.ID)
			}
		}
		if entry.Inherited != nil {
			if err := validateInherited(entry.Inherited); err != nil {
				return nil, fmt.Errorf("voyage state task %q has invalid inheritance: %w", task.ID, err)
			}
		}
		if entry.Status == Running && recoverRunning {
			entry.Status = Pending
			entry.Error = "previous sail stopped while task was running"
			state.Tasks[task.ID] = entry
		}
	}
	return &state, nil
}

func validateInherited(in *InheritedTask) error {
	if len(in.PredecessorPlanHash) != 64 || len(in.PredecessorTaskID) == 0 || len(in.TaskFingerprint) != 64 || len(in.ClosureFingerprint) != 64 || in.FinishedAt.IsZero() || len(in.Summary) > 8192 || len(in.EvidenceRefs) > 8 || len(in.OriginalBeadID) > 128 {
		return errors.New("invalid inherited provenance")
	}
	for _, ref := range in.EvidenceRefs {
		if strings.TrimSpace(ref) == "" || len(ref) > 512 {
			return errors.New("invalid inherited evidence reference")
		}
	}
	return nil
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

// StateHash identifies the exact bounded predecessor bytes, not merely its
// plan hash. This prevents migration from silently accepting a rewritten
// predecessor state under the same plan revision.
func StateHash(raw []byte) string { return Hash(raw) }

// PublishSuccessor atomically creates a successor state and treats an
// existing identical lineage publication as an idempotent retry. It refuses
// an ambiguous existing path rather than overwriting it.
func PublishSuccessor(path string, state *State, plan *Plan, hash string) (*State, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("successor state must be a regular file")
		}
		existing, loadErr := LoadState(path, plan, hash)
		if loadErr != nil || existing.Lineage == nil || state.Lineage == nil || existing.Lineage.PredecessorPlanHash != state.Lineage.PredecessorPlanHash || existing.Lineage.PredecessorStateHash != state.Lineage.PredecessorStateHash {
			return nil, errors.New("successor state already exists with different lineage")
		}
		// An existing successor is active execution state, not predecessor
		// evidence. Persist LoadState's running-to-pending recovery so a Sail
		// process lost between dispatch and completion can be resumed safely.
		if err := SaveState(path, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := SaveState(path, state); err != nil {
		return nil, err
	}
	return state, nil
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
