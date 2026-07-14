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
	Approved           bool     `json:"approved"`
	Tasks              []Task   `json:"tasks"`
}

type Task struct {
	ID        string   `json:"id"`
	Persona   string   `json:"persona"`
	Summary   string   `json:"summary"`
	Prompt    string   `json:"prompt"`
	DependsOn []string `json:"depends_on"`
	Models    []string `json:"models,omitempty"`
	Efforts   []string `json:"efforts,omitempty"`
	RetrySafe bool     `json:"retry_safe,omitempty"`
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
