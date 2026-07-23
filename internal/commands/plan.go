package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

const skipperConsultPrefix = "SHIPMATES_CONSULT_ARCHITECT:"

// Plan opens the Captain's durable planning room with the configured Skipper.
func Plan() *cli.Command {
	return &cli.Command{
		Name:  "plan",
		Usage: "plan a voyage with the Skipper, consult crew, and sail when approved",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a fresh Skipper thread and clear the active voyage draft"},
			&cli.BoolFlag{Name: "plain", Usage: "avoid alternate-screen presentation"},
		},
		Action: runPlan,
	}
}

func runPlan(ctx context.Context, c *cli.Command) error {
	originalDir, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := project.FindRoot(originalDir)
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(originalDir) }()

	cfg, err := project.LoadConfig()
	if err != nil {
		return err
	}
	skipper := strings.TrimSpace(cfg.SkipperPersona)
	if skipper == "" {
		skipper = "skipper"
	}
	if _, err := project.CanonicalPersonaAt(".", skipper); err != nil {
		return fmt.Errorf("configured skipper %q is not installed: %w", skipper, err)
	}
	if err := validatePlanningPolicy(root, skipper); err != nil {
		return err
	}
	if err := client.EnsureRunning(); err != nil {
		return err
	}
	fresh := c.Bool("fresh")
	var returnMessage string
	for {
		guard, err := dashboard.NewGuard(dashboard.NewNativeTerminal(os.Stdin, os.Stdout), c.Bool("plain"))
		if err != nil {
			return err
		}
		var sailRequested, retryFailed, verboseSail bool
		err = dashboard.Run(ctx, guard, func(runCtx context.Context) error {
			sailRequested, retryFailed, verboseSail, err = runPlanningRoom(runCtx, skipper, fresh, c.Bool("plain"), returnMessage)
			return err
		})
		fresh, returnMessage = false, ""
		if err != nil || !sailRequested {
			return err
		}
		sail := Sail()
		sail.Writer, sail.ErrWriter = os.Stdout, os.Stderr
		args := []string{"sail"}
		if retryFailed {
			args = append(args, "--retry-failed")
		}
		if verboseSail {
			args = append(args, "--verbose")
		}
		err = sail.Run(ctx, args)
		if err == nil {
			returnMessage = "Sail completed every approved voyage task successfully. Re-enter the Captain planning room, inspect the persisted voyage results and relevant workspace changes, give the Captain a concise evidence-based completion summary, call out any residual risks or follow-up opportunities without inventing new scope, and wait for the Captain's next instruction."
			continue
		}
		if ctx.Err() != nil {
			return err
		}
		returnMessage = "Sail returned control because the voyage is incomplete or blocked: " + boundedSailText(err.Error(), 1024) + ". Review persisted voyage state with the Captain, identify the smallest decision or amendment needed, and do not mark the voyage approved again without explicit Captain confirmation."
	}
}

func validatePlanningPolicy(root, skipper string) error {
	rootID, err := project.ScopeID(root)
	if err != nil {
		return fmt.Errorf("identify Shipmates project: %w", err)
	}
	snapshot, diagnostics := policy.Load(root, skipper, rootID)
	if snapshot != nil && len(diagnostics) == 0 {
		return nil
	}
	detail := "policy could not be loaded"
	if len(diagnostics) > 0 {
		detail = diagnostics[0].Code
		if diagnostics[0].Path != "" {
			detail += " at " + diagnostics[0].Path
		}
	}
	return fmt.Errorf("Skipper cannot start: invalid project policy (%s); run: shipmates policy validate %s", detail, skipper)
}

func runPlanningRoom(ctx context.Context, skipper string, fresh, plain bool, returnMessage string) (bool, bool, bool, error) {
	conn, err := dashboard.Connect(ctx, dashboard.HTTPTransport{}, skipper, dashboard.AttachRequest{Fresh: fresh})
	if err != nil {
		return false, false, false, err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(releaseCtx)
	}()
	if fresh {
		if err := clearActiveVoyage(defaultVoyagePlan); err != nil {
			return false, false, false, err
		}
	}
	model, err := dashboard.NewModel(conn.Attach)
	if err != nil {
		return false, false, false, err
	}
	model.Notice("Captain planning room: discuss the voyage; /consult <question>; /sail after explicit approval; /quit to detach")
	if returnMessage != "" {
		if err := sendPlanningMessage(ctx, model, conn, returnMessage); err != nil {
			model.Notice(returnMessage)
		}
	}
	sailRequested := false
	retryFailed := false
	verboseSail := false
	var consultation planningConsult
	refresh := func() { refreshPlanSidebar(model, defaultVoyagePlan) }
	hook := func(hookCtx context.Context, input dashboard.ParsedInput) (bool, bool, error) {
		if consultation.Active() && (input.Kind == dashboard.ParsedMessage || input.Kind == dashboard.ParsedConsult || input.Kind == dashboard.ParsedSail) {
			model.Notice("Architect consultation in progress; /quit and /interrupt remain available")
			return true, false, nil
		}
		switch input.Kind {
		case dashboard.ParsedConsult:
			consultation.Start(hookCtx, input.Text, true, consultArchitect)
			model.Notice("consulting Architect; Captain controller remains active")
			return true, false, nil
		case dashboard.ParsedSail:
			if _, _, err := voyage.Load(defaultVoyagePlan); err != nil {
				return true, false, fmt.Errorf("cannot sail: %w", err)
			}
			sailRequested = true
			retryFailed = strings.Contains(input.Text, "retry-failed")
			verboseSail = strings.Contains(input.Text, "verbose")
			return true, true, nil
		}
		return false, false, nil
	}
	var lastAutomaticConsult uint64
	var lastInvalidDraft [32]byte
	syncHook := func(hookCtx context.Context, syncedModel *dashboard.Model, syncedConn *dashboard.Connection) error {
		if syncedModel.State != livesession.Idle {
			return nil
		}
		if result, ok := consultation.Poll(); ok {
			if result.err != nil {
				if result.captain {
					syncedModel.Notice("Architect consultation failed: " + boundedSailText(result.err.Error(), 512))
					return nil
				}
				message := "The automatic Architect consultation could not be completed: " + boundedSailText(result.err.Error(), 512) + ". Continue planning with the Captain using the available evidence. Do not ask the Captain to run /consult."
				if err := sendPlanningMessage(hookCtx, syncedModel, syncedConn, message); err != nil {
					return fmt.Errorf("automatic architect consultation failed: %v; Skipper notification failed: %w", result.err, err)
				}
				return nil
			}
			advice := boundedSailText(result.advice, 12<<10)
			message := "Automatic Architect consultation requested by the Skipper.\n\nQuestion: " + result.question + "\n\nArchitect advice:\n" + advice + "\n\nEvaluate this advice against the Captain's goals, explain consequential tradeoffs, and update the unapproved voyage draft only when appropriate. Do not ask the Captain to run /consult."
			if result.captain {
				message = "Architect advisory requested by the Captain:\n\n" + advice + "\n\nEvaluate this advice against the Captain's stated goals. Explain any tradeoff, then update the unapproved structured voyage draft only if appropriate."
			}
			if err := sendPlanningMessage(hookCtx, syncedModel, syncedConn, message); err != nil {
				return err
			}
			syncedModel.Notice("architect consultation delivered to Skipper")
			return nil
		}
		if consultation.Active() {
			return nil
		}
		question, sequence, eventIndex, ok := skipperConsultation(syncedModel.Events, lastAutomaticConsult)
		if !ok {
			invalidHash, validationErr := invalidVoyageDraft(defaultVoyagePlan)
			if validationErr == nil {
				lastInvalidDraft = [32]byte{}
				return nil
			}
			if invalidHash == lastInvalidDraft {
				return nil
			}
			lastInvalidDraft = invalidHash
			message := "Host voyage validation rejected the current draft: " + boundedSailText(validationErr.Error(), 512) + ". Correct only this validation defect, keep approved false, preserve the Captain's accepted scope and decisions, and then present the corrected plan."
			if err := sendPlanningMessage(hookCtx, syncedModel, syncedConn, message); err != nil {
				return err
			}
			syncedModel.Notice("invalid voyage draft returned to Skipper for correction")
			return nil
		}
		lastAutomaticConsult = sequence
		syncedModel.Events[eventIndex].Text = "agent: Consulting Architect automatically: " + question
		consultation.Start(hookCtx, question, false, consultArchitect)
		syncedModel.Notice("consulting Architect automatically; Captain controller remains active")
		return nil
	}
	editor := dashboard.NewNativeEditor(os.Stdin, os.Stdout)
	err = dashboard.ActionLoopWithHooksAndSync(ctx, editor, dashboard.NativeSize(os.Stdout), dashboard.NativeRenderer(os.Stdout, plain, editor), model, conn, refresh, hook, syncHook)
	return sailRequested, retryFailed, verboseSail, err
}

// refreshPlanSidebar is the host-side reload boundary used before every Plan
// redraw. It deliberately re-reads both plan and persisted state through
// voyageSidebar instead of retaining a prior acceptance projection.
func refreshPlanSidebar(model *dashboard.Model, path string) {
	model.Sidebar = voyageSidebar(path)
}

type planningConsultResult struct {
	question string
	advice   string
	err      error
	captain  bool
}

type planningConsult struct {
	results <-chan planningConsultResult
}

func (p *planningConsult) Active() bool { return p.results != nil }

func (p *planningConsult) Start(ctx context.Context, question string, captain bool, consult func(context.Context, string) (string, error)) bool {
	if p.Active() {
		return false
	}
	results := make(chan planningConsultResult, 1)
	p.results = results
	go func() {
		advice, err := consult(ctx, question)
		results <- planningConsultResult{question: question, advice: advice, err: err, captain: captain}
	}()
	return true
}

func (p *planningConsult) Poll() (planningConsultResult, bool) {
	if !p.Active() {
		return planningConsultResult{}, false
	}
	select {
	case result := <-p.results:
		p.results = nil
		return result, true
	default:
		return planningConsultResult{}, false
	}
}

func invalidVoyageDraft(path string) ([32]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return [32]byte{}, nil
	}
	_, _, err = voyage.LoadDraft(path)
	if err == nil {
		return [32]byte{}, nil
	}
	return sha256.Sum256(raw), err
}

func skipperConsultation(events []dashboard.DisplayEvent, after uint64) (string, uint64, int, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Kind != "agent" || event.Partial || event.Sequence <= after {
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(event.Text, "agent:"))
		if !strings.HasPrefix(text, skipperConsultPrefix) {
			return "", 0, 0, false
		}
		question := strings.TrimSpace(strings.TrimPrefix(text, skipperConsultPrefix))
		if question == "" {
			return "", 0, 0, false
		}
		return boundedSailText(question, 2045), event.Sequence, i, true
	}
	return "", 0, 0, false
}

func clearActiveVoyage(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect active voyage draft: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("active voyage draft is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("clear active voyage draft: %w", err)
	}
	return nil
}

func sendPlanningMessage(ctx context.Context, model *dashboard.Model, conn *dashboard.Connection, message string) error {
	if model.State != livesession.Idle || model.PendingApproval != nil {
		return errors.New("Skipper is busy; wait for the current turn before sending this message")
	}
	snap := livesession.Snapshot{Persona: model.Persona, SessionID: conn.Attach.SessionID, Backend: conn.Attach.Backend, ThreadID: conn.Attach.ThreadID, TurnID: conn.Attach.TurnID, State: model.State}
	res, err := conn.Action(ctx, livesession.ControllerMessage, snap, message)
	if err != nil {
		return err
	}
	conn.Attach.Snapshot = res.Snapshot
	return model.ApplySnapshot(res.Snapshot)
}

func consultArchitect(ctx context.Context, question string) (string, error) {
	installed, err := project.CanonicalPersonaAt(".", "architect")
	if err != nil {
		return "", errors.New("architect is not installed; run: shipmates add architect")
	}
	draft, _ := os.ReadFile(defaultVoyagePlan)
	if len(draft) > voyage.MaxPlanBytes {
		return "", errors.New("voyage draft exceeds size limit")
	}
	prompt := "Provide bounded architectural advice to the Skipper. Do not modify files, invoke Shipmates, or use tools. Address only the question and supplied voyage draft. Identify assumptions, blast radius, alternatives, risks, and a recommendation.\n\nCaptain question: " + question + "\n\nCurrent voyage draft:\n" + string(draft)
	cfg, err := project.ResolvePersonaConfig("architect")
	if err != nil {
		return "", err
	}
	ladder, err := project.ModelLadderAt(".")
	if err != nil {
		return "", err
	}
	if len(ladder) > 1 {
		cfg.Model = ladder[1]
	} else if len(ladder) == 1 {
		cfg.Model = ladder[0]
	}
	if cfg.Effort == "" {
		cfg.Effort = "medium"
	}
	var stdout, stderr bytes.Buffer
	if err := dispatchManagedCodex(ctx, installed, prompt, false, cfg, nil, &stdout, &stderr); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func voyageSidebar(path string) *dashboard.Sidebar {
	p, canonical, err := voyage.LoadDraft(path)
	if err != nil {
		return &dashboard.Sidebar{Title: "VOYAGE PLAN", Status: "Draft unavailable: " + boundedSailText(err.Error(), 96), Sections: []dashboard.SidebarSection{{Heading: "Next", Items: []string{"Describe the outcome to the Skipper"}}}}
	}
	hash := voyage.Hash(canonical)
	status := "DRAFT - Captain approval required"
	if p.Approved {
		status = "APPROVED - /sail when ready"
	}
	var state *voyage.State
	var stateLoadErr error
	statePath := filepath.Join(project.Dir, "voyages", voyage.Hash(canonical)[:16]+".json")
	if _, statErr := os.Stat(statePath); statErr == nil {
		state, stateLoadErr = voyage.LoadState(statePath, p, voyage.Hash(canonical))
	}
	completed := state != nil && len(state.Tasks) == len(p.Tasks)
	acceptanceStatus := voyage.AcceptanceUnset
	if state != nil && state.Acceptance != nil {
		acceptanceStatus = state.Acceptance.Status
	}
	failed, active := false, false
	if state != nil {
		for _, entry := range state.Tasks {
			if entry.Status != voyage.Completed {
				completed = false
			}
			if entry.Status == voyage.Failed || entry.Status == voyage.Blocked || entry.Status == voyage.NeedsInput {
				failed = true
			}
			if entry.Status == voyage.Running {
				active = true
			}
		}
	}
	objective := p.Objective
	if completed {
		switch acceptanceStatus {
		case voyage.AcceptancePass:
			status = "COMPLETED - acceptance criteria passed"
			objective = "PASS - " + objective
		case voyage.AcceptanceNoGo:
			status = "COMPLETED - acceptance failed"
			objective = "NO-GO - " + objective
		default:
			status = "COMPLETED - acceptance unknown"
		}
	} else if active {
		status = "SAILING - crew tasks active"
	} else if failed {
		status = "INCOMPLETE - review blockers with Skipper"
	} else if p.Approved && (state == nil || stateLoadErr != nil) {
		// A missing, legacy, malformed, stale, or mismatched persisted state
		// cannot prove acceptance. Keep the plan actionable, but make unknown
		// explicit rather than allowing a cached or inferred success notice.
		status = "APPROVED - acceptance unknown"
	}
	sections := []dashboard.SidebarSection{{Heading: "Objective", Items: []string{objective}}}
	appendSection := func(name string, values []string) {
		if len(values) > 0 {
			sections = append(sections, dashboard.SidebarSection{Heading: name, Items: values})
		}
	}
	appendSection("Scope", p.Scope)
	appendSection("Non-goals", p.NonGoals)
	appendSection("Blast area", p.BlastArea)
	appendSection("Risks", p.Risks)
	acceptance := append([]string(nil), p.AcceptanceCriteria...)
	if completed && acceptanceStatus == voyage.AcceptancePass {
		for i := range acceptance {
			acceptance[i] = "PASS - " + acceptance[i]
		}
	} else if completed && acceptanceStatus == voyage.AcceptanceNoGo {
		for i := range acceptance {
			acceptance[i] = "NO-GO - " + acceptance[i]
		}
	}
	appendSection("Acceptance", acceptance)
	appendSection("Open decisions", p.OpenDecisions)
	jobs := make([]string, 0, len(p.Tasks))
	for _, task := range p.Tasks {
		taskStatus := "pending"
		if state != nil {
			taskStatus = string(state.Tasks[task.ID].Status)
		}
		bead := ""
		if state != nil {
			if id := state.Tasks[task.ID].BeadID; id != "" {
				bead = " bead=" + id
			}
		}
		jobs = append(jobs, fmt.Sprintf("[%s] %s [%s]%s %s", taskStatus, task.ID, task.Persona, bead, task.Summary))
	}
	appendSection("Jobs", jobs)
	if recovery := recoverySidebar(hash, p, state); len(recovery) > 0 {
		sections = append(sections, dashboard.SidebarSection{Heading: "Auto-captain recovery (advisory)", Items: recovery})
	}
	if structured := structuredRecoverySidebar(hash, p); len(structured) > 0 {
		sections = append(sections, dashboard.SidebarSection{Heading: "Structured Sail recovery", Items: structured})
	}
	return &dashboard.Sidebar{Title: p.Title, Status: status, Sections: sections}
}

type structuredAttemptDisplay struct {
	ID, Model, Effort, TaskFingerprint, ResultHash             string
	Outcome                                                    recovery.CrewOutcome
	Reason                                                     recovery.CrewReason
	Tokens                                                     recovery.TokenUsage
	EvidenceCount                                              uint8
	Verifier                                                   recovery.VerifierStatus
	NextModel, NextEffort, To, Action, Cause                   string
	Infrastructure                                             uint8
	Corrective, CorrectiveBead, Verification, VerificationBead string
}

// structuredRecoverySidebar is a bounded projection of the append-only
// semantic ledger. It never renders prompts, summaries, paths, or raw records.
func structuredRecoverySidebar(planHash string, p *voyage.Plan) []string {
	if p == nil {
		return nil
	}
	items := make([]string, 0, 12)
	for _, task := range p.Tasks {
		if task.Recovery == nil || !task.Recovery.Enabled {
			continue
		}
		ledger, err := recovery.OpenAttemptLedger(structuredLedgerPath(planHash, task))
		if err != nil || len(ledger.Records) == 0 {
			continue
		}
		attempts := make([]*structuredAttemptDisplay, 0, 8)
		byID := make(map[string]*structuredAttemptDisplay)
		for _, record := range ledger.Records {
			if len(attempts) >= 8 && record.Kind == "reservation" {
				continue
			}
			entry := byID[record.AttemptID]
			switch record.Kind {
			case "reservation":
				var value recovery.AttemptReservation
				if json.Unmarshal(record.Payload, &value) != nil {
					continue
				}
				entry = &structuredAttemptDisplay{ID: value.AttemptID, Model: value.Model, Effort: value.Effort, TaskFingerprint: value.TaskFingerprint, Tokens: value.Tokens}
				byID[record.AttemptID] = entry
				attempts = append(attempts, entry)
			case "result":
				var value recovery.ResultRecord
				if json.Unmarshal(record.Payload, &value) != nil {
					continue
				}
				if entry == nil {
					entry = &structuredAttemptDisplay{ID: record.AttemptID}
					byID[record.AttemptID] = entry
					attempts = append(attempts, entry)
				}
				entry.ResultHash, entry.Model, entry.Effort = value.ResultHash, value.Model, value.Effort
				entry.Outcome, entry.Reason, entry.TaskFingerprint = value.Outcome, value.Reason, value.TaskFingerprint
				entry.Tokens, entry.EvidenceCount, entry.Verifier = value.Tokens, value.EvidenceCount, value.VerifierStatus
				entry.Corrective = value.CorrectiveTemplateID
			case "transition":
				var value recovery.TransitionRecord
				if json.Unmarshal(record.Payload, &value) != nil {
					continue
				}
				if entry == nil {
					continue
				}
				entry.To, entry.Action = value.Transition.To, string(value.Transition.Action)
				entry.NextModel, entry.NextEffort = value.Transition.NextModel, value.Transition.NextEffort
				entry.Cause = string(entry.Reason)
			case "infrastructure_retry":
				if entry == nil {
					continue
				}
				entry.Infrastructure++
			case "corrective_successor":
				var value recovery.CorrectiveSuccessor
				if json.Unmarshal(record.Payload, &value) != nil || entry == nil {
					continue
				}
				entry.Corrective, entry.CorrectiveBead = value.Task.ID, value.TaskBeadID
				entry.Verification, entry.VerificationBead = value.VerificationTask.ID, value.VerificationBeadID
			}
		}
		for _, entry := range attempts {
			if entry.Model == "" {
				entry.Model = "unknown"
			}
			if entry.Effort == "" {
				entry.Effort = "unknown"
			}
			remaining := uint32(0)
			if entry.Tokens.Reserved >= entry.Tokens.Used {
				remaining = entry.Tokens.Reserved - entry.Tokens.Used
			}
			next := "none"
			if entry.NextModel != "" {
				next = safeSidebar(entry.NextModel, 32) + "/" + safeSidebar(entry.NextEffort, 16)
			}
			line := fmt.Sprintf("%s: slot=crew-result | current=%s/%s | next=%s | budget=%d reserved,%d used,%d remaining | outcome=%s | reason=%s", task.ID, safeSidebar(entry.Model, 64), safeSidebar(entry.Effort, 16), next, entry.Tokens.Reserved, entry.Tokens.Used, remaining, safeSidebar(stringOrUnknown(entry.Outcome), 32), safeSidebar(stringOrUnknown(entry.Reason), 40))
			if entry.EvidenceCount > 0 || entry.ResultHash != "" {
				line += " | evidence=" + shortHash(entry.ResultHash)
			}
			if entry.TaskFingerprint != "" {
				line += " | fingerprint=" + shortHash(entry.TaskFingerprint)
			}
			if entry.Infrastructure > 0 {
				line += fmt.Sprintf(" | infrastructure retries=%d", entry.Infrastructure)
			}
			if entry.NextModel != "" {
				line += " | transition=" + safeSidebar(entry.Model, 32) + "/" + safeSidebar(entry.Effort, 16) + " -> " + safeSidebar(entry.NextModel, 32) + "/" + safeSidebar(entry.NextEffort, 16) + " cause=" + safeSidebar(entry.Cause, 32)
			}
			if entry.To == "needs_input" {
				line += " | pause=Captain input required | Sol=advisory only"
			}
			if entry.Verifier == recovery.VerifierNoGo {
				line += " | verifier=NO-GO"
			}
			if entry.Corrective != "" {
				line += " | successor=" + safeSidebar(entry.Corrective, 32) + " bead=" + safeSidebar(entry.CorrectiveBead, 24) + " follow-up=" + safeSidebar(entry.Verification, 32) + " bead=" + safeSidebar(entry.VerificationBead, 24)
			}
			if entry.To != "" {
				line += " | state=" + safeSidebar(entry.To, 32)
			}
			items = append(items, line)
		}
	}
	return items
}

func stringOrUnknown[T ~string](value T) string {
	if value == "" {
		return "unknown"
	}
	return string(value)
}

type recoveryDisplayRecord struct {
	Kind        string `json:"kind"`
	Fingerprint string `json:"blocker_fingerprint"`
	Reason      string `json:"reason"`
	Decision    string `json:"decision"`
	Action      string `json:"action"`
	Evidence    []struct {
		Source string `json:"source"`
		Digest string `json:"digest"`
	} `json:"evidence"`
}

// recoverySidebar is deliberately a presentation-only projection. It reads
// only bounded, already persisted recovery records and never exposes a
// proposal as an executable control.
func recoverySidebar(planHash string, p *voyage.Plan, state *voyage.State) []string {
	items := []string{"feature: " + boolWord(recoveryEnabled()) + " | Sol model: gpt-5.6-sol | authority: Sail, not Sol"}
	items = append(items, "machine-approved derivatives: bounded envelope only | Captain approval required for material amendments")
	if state != nil {
		items = append(items, "successor: "+shortHash(planHash))
		if state.Lineage != nil {
			items = append(items, "predecessor: "+shortHash(state.Lineage.PredecessorPlanHash))
		}
	}
	records := readRecoveryDisplayRecords(planHash)
	assessments := 0
	for _, record := range records {
		if record.Kind != "assessment" && record.Kind != "blocker" {
			continue
		}
		if record.Kind == "assessment" {
			assessments++
		}
		fp := shortHash(record.Fingerprint)
		if record.Kind == "blocker" {
			items = append(items, "blocker: "+safeSidebar(record.Reason, 64)+" | fingerprint: "+fp)
			for _, evidence := range record.Evidence {
				items = append(items, "evidence: "+safeSidebar(evidence.Source, 32)+"#"+shortHash(evidence.Digest))
			}
		} else {
			decision := safeSidebar(record.Decision, 32)
			if decision == "resume" || decision == "continue" {
				items = append(items, "recommendation: "+decision+" | accepted action: bounded retry/resume")
			} else if decision == "amendment_required" {
				items = append(items, "recommendation: amendment required | accepted action: none | material change requires Captain approval")
			} else {
				items = append(items, "recommendation: "+decision+" | accepted action: none")
			}
		}
	}
	if assessments > 0 {
		items = append(items, fmt.Sprintf("attempts assessed: %d", assessments))
	}
	_ = p // retain the plan in this presentation seam for future bounded fields
	return items
}

func recoveryEnabled() bool {
	cfg, err := project.LoadConfig()
	return err == nil && cfg.Recovery.AutoCaptain
}

func readRecoveryDisplayRecords(planHash string) []recoveryDisplayRecord {
	b, err := os.ReadFile(filepath.Join(project.Dir, "recovery", planHash[:16]+".jsonl"))
	if err != nil || len(b) > 4<<20 {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 129 {
		lines = lines[len(lines)-129:]
	}
	out := make([]recoveryDisplayRecord, 0, 8)
	for _, line := range lines {
		var record recoveryDisplayRecord
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record.Kind == "blocker" || record.Kind == "assessment" {
			out = append(out, record)
		}
		if len(out) >= 8 {
			out = out[len(out)-8:]
		}
	}
	return out
}

func shortHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 12 {
		return value[:12]
	}
	if value == "" {
		return "—"
	}
	return value
}

func safeSidebar(value string, limit int) string { return boundedSailText(value, limit) }
func boolWord(value bool) string {
	if value {
		return "enabled"
	}
	return "disabled"
}
