package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
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
			&cli.BoolFlag{Name: "fresh", Usage: "start a fresh Skipper planning thread"},
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
	refresh := func() { model.Sidebar = voyageSidebar(defaultVoyagePlan) }
	hook := func(hookCtx context.Context, input dashboard.ParsedInput) (bool, bool, error) {
		switch input.Kind {
		case dashboard.ParsedConsult:
			advice, err := consultArchitect(hookCtx, input.Text)
			if err != nil {
				return true, false, err
			}
			message := "Architect advisory requested by the Captain:\n\n" + advice + "\n\nEvaluate this advice against the Captain's stated goals. Explain any tradeoff, then update the unapproved structured voyage draft only if appropriate."
			if err := sendPlanningMessage(hookCtx, model, conn, message); err != nil {
				return true, false, err
			}
			model.Notice("architect consultation delivered to Skipper")
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
		advice, err := consultArchitect(hookCtx, question)
		if err != nil {
			message := "The automatic Architect consultation could not be completed: " + boundedSailText(err.Error(), 512) + ". Continue planning with the Captain using the available evidence. Do not ask the Captain to run /consult."
			if sendErr := sendPlanningMessage(hookCtx, syncedModel, syncedConn, message); sendErr != nil {
				return fmt.Errorf("automatic architect consultation failed: %v; Skipper notification failed: %w", err, sendErr)
			}
			return nil
		}
		message := "Automatic Architect consultation requested by the Skipper.\n\nQuestion: " + question + "\n\nArchitect advice:\n" + advice + "\n\nEvaluate this advice against the Captain's goals, explain consequential tradeoffs, and update the unapproved voyage draft only when appropriate. Do not ask the Captain to run /consult."
		if err := sendPlanningMessage(hookCtx, syncedModel, syncedConn, message); err != nil {
			return err
		}
		syncedModel.Notice("architect consultation delivered automatically")
		return nil
	}
	editor := dashboard.NewNativeEditor(os.Stdin, os.Stdout)
	err = dashboard.ActionLoopWithHooksAndSync(ctx, editor, dashboard.NativeSize(os.Stdout), dashboard.NativeRenderer(os.Stdout, plain, editor), model, conn, refresh, hook, syncHook)
	return sailRequested, retryFailed, verboseSail, err
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
	status := "DRAFT - Captain approval required"
	if p.Approved {
		status = "APPROVED - /sail when ready"
	}
	var state *voyage.State
	statePath := filepath.Join(project.Dir, "voyages", voyage.Hash(canonical)[:16]+".json")
	if _, statErr := os.Stat(statePath); statErr == nil {
		state, _ = voyage.LoadState(statePath, p, voyage.Hash(canonical))
	}
	completed := state != nil && len(state.Tasks) == len(p.Tasks)
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
		status = "COMPLETED - acceptance criteria passed"
		objective = "PASS - " + objective
	} else if active {
		status = "SAILING - crew tasks active"
	} else if failed {
		status = "INCOMPLETE - review blockers with Skipper"
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
	if completed {
		for i := range acceptance {
			acceptance[i] = "PASS - " + acceptance[i]
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
	return &dashboard.Sidebar{Title: p.Title, Status: status, Sections: sections}
}
