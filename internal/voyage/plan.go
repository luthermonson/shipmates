package voyage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

const MaxPlanBytes = 1 << 20

var taskIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Plan struct {
	Version            int      `json:"version"`
	Title              string   `json:"title"`
	Objective          string   `json:"objective"`
	Scope              []string `json:"scope,omitempty"`
	NonGoals           []string `json:"non_goals,omitempty"`
	BlastArea          []string `json:"blast_area,omitempty"`
	Risks              []string `json:"risks,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	OpenDecisions      []string `json:"open_decisions,omitempty"`
	// AcceptanceGateTask is the sole task authorized to publish the explicit
	// final acceptance marker. Empty preserves legacy plans, whose acceptance
	// remains unknown rather than implicitly passing.
	AcceptanceGateTask string `json:"acceptance_gate_task,omitempty"`
	Approved           bool   `json:"approved"`
	Tasks              []Task `json:"tasks"`
}

type Task struct {
	ID        string        `json:"id"`
	Persona   string        `json:"persona"`
	Summary   string        `json:"summary"`
	Prompt    string        `json:"prompt"`
	DependsOn []string      `json:"depends_on"`
	Models    []string      `json:"models,omitempty"`
	Efforts   []string      `json:"efforts,omitempty"`
	RetrySafe bool          `json:"retry_safe,omitempty"`
	Recovery  *RecoveryTask `json:"recovery,omitempty"`
}

// RecoveryTask is an opt-in, immutable contract for future structured Sail
// recovery. A nil value preserves the legacy process-error execution model.
type RecoveryTask struct {
	Enabled                  bool                 `json:"enabled"`
	MaxAttempts              uint8                `json:"max_attempts"`
	MaxInfrastructureRetries uint8                `json:"max_infrastructure_retries"`
	MaxTokens                uint32               `json:"max_tokens"`
	Models                   []string             `json:"models"`
	Efforts                  []string             `json:"efforts"`
	CorrectiveTemplates      []CorrectiveTemplate `json:"corrective_templates"`
	ApprovedCriterionIDs     []string             `json:"approved_criterion_ids"`
}

type CorrectiveTemplate struct {
	ID                  string   `json:"id"`
	Summary             string   `json:"summary"`
	Prompt              string   `json:"prompt"`
	VerificationSummary string   `json:"verification_summary"`
	VerificationPrompt  string   `json:"verification_prompt"`
	CriterionIDs        []string `json:"criterion_ids"`
	RetrySafe           bool     `json:"retry_safe"`
}

func (r *RecoveryTask) Validate() error {
	if r == nil {
		return nil
	}
	if !r.Enabled {
		return errors.New("recovery contract must be explicitly enabled or omitted")
	}
	if r.MaxAttempts == 0 || r.MaxAttempts > 8 || r.MaxInfrastructureRetries > 8 || r.MaxTokens == 0 || r.MaxTokens > 1<<20 {
		return errors.New("recovery contract budget is out of bounds")
	}
	if len(r.Models) == 0 || len(r.Models) > 4 || len(r.Efforts) == 0 || len(r.Efforts) > 4 || len(r.CorrectiveTemplates) > 8 {
		return errors.New("recovery contract tiers or templates are out of bounds")
	}
	if r.MaxAttempts > uint8(len(r.Models)*len(r.Efforts)) {
		return errors.New("recovery contract attempts exceed closed tier ladder")
	}
	for i, model := range r.Models {
		if strings.TrimSpace(model) == "" || len(model) > 128 || (i > 0 && r.Models[i-1] == model) {
			return errors.New("recovery contract has invalid model tier")
		}
	}
	last := -1
	for _, effort := range r.Efforts {
		rank := map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}[effort]
		if rank == 0 && effort != "low" {
			return errors.New("recovery contract has invalid effort tier")
		}
		if _, ok := map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}[effort]; !ok || rank < last {
			return errors.New("recovery contract effort ladder is invalid")
		}
		last = rank
	}
	seen := map[string]bool{}
	criteria := map[string]bool{}
	for _, id := range r.ApprovedCriterionIDs {
		if !taskIDPattern.MatchString(id) || criteria[id] {
			return errors.New("invalid approved criterion id")
		}
		criteria[id] = true
	}
	for _, template := range r.CorrectiveTemplates {
		if !taskIDPattern.MatchString(template.ID) || seen[template.ID] || strings.TrimSpace(template.Summary) == "" || strings.TrimSpace(template.Prompt) == "" || strings.TrimSpace(template.VerificationSummary) == "" || strings.TrimSpace(template.VerificationPrompt) == "" || len(template.Prompt) > 64<<10 || len(template.VerificationPrompt) > 64<<10 || !template.RetrySafe {
			return errors.New("invalid corrective template")
		}
		for _, criterion := range template.CriterionIDs {
			if !criteria[criterion] {
				return errors.New("corrective template criterion is not approved")
			}
		}
		seen[template.ID] = true
	}
	return nil
}

func Load(path string) (*Plan, []byte, error) {
	p, canonical, err := LoadDraft(path)
	if err != nil {
		return nil, nil, err
	}
	if !p.Approved {
		return nil, nil, errors.New("voyage is not captain-approved")
	}
	return p, canonical, nil
}

// LoadDraft accepts an unapproved planning document while preserving every
// structural and safety validation used by execution.
func LoadDraft(path string) (*Plan, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxPlanBytes {
		return nil, nil, errors.New("voyage plan exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return nil, nil, fmt.Errorf("decode voyage plan: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return nil, nil, errors.New("decode voyage plan: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("decode voyage plan trailing content: %w", err)
	}
	canonical, err := json.Marshal(p)
	if err != nil {
		return nil, nil, err
	}
	if err := p.validate(false); err != nil {
		return nil, nil, err
	}
	return &p, canonical, nil
}

func (p *Plan) Validate() error {
	return p.validate(true)
}

func (p *Plan) validate(requireApproval bool) error {
	if p.Version != 1 {
		return errors.New("voyage plan version must be 1")
	}
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Objective) == "" {
		return errors.New("voyage title and objective are required")
	}
	if requireApproval && !p.Approved {
		return errors.New("voyage is not captain-approved")
	}
	for label, values := range map[string][]string{"scope": p.Scope, "non_goals": p.NonGoals, "blast_area": p.BlastArea, "risks": p.Risks, "acceptance_criteria": p.AcceptanceCriteria, "open_decisions": p.OpenDecisions} {
		if len(values) > 64 {
			return fmt.Errorf("voyage %s exceeds 64 items", label)
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 2048 {
				return fmt.Errorf("voyage %s contains an invalid item", label)
			}
		}
	}
	if len(p.Tasks) == 0 || len(p.Tasks) > 128 {
		return errors.New("voyage must contain 1 to 128 tasks")
	}
	if p.AcceptanceGateTask != "" && !taskIDPattern.MatchString(p.AcceptanceGateTask) {
		return errors.New("invalid acceptance gate task")
	}
	byID := make(map[string]Task, len(p.Tasks))
	for _, task := range p.Tasks {
		if !taskIDPattern.MatchString(task.ID) {
			return fmt.Errorf("invalid task id %q", task.ID)
		}
		if _, exists := byID[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		if strings.TrimSpace(task.Persona) == "" || strings.TrimSpace(task.Summary) == "" || strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task %q requires persona, summary, and prompt", task.ID)
		}
		if len(task.Prompt) > 64<<10 {
			return fmt.Errorf("task %q prompt exceeds size limit", task.ID)
		}
		if len(task.Models) > 4 || len(task.Efforts) > 4 || task.TierCount() > 8 {
			return fmt.Errorf("task %q escalation ladder exceeds eight tiers", task.ID)
		}
		if task.TierCount() > 1 && !task.RetrySafe {
			return fmt.Errorf("task %q needs retry_safe for progressive escalation", task.ID)
		}
		if err := task.Recovery.Validate(); err != nil {
			return fmt.Errorf("task %q recovery contract: %w", task.ID, err)
		}
		for _, model := range task.Models {
			if strings.TrimSpace(model) == "" || len(model) > 128 {
				return fmt.Errorf("task %q has invalid model tier", task.ID)
			}
		}
		lastEffort := -1
		for _, effort := range task.Efforts {
			rank := -1
			switch effort {
			case "low":
				rank = 0
			case "medium":
				rank = 1
			case "high":
				rank = 2
			case "xhigh":
				rank = 3
			case "max":
				rank = 4
			default:
				return fmt.Errorf("task %q has invalid effort tier %q", task.ID, effort)
			}
			if rank < lastEffort {
				return fmt.Errorf("task %q effort ladder must progress from low to high", task.ID)
			}
			lastEffort = rank
		}
		byID[task.ID] = task
	}
	if p.AcceptanceGateTask != "" {
		if _, ok := byID[p.AcceptanceGateTask]; !ok {
			return errors.New("acceptance gate task is not in the plan")
		}
	}
	for _, task := range p.Tasks {
		seen := map[string]bool{}
		for _, dependency := range task.DependsOn {
			if dependency == task.ID || seen[dependency] {
				return fmt.Errorf("task %q has invalid dependency %q", task.ID, dependency)
			}
			if _, exists := byID[dependency]; !exists {
				return fmt.Errorf("task %q depends on unknown task %q", task.ID, dependency)
			}
			seen[dependency] = true
		}
	}
	if _, err := p.Order(); err != nil {
		return err
	}
	return nil
}

func (t Task) TierCount() int {
	models, efforts := len(t.Models), len(t.Efforts)
	if models == 0 {
		models = 1
	}
	if efforts == 0 {
		efforts = 1
	}
	return models * efforts
}

func (p *Plan) Order() ([]string, error) {
	indegree := map[string]int{}
	children := map[string][]string{}
	for _, task := range p.Tasks {
		indegree[task.ID] = len(task.DependsOn)
		for _, dependency := range task.DependsOn {
			children[dependency] = append(children[dependency], task.ID)
		}
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(p.Tasks))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(p.Tasks) {
		return nil, errors.New("voyage task dependency graph contains a cycle")
	}
	return order, nil
}

func Hash(canonical []byte) string {
	sum := sha256.Sum256(bytes.TrimSpace(canonical))
	return hex.EncodeToString(sum[:])
}

// TaskFingerprint is the deterministic execution contract for one task. It
// intentionally includes ordered dependencies and every dispatch/escalation
// field; task IDs alone are never an inheritance key.
func TaskFingerprint(t Task) string {
	legacy := struct {
		ID, Persona, Summary, Prompt string
		DependsOn                    []string
		Models                       []string
		Efforts                      []string
		RetrySafe                    bool
	}{t.ID, t.Persona, t.Summary, t.Prompt, t.DependsOn, t.Models, t.Efforts, t.RetrySafe}
	if t.Recovery == nil {
		return hashCanonical(legacy)
	}
	return hashCanonical(struct {
		Legacy   any
		Recovery *RecoveryTask
	}{legacy, t.Recovery})
}

// GlobalFingerprint conservatively covers plan fields whose change can alter
// the meaning or authorization of every task in a voyage.
func GlobalFingerprint(p *Plan) string {
	return hashCanonical(struct {
		Version            int
		Title, Objective   string
		Scope, NonGoals    []string
		BlastArea, Risks   []string
		AcceptanceCriteria []string
		OpenDecisions      []string
		AcceptanceGateTask string
	}{p.Version, p.Title, p.Objective, p.Scope, p.NonGoals, p.BlastArea, p.Risks, p.AcceptanceCriteria, p.OpenDecisions, p.AcceptanceGateTask})
}

// TaskFingerprints returns local and dependency-closure fingerprints in a
// stable topological-independent form. A changed dependency changes every
// dependent closure fingerprint.
func TaskFingerprints(p *Plan) (map[string]string, map[string]string, error) {
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	local := make(map[string]string, len(p.Tasks))
	byID := make(map[string]Task, len(p.Tasks))
	for _, t := range p.Tasks {
		local[t.ID], byID[t.ID] = TaskFingerprint(t), t
	}
	closure := make(map[string]string, len(p.Tasks))
	var visit func(string, map[string]bool) (string, error)
	visit = func(id string, visiting map[string]bool) (string, error) {
		if got, ok := closure[id]; ok {
			return got, nil
		}
		if visiting[id] {
			return "", errors.New("voyage task dependency graph contains a cycle")
		}
		visiting[id] = true
		parts := []struct{ ID, Fingerprint string }{{id, local[id]}}
		for _, dep := range byID[id].DependsOn {
			fp, err := visit(dep, visiting)
			if err != nil {
				return "", err
			}
			parts = append(parts, struct{ ID, Fingerprint string }{dep, fp})
		}
		delete(visiting, id)
		got := hashCanonical(parts)
		closure[id] = got
		return got, nil
	}
	for _, t := range p.Tasks {
		if _, err := visit(t.ID, map[string]bool{}); err != nil {
			return nil, nil, err
		}
	}
	return local, closure, nil
}

func hashCanonical(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
