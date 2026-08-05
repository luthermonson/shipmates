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

	"github.com/luthermonson/shipmates/internal/project"
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

type AcceptanceStatus string

const (
	AcceptanceUnset AcceptanceStatus = "unset"
	AcceptancePass  AcceptanceStatus = "pass"
	AcceptanceNoGo  AcceptanceStatus = "no_go"
)

const MaxAcceptanceEvidence = 8

// AcceptanceVerdict is authoritative voyage-level acceptance. It is
// deliberately separate from task completion and Beads evidence.
type AcceptanceVerdict struct {
	Status            AcceptanceStatus `json:"status"`
	RecordedAt        time.Time        `json:"recorded_at,omitempty"`
	EvidenceRefs      []string         `json:"evidence_refs,omitempty"`
	PlanHash          string           `json:"plan_hash,omitempty"`
	GlobalFingerprint string           `json:"global_fingerprint,omitempty"`
}

func (v AcceptanceVerdict) Validate() error {
	if v.Status != AcceptanceUnset && v.Status != AcceptancePass && v.Status != AcceptanceNoGo {
		return errors.New("invalid acceptance verdict")
	}
	if len(v.EvidenceRefs) > MaxAcceptanceEvidence {
		return errors.New("acceptance evidence exceeds bound")
	}
	for _, ref := range v.EvidenceRefs {
		if strings.TrimSpace(ref) == "" || len(ref) > 512 || strings.ContainsAny(ref, "\r\n") {
			return errors.New("invalid acceptance evidence reference")
		}
	}
	if v.Status == AcceptanceUnset {
		if !v.RecordedAt.IsZero() || len(v.EvidenceRefs) != 0 {
			return errors.New("unset acceptance verdict has provenance")
		}
		return nil
	}
	if v.PlanHash != "" && len(v.PlanHash) != 64 {
		return errors.New("invalid acceptance plan binding")
	}
	if v.GlobalFingerprint != "" && len(v.GlobalFingerprint) != 64 {
		return errors.New("invalid acceptance fingerprint binding")
	}
	if v.RecordedAt.IsZero() || len(v.EvidenceRefs) == 0 {
		return errors.New("acceptance verdict requires timestamp and evidence")
	}
	return nil
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
	Acceptance        *AcceptanceVerdict   `json:"acceptance,omitempty"`
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
	// Version 1 predates authoritative acceptance.  Do not let a hand-edited
	// legacy record acquire a verdict merely because the current decoder knows
	// the new field; ApplyAcceptanceOutput performs the explicit v1 -> v2
	// migration when the approved gate emits a valid marker.
	if state.Version == 1 && state.Acceptance != nil {
		return nil, errors.New("legacy voyage state cannot contain acceptance verdict")
	}
	if state.Acceptance != nil && state.Version != 1 {
		if err := state.Acceptance.Validate(); err != nil {
			return nil, err
		}
		if state.Version == StateVersion && state.Acceptance.Status != AcceptanceUnset && (state.Acceptance.PlanHash == "" || state.Acceptance.GlobalFingerprint == "" || state.Acceptance.PlanHash != state.PlanHash || state.Acceptance.GlobalFingerprint != state.GlobalFingerprint) {
			return nil, errors.New("acceptance verdict is not bound to this voyage state")
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
	if state == nil {
		return errors.New("nil voyage state")
	}
	if state.Acceptance != nil && state.Acceptance.Status != AcceptanceUnset {
		// Programmatic state builders receive the same canonical binding as the
		// closed marker path before bytes are persisted. A hand-edited JSON file
		// cannot acquire this binding because LoadState validates exact identity.
		if state.Acceptance.PlanHash == "" {
			state.Acceptance.PlanHash = state.PlanHash
		}
		if state.Acceptance.GlobalFingerprint == "" {
			state.Acceptance.GlobalFingerprint = state.GlobalFingerprint
		}
	}
	if state.Acceptance != nil && state.Version != 1 {
		if err := state.Acceptance.Validate(); err != nil {
			return err
		}
		if state.Version == StateVersion && state.Acceptance.Status != AcceptanceUnset && (state.Acceptance.PlanHash != state.PlanHash || state.Acceptance.GlobalFingerprint != state.GlobalFingerprint) {
			return errors.New("acceptance verdict is not bound to this voyage state")
		}
	}
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
	// project.DurableRename replaces the unix rename+directory-fsync pair with
	// the platform's actual durability primitive. The direct pattern (fsync the
	// containing directory) fails on Windows with "Access is denied" because
	// NTFS refuses FlushFileBuffers on a directory handle.
	return project.DurableRename(name, path)
}

// StateHash identifies the exact bounded predecessor bytes, not merely its
// plan hash. This prevents migration from silently accepting a rewritten
// predecessor state under the same plan revision.
func StateHash(raw []byte) string { return Hash(raw) }

// PublishSuccessor atomically creates a successor state and treats an
// existing identical lineage publication as an idempotent retry. It refuses
// an ambiguous existing path rather than overwriting it.
//
// Concurrent publishers of the same lineage are legal (retries, crashed and
// restarted migrations). On Windows the loser of the replace race can observe
// the winner's in-flight delete-pending file as a transient access/sharing
// error, so those — and only those — are retried within a bounded window.
func PublishSuccessor(path string, state *State, plan *Plan, hash string) (*State, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		published, err := publishSuccessorOnce(path, state, plan, hash)
		if err == nil || !transientFSContention(err) || time.Now().After(deadline) {
			return published, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func publishSuccessorOnce(path string, state *State, plan *Plan, hash string) (*State, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("successor state must be a regular file")
		}
		existing, loadErr := LoadState(path, plan, hash)
		if loadErr != nil && transientFSContention(loadErr) {
			return nil, loadErr
		}
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
