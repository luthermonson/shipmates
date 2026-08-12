package voyage

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaxLineagePathBytes = MaxPlanBytes

// MigrateSuccessor validates one explicit, commissioned predecessor and prepares
// a separately hashed successor. The predecessor plan/state are read-only;
// only the successor state path may be created.
func MigrateSuccessor(successorPlanPath, predecessorPlanPath, predecessorStatePath, successorStatePath string, now time.Time) (*State, error) {
	state, successor, successorHash, err := buildSuccessor(successorPlanPath, predecessorPlanPath, predecessorStatePath, now)
	if err != nil {
		return nil, err
	}
	return PublishSuccessor(successorStatePath, state, successor, successorHash)
}

// MigrateSuccessorPreview performs the same validation and inheritance
// calculation without publishing a successor state.
func MigrateSuccessorPreview(successorPlanPath, predecessorPlanPath, predecessorStatePath string, now time.Time) (*State, error) {
	state, _, _, err := buildSuccessor(successorPlanPath, predecessorPlanPath, predecessorStatePath, now)
	return state, err
}

func buildSuccessor(successorPlanPath, predecessorPlanPath, predecessorStatePath string, now time.Time) (*State, *Plan, string, error) {
	for _, path := range []string{successorPlanPath, predecessorPlanPath, predecessorStatePath} {
		if err := validateLineagePath(path); err != nil {
			return nil, nil, "", err
		}
	}
	successor, successorCanonical, err := Load(successorPlanPath)
	if err != nil {
		return nil, nil, "", errors.New("successor plan is invalid or uncommissioned")
	}
	predecessor, predecessorCanonical, err := Load(predecessorPlanPath)
	if err != nil {
		return nil, nil, "", errors.New("predecessor plan is invalid or uncommissioned")
	}
	predecessorHash := Hash(predecessorCanonical)
	successorHash := Hash(successorCanonical)
	if predecessorHash == successorHash {
		return nil, nil, "", errors.New("successor must be a distinct plan revision")
	}
	predecessorState, err := LoadStateStrict(predecessorStatePath, predecessor, predecessorHash)
	if err != nil {
		return nil, nil, "", errors.New("predecessor state is invalid")
	}
	predecessorBytes, err := os.ReadFile(predecessorStatePath)
	if err != nil || len(predecessorBytes) > MaxStateBytes {
		return nil, nil, "", errors.New("predecessor state is unavailable")
	}
	predecessorStateHash := StateHash(predecessorBytes)
	local, closure, err := TaskFingerprints(successor)
	if err != nil {
		return nil, nil, "", err
	}
	oldLocal, oldClosure, err := TaskFingerprints(predecessor)
	if err != nil {
		return nil, nil, "", errors.New("predecessor task fingerprints are invalid")
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}
	state := &State{Version: StateVersion, PlanHash: successorHash, Title: successor.Title, StartedAt: now, UpdatedAt: now, GlobalFingerprint: GlobalFingerprint(successor), Lineage: &Lineage{PredecessorPlanHash: predecessorHash, PredecessorStateHash: predecessorStateHash, CreatedAt: now}, Tasks: make(map[string]TaskState, len(successor.Tasks))}
	for _, task := range successor.Tasks {
		entry := TaskState{Status: Pending, TaskFingerprint: local[task.ID], ClosureFingerprint: closure[task.ID]}
		old, exists := predecessorState.Tasks[task.ID]
		if exists && old.Inherited != nil && (old.Inherited.PredecessorPlanHash != predecessorHash || old.Inherited.PredecessorTaskID != task.ID || old.Inherited.TaskFingerprint != old.TaskFingerprint || old.Inherited.ClosureFingerprint != old.ClosureFingerprint || old.Inherited.OriginalBeadID != old.BeadID) {
			return nil, nil, "", errors.New("predecessor state has stale lineage evidence")
		}
		if exists && GlobalFingerprint(successor) == GlobalFingerprint(predecessor) && old.Status == Completed && validCompletionEvidence(old) && oldLocal[task.ID] == local[task.ID] && oldClosure[task.ID] == closure[task.ID] {
			entry = inheritedEntry(old, predecessorHash, task.ID, oldLocal[task.ID], oldClosure[task.ID])
		}
		state.Tasks[task.ID] = entry
	}
	return state, successor, successorHash, nil
}

func inheritedEntry(old TaskState, predecessorHash, taskID, taskFingerprint, closureFingerprint string) TaskState {
	provenance := old.Inherited
	if provenance == nil {
		provenance = &InheritedTask{PredecessorPlanHash: predecessorHash, PredecessorTaskID: taskID, TaskFingerprint: taskFingerprint, ClosureFingerprint: closureFingerprint, Summary: old.Summary, StartedAt: old.StartedAt, FinishedAt: old.FinishedAt, EvidenceRefs: append([]string(nil), old.EvidenceRefs...), OriginalBeadID: old.BeadID}
	}
	copy := old
	copy.Status = Completed
	copy.Error = ""
	copy.TaskFingerprint = taskFingerprint
	copy.ClosureFingerprint = closureFingerprint
	copy.Inherited = provenance
	// Linking an inherited task would mutate its predecessor Bead. Its
	// existing dependency edges are authoritative and must remain untouched.
	copy.BeadDependenciesLinked = true
	return copy
}

func validCompletionEvidence(entry TaskState) bool {
	if entry.Status != Completed || entry.FinishedAt.IsZero() || len(entry.Summary) > 8192 || len(entry.Error) != 0 {
		return false
	}
	if entry.Inherited != nil {
		return validateInherited(entry.Inherited) == nil
	}
	return true
}

func validateLineagePath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("lineage path must be absolute and clean")
	}
	// A lineage path must arrive fully resolved: resolution is the caller's
	// job, because only the caller knows the trust boundary its gate
	// enforces — commands EvalSymlinks operator-supplied paths and require
	// the result to stay under the project root (safeVoyagePlanPath,
	// safeVoyageStatePath). Requiring the resolved form here proves no
	// component traverses a symlink without this package policing ancestors
	// it cannot judge: the old walk to the filesystem root refused macOS's
	// own /var -> private/var symlink and with it every path under the
	// system temp directory.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return errors.New("lineage path must be fully resolved (no symlinks)")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > MaxLineagePathBytes {
		return errors.New("lineage input must be a bounded regular file")
	}
	return nil
}

// ValidateLineageJSON is a small closed-schema helper used by command tests
// and diagnostics; migration itself decodes through the plan/state loaders.
func ValidateLineageJSON(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in Lineage
	if err := dec.Decode(&in); err != nil {
		return err
	}
	if strings.TrimSpace(in.PredecessorPlanHash) == "" || strings.TrimSpace(in.PredecessorStateHash) == "" {
		return errors.New("lineage hashes are required")
	}
	return nil
}
