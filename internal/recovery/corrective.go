package recovery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/luthermonson/shipmates/internal/voyage"
)

var ErrNoAuthorizedTemplate = errors.New("no_authorized_corrective_template")

type CorrectiveLineage struct {
	PredecessorTaskID          string         `json:"predecessor_task_id"`
	PredecessorTaskFingerprint string         `json:"predecessor_task_fingerprint"`
	ResultHash                 string         `json:"result_hash"`
	CriterionIDs               []string       `json:"criterion_ids"`
	TemplateID                 string         `json:"template_id"`
	Evidence                   []CrewEvidence `json:"evidence"`
}

type CorrectiveSuccessor struct {
	Lineage            CorrectiveLineage `json:"lineage"`
	Task               voyage.Task       `json:"task"`
	VerificationTask   voyage.Task       `json:"verification_task"`
	TaskBeadID         string            `json:"task_bead_id"`
	VerificationBeadID string            `json:"verification_bead_id"`
}

func ValidateVerificationNoGo(plan *voyage.Plan, task voyage.Task, result CrewResult) error {
	canonical, err := json.Marshal(plan)
	if err != nil || result.PlanHash != voyage.Hash(canonical) || result.GlobalFingerprint != voyage.GlobalFingerprint(plan) || result.TaskID != task.ID || result.TaskFingerprint != voyage.TaskFingerprint(task) {
		return errors.New("verification_no_go binding mismatch")
	}
	if result.Outcome != OutcomeNoGo || result.Reason != ReasonVerifierNoGo || result.Verifier.Status != VerifierNoGo || result.Verifier.Role != "designated-verifier" {
		return errors.New("verification_no_go requires designated verifier")
	}
	if task.Recovery == nil || !task.Recovery.Enabled {
		return errors.New("structured recovery is not opted in")
	}
	if err := task.Recovery.Validate(); err != nil {
		return err
	}
	approved := make(map[string]bool, len(task.Recovery.ApprovedCriterionIDs))
	for _, id := range task.Recovery.ApprovedCriterionIDs {
		approved[id] = true
	}
	seen := map[string]bool{}
	for _, id := range result.Verifier.CriterionIDs {
		if !approved[id] || seen[id] {
			return errors.New("verifier criterion is not approved")
		}
		seen[id] = true
	}
	if len(seen) == 0 {
		return errors.New("verification_no_go has no approved criterion")
	}
	if result.CorrectiveTemplateID == "" {
		return ErrNoAuthorizedTemplate
	}
	for _, template := range task.Recovery.CorrectiveTemplates {
		if template.ID == result.CorrectiveTemplateID {
			if !sameCriteria(template.CriterionIDs, result.Verifier.CriterionIDs) {
				return errors.New("corrective template criterion substitution")
			}
			return nil
		}
	}
	return ErrNoAuthorizedTemplate
}

func InstantiateCorrective(plan *voyage.Plan, task voyage.Task, result CrewResult, resultCanonical []byte) (CorrectiveSuccessor, error) {
	if plan == nil {
		return CorrectiveSuccessor{}, errors.New("corrective plan is required")
	}
	if err := ValidateVerificationNoGo(plan, task, result); err != nil {
		return CorrectiveSuccessor{}, err
	}
	var template voyage.CorrectiveTemplate
	for _, candidate := range task.Recovery.CorrectiveTemplates {
		if candidate.ID == result.CorrectiveTemplateID {
			template = candidate
			break
		}
	}
	if template.ID == "" {
		return CorrectiveSuccessor{}, ErrNoAuthorizedTemplate
	}
	if len(resultCanonical) == 0 {
		return CorrectiveSuccessor{}, errors.New("corrective result evidence is not immutable")
	}
	resultHash := hashBytes(resultCanonical)
	criteria := append([]string(nil), result.Verifier.CriterionIDs...)
	sort.Strings(criteria)
	lineage := CorrectiveLineage{PredecessorTaskID: task.ID, PredecessorTaskFingerprint: voyage.TaskFingerprint(task), ResultHash: resultHash, CriterionIDs: criteria, TemplateID: template.ID, Evidence: append([]CrewEvidence(nil), result.Verifier.Evidence...)}
	keyBytes, _ := json.Marshal(struct{ L CorrectiveLineage }{lineage})
	key := hashBytes(keyBytes)
	budget := *task.Recovery
	copyBudget := budget
	successor := voyage.Task{ID: "corrective-" + key[:12], Persona: task.Persona, Summary: template.Summary, Prompt: template.Prompt, DependsOn: []string{task.ID}, Models: append([]string(nil), task.Recovery.Models...), Efforts: append([]string(nil), task.Recovery.Efforts...), RetrySafe: true, Recovery: &copyBudget}
	verification := voyage.Task{ID: "verify-" + key[:12], Persona: "verifier", Summary: template.VerificationSummary, Prompt: template.VerificationPrompt, DependsOn: []string{successor.ID}, Models: append([]string(nil), task.Models...), Efforts: append([]string(nil), task.Efforts...), RetrySafe: true}
	return CorrectiveSuccessor{Lineage: lineage, Task: successor, VerificationTask: verification, TaskBeadID: "Shipmates-" + key[:12], VerificationBeadID: "Shipmates-" + key[12:24]}, nil
}

func sameCriteria(a, b []string) bool {
	aa, bb := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func (c CorrectiveSuccessor) Validate() error {
	if !strings.HasPrefix(c.Task.ID, "corrective-") || !strings.HasPrefix(c.VerificationTask.ID, "verify-") || c.TaskBeadID == c.VerificationBeadID || c.Lineage.TemplateID == "" || len(c.Lineage.CriterionIDs) == 0 || len(c.Lineage.ResultHash) != 64 {
		return fmt.Errorf("invalid corrective successor lineage")
	}
	if err := c.Task.Recovery.Validate(); err != nil {
		return err
	}
	return nil
}
