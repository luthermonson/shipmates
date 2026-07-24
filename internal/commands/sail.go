package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/turninput"
	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

const defaultVoyagePlan = ".shipmates/voyage.json"

var sailTaskDispatcher = dispatchManagedSailTask
var codexTurnDispatcher = dispatchManagedCodex

// Sail executes a captain-approved voyage from its dependency graph to a
// persisted terminal state. Planning remains an interactive skipper duty.
func Sail() *cli.Command {
	return &cli.Command{
		Name:  "sail",
		Usage: "execute a captain-approved voyage until every job is done or blocked",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "plan", Value: defaultVoyagePlan, Usage: "approved voyage plan path"},
			&cli.StringFlag{Name: "predecessor-plan", Usage: "explicit approved predecessor plan for a conservative amendment"},
			&cli.StringFlag{Name: "predecessor-state", Usage: "immutable predecessor state path; required with --predecessor-plan"},
			&cli.IntFlag{Name: "max-concurrent", Value: 3, Usage: "maximum crew turns in flight"},
			&cli.DurationFlag{Name: "task-timeout", Value: 30 * time.Minute, Usage: "maximum duration of each crew task"},
			&cli.BoolFlag{Name: "dry-run", Usage: "validate and display order without dispatching"},
			&cli.BoolFlag{Name: "retry-failed", Usage: "retry failed and dependency-blocked tasks"},
			&cli.BoolFlag{Name: "verbose", Usage: "show task prompts, agent reports, and exact tool details exposed by Codex"},
			&cli.BoolFlag{Name: "no-color", Usage: "disable persona colors"},
		},
		Action: runSail,
	}
}

func runSail(ctx context.Context, c *cli.Command) error {
	if runtime.GOOS == "windows" {
		return errors.New("shipmates sail is unix-only in v0.4: the voyage executor depends on PID-file dispatch locks + signal semantics not yet ported to Windows")
	}
	if os.Getenv(codexapp.ManagedSessionEnvironment) == "1" {
		return errors.New("sail cannot run inside a managed Shipmates persona session; the Captain must use /sail from the planning TUI or run shipmates sail from a normal terminal")
	}
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	planPath, err := safeVoyagePlanPath(c.String("plan"))
	if err != nil {
		return err
	}
	plan, canonical, err := voyage.Load(planPath)
	if err != nil {
		return err
	}
	if c.Int("max-concurrent") < 1 || c.Int("max-concurrent") > 16 {
		return errors.New("max-concurrent must be between 1 and 16")
	}
	if c.Duration("task-timeout") <= 0 {
		return errors.New("task-timeout must be positive")
	}
	predecessorPlan := c.String("predecessor-plan")
	predecessorState := c.String("predecessor-state")
	if (predecessorPlan == "") != (predecessorState == "") {
		return errors.New("predecessor-plan and predecessor-state must be supplied together")
	}
	if err := validateSailModelLadders(plan); err != nil {
		return err
	}

	installed := make(map[string]*project.InstalledPersona)
	for _, task := range plan.Tasks {
		if _, ok := installed[task.Persona]; ok {
			continue
		}
		persona, err := project.CanonicalPersonaAt(".", task.Persona)
		if err != nil {
			return fmt.Errorf("task %q persona %q: %w", task.ID, task.Persona, err)
		}
		installed[task.Persona] = persona
	}

	hash := voyage.Hash(canonical)
	activeDerivativePath := filepath.Join(project.Dir, "voyages", hash[:16]+".active.json")
	if info, activeErr := os.Lstat(activeDerivativePath); activeErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("active derivative plan is unsafe")
		}
		activePlan, _, loadErr := voyage.LoadDraft(activeDerivativePath)
		if loadErr != nil || !activePlan.Approved {
			return errors.New("active derivative plan is invalid")
		}
		plan = activePlan
		if err := validateSailModelLadders(plan); err != nil {
			return err
		}
	} else if !errors.Is(activeErr, os.ErrNotExist) {
		return activeErr
	}
	projectConfig, err := project.LoadConfig()
	if err != nil {
		return err
	}
	var recoveryRuntime *sailRecovery
	if !c.Bool("dry-run") {
		recoveryRuntime, err = newSailRecovery(hash, *projectConfig)
		if err != nil {
			return err
		}
	}
	statePath := filepath.Join(project.Dir, "voyages", hash[:16]+".json")
	var releaseVoyage func()
	if !c.Bool("dry-run") {
		releaseVoyage, err = project.AcquireDispatchLock("voyage-" + hash[:16])
		if err != nil {
			return fmt.Errorf("voyage is already sailing: %w", err)
		}
		defer releaseVoyage()
	}
	var state *voyage.State
	if predecessorPlan != "" {
		predecessorPlan, pathErr := safeVoyagePlanPath(predecessorPlan)
		if pathErr != nil {
			return pathErr
		}
		predecessorState, pathErr = safeVoyageStatePath(predecessorState)
		if pathErr != nil {
			return pathErr
		}
		if c.Bool("dry-run") {
			// Dry runs validate and render the exact migration result without
			// publishing successor state.
			state, err = voyage.MigrateSuccessorPreview(planPath, predecessorPlan, predecessorState, time.Now().UTC())
		} else {
			state, err = voyage.MigrateSuccessor(planPath, predecessorPlan, predecessorState, statePath, time.Now().UTC())
		}
	} else {
		state, err = voyage.LoadState(statePath, plan, hash)
	}
	if err != nil {
		return err
	}
	if err := recoveryRuntime.restoreDerivatives(plan, state); err != nil {
		return err
	}
	if c.Bool("retry-failed") {
		for id, entry := range state.Tasks {
			if entry.Status == voyage.Failed || entry.Status == voyage.Blocked || entry.Status == voyage.NeedsInput {
				if persistedInfrastructureFailure(entry.Error) {
					entry.Attempt = 0
				}
				entry.Status, entry.Error, entry.Summary = voyage.Pending, "", ""
				entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
				state.Tasks[id] = entry
			}
		}
	}

	display := newSailDisplay(c.Writer, !c.Bool("no-color"))
	display.verbose = c.Bool("verbose")
	var beadGraph *sailBeads
	if !c.Bool("dry-run") {
		beadGraph, err = prepareSailBeads(ctx, plan, state, hash, statePath)
		if err != nil {
			return err
		}
	}
	cancelWatchDone := make(chan struct{})
	defer close(cancelWatchDone)
	go func() {
		select {
		case <-ctx.Done():
			display.Canceling()
		case <-cancelWatchDone:
		}
	}()
	display.Header(plan, hash[:12], c.Bool("dry-run"))
	display.RecoveryConfig(recoveryRuntime != nil)
	display.Lineage(state, hash)
	order, _ := plan.Order()
	byID := make(map[string]voyage.Task, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byID[task.ID] = task
	}
	activateDerivative := func(result *recovery.DerivativeResult) error {
		if result == nil || result.Plan == nil {
			return nil
		}
		if err := result.Plan.Validate(); err != nil {
			return err
		}
		if err := publishActiveDerivative(activeDerivativePath, result.CanonicalPlan); err != nil {
			return err
		}
		local, closure, err := voyage.TaskFingerprints(result.Plan)
		if err != nil {
			return err
		}
		plan.Tasks = append([]voyage.Task(nil), result.Plan.Tasks...)
		byID = make(map[string]voyage.Task, len(plan.Tasks))
		for _, task := range plan.Tasks {
			byID[task.ID] = task
			entry, exists := state.Tasks[task.ID]
			if exists && entry.Inherited != nil && (entry.TaskFingerprint != local[task.ID] || entry.ClosureFingerprint != closure[task.ID]) {
				return errors.New("derivative would invalidate inherited completion")
			}
			if exists && entry.Status == voyage.Completed && (entry.TaskFingerprint != local[task.ID] || entry.ClosureFingerprint != closure[task.ID]) {
				return errors.New("derivative would invalidate completed task")
			}
			if !exists {
				entry = voyage.TaskState{Status: voyage.Pending}
				if err := beadGraph.ensureDerivativeTask(ctx, task, result.PlanHash, state, statePath); err != nil {
					return err
				}
				entry = state.Tasks[task.ID]
			}
			entry.TaskFingerprint, entry.ClosureFingerprint = local[task.ID], closure[task.ID]
			state.Tasks[task.ID] = entry
		}
		order, err = plan.Order()
		return err
	}
	for _, id := range order {
		display.PlanTask(byID[id], state.Tasks[id])
	}
	if c.Bool("dry-run") {
		return nil
	}
	if err := voyage.SaveState(statePath, state); err != nil {
		return err
	}

	var stateMu sync.Mutex
	save := func() error {
		stateMu.Lock()
		defer stateMu.Unlock()
		return voyage.SaveState(statePath, state)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("voyage canceled; resume with shipmates sail: %w", err)
		}
		ready := make([]voyage.Task, 0)
		readyPersonas := make(map[string]bool)
		changed := false
		for _, id := range order {
			task := byID[id]
			entry := state.Tasks[id]
			if entry.Status != voyage.Pending {
				continue
			}
			blocked := ""
			allDone := true
			for _, dependency := range task.DependsOn {
				dep := state.Tasks[dependency]
				if dep.Status == voyage.Failed || dep.Status == voyage.Blocked || dep.Status == voyage.NeedsInput {
					blocked = dependency
					break
				}
				if dep.Status != voyage.Completed {
					allDone = false
				}
			}
			if blocked != "" {
				entry.Status = voyage.Blocked
				entry.Error = "dependency " + blocked + " did not complete"
				entry.FinishedAt = time.Now().UTC()
				state.Tasks[id] = entry
				if err := beadGraph.finish(ctx, entry); err != nil {
					display.BeadWarning(task, err)
				}
				display.Blocked(task, entry.Error)
				changed = true
			} else if allDone && !readyPersonas[task.Persona] {
				ready = append(ready, task)
				readyPersonas[task.Persona] = true
			}
		}
		if changed {
			if err := save(); err != nil {
				return err
			}
		}
		if len(ready) == 0 {
			break
		}
		if len(ready) > c.Int("max-concurrent") {
			ready = ready[:c.Int("max-concurrent")]
		}

		dispatchConfigs := make(map[string]project.PersonaConfig, len(ready))
		structuredLedgers := make(map[string]*recovery.AttemptLedger, len(ready))
		structuredAttempts := make(map[string]string, len(ready))
		for _, task := range ready {
			cfg, cfgErr := sailTaskConfig(task, state.Tasks[task.ID].Attempt)
			if cfgErr != nil {
				return cfgErr
			}
			dispatchConfigs[task.ID] = cfg
			entry := state.Tasks[task.ID]
			if structuredEnabled(task) {
				ledger, attemptID, ledgerErr := prepareStructuredAttempt(hash, plan, task, entry)
				if ledgerErr != nil {
					return fmt.Errorf("task %q structured attempt: %w", task.ID, ledgerErr)
				}
				structuredLedgers[task.ID], structuredAttempts[task.ID] = ledger, attemptID
			}
			entry.Status, entry.StartedAt = voyage.Running, time.Now().UTC()
			entry.Error = ""
			state.Tasks[task.ID] = entry
			display.Started(task, cfg)
			if c.Bool("verbose") {
				brief := sailTaskPrompt(plan, task)
				if !structuredEnabled(task) {
					brief = beadGraph.prompt(brief, task, entry)
				}
				display.Brief(task, cfg, brief)
			}
		}
		if err := save(); err != nil {
			return err
		}

		type result struct {
			task      voyage.Task
			output    string
			err       error
			ledger    *recovery.AttemptLedger
			attemptID string
		}
		type dispatch struct {
			task    voyage.Task
			attempt int
			cfg     project.PersonaConfig
			prompt  string
		}
		results := make(chan result, len(ready))
		batchCtx, cancelBatch := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, task := range ready {
			entry := state.Tasks[task.ID]
			if err := beadGraph.start(ctx, task, entry); err != nil {
				display.BeadWarning(task, err)
			}
			prompt := sailTaskPrompt(plan, task)
			if !structuredEnabled(task) {
				prompt = beadGraph.prompt(prompt, task, entry)
			}
			work := dispatch{task: task, attempt: entry.Attempt, cfg: dispatchConfigs[task.ID], prompt: prompt}
			wg.Add(1)
			go func(work dispatch) {
				defer wg.Done()
				task := work.task
				turnCtx, cancel := context.WithTimeout(batchCtx, c.Duration("task-timeout"))
				defer cancel()
				if c.Bool("verbose") {
					turnCtx = context.WithValue(turnCtx, sailTraceContextKey{}, sailTrace{display: display, task: task, cfg: work.cfg})
				}
				stdout := newBoundedOutput(256 << 10)
				stderr := sailActivityWriter{display: display, task: task, cfg: work.cfg}
				err := sailTaskDispatcher(turnCtx, installed[task.Persona], work.prompt, work.cfg, stdout, stderr)
				results <- result{task: task, output: strings.TrimSpace(stdout.String()), err: err, ledger: structuredLedgers[task.ID], attemptID: structuredAttempts[task.ID]}
			}(work)
		}
		go func() {
			wg.Wait()
			close(results)
		}()
		var persistErr error
		for result := range results {
			if persistErr != nil {
				continue
			}
			entry := state.Tasks[result.task.ID]
			entry.FinishedAt = time.Now().UTC()
			if structuredEnabled(result.task) {
				if result.err != nil {
					if isManagedSessionFailure(result.err) {
						infra := structuredInfrastructureRetries(result.ledger, result.attemptID)
						if infra < result.task.Recovery.MaxInfrastructureRetries {
							if _, appendErr := result.ledger.Append("infrastructure_retry", result.attemptID, recovery.DispatchRecord{AttemptID: result.attemptID, Model: dispatchConfigs[result.task.ID].Model, Effort: dispatchConfigs[result.task.ID].Effort}, time.Now().UTC()); appendErr != nil {
								persistErr = appendErr
								cancelBatch()
								continue
							}
							entry.Status, entry.Error = voyage.Pending, "infrastructure retry scheduled"
							entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
							display.StructuredInfrastructureRetry(result.task, dispatchConfigs[result.task.ID], infra+1)
							display.Escalated(result.task, entry.Attempt, "bounded infrastructure retry")
						} else {
							entry.Status, entry.Error = voyage.Failed, "infrastructure retry budget exhausted"
							display.Failed(result.task, entry.Error)
							if adviceErr := adviseStructuredBlocker(ctx, recoveryRuntime, plan, hash, result.task, entry, recovery.ReasonExhaustedTiers, entry.Error, display); adviceErr != nil {
								persistErr = adviceErr
								cancelBatch()
								continue
							}
						}
					} else {
						entry.Status, entry.Error = voyage.Failed, structuredErrorText(structuredContractFailure(result.ledger, result.attemptID, result.err))
						display.Failed(result.task, entry.Error)
						if adviceErr := adviseStructuredBlocker(ctx, recoveryRuntime, plan, hash, result.task, entry, recovery.ReasonUncertainty, entry.Error, display); adviceErr != nil {
							persistErr = adviceErr
							cancelBatch()
							continue
						}
					}
				} else {
					parsed, canonicalResult, parseErr := recovery.ParseCrewResult([]byte(result.output), structuredBinding(hash, plan, result.task, entry, result.attemptID))
					if parseErr != nil {
						entry.Status, entry.Error = voyage.Failed, structuredErrorText(structuredContractFailure(result.ledger, result.attemptID, parseErr))
						display.Failed(result.task, entry.Error)
						if adviceErr := adviseStructuredBlocker(ctx, recoveryRuntime, plan, hash, result.task, entry, recovery.ReasonUncertainty, entry.Error, display); adviceErr != nil {
							persistErr = adviceErr
							cancelBatch()
							continue
						}
					} else {
						transition, transitionErr := recovery.AdvanceRecovery(result.task, uint8(entry.Attempt), structuredInfrastructureRetries(result.ledger, result.attemptID), parsed)
						if transitionErr != nil {
							persistErr = transitionErr
							cancelBatch()
							continue
						}
						persistErr = persistStructuredResult(result.ledger, result.attemptID, parsed, canonicalResult, transition)
						if persistErr != nil {
							cancelBatch()
							continue
						}
						display.StructuredResult(result.task, parsed, transition)
						switch transition.To {
						case "completed":
							entry.Status, entry.Summary, entry.Error = voyage.Completed, parsed.Summary, ""
							display.Completed(result.task, parsed.Summary)
						case "retrying", "retrying_infrastructure":
							if transition.To == "retrying" {
								entry.Attempt++
							}
							entry.Status, entry.Error = voyage.Pending, "structured retry scheduled"
							entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
							display.Escalated(result.task, entry.Attempt, transition.NextModel+"/"+transition.NextEffort)
						case "needs_input":
							entry.Status, entry.Error = voyage.NeedsInput, parsed.Summary
							display.InputRequired(result.task, parsed.Summary)
							if adviceErr := adviseStructuredBlocker(ctx, recoveryRuntime, plan, hash, result.task, entry, recovery.ReasonUncertainty, parsed.Summary, display); adviceErr != nil {
								persistErr = adviceErr
								cancelBatch()
								continue
							}
						case "no_go", "stopped":
							entry.Status, entry.Error = voyage.Failed, parsed.Summary
							if parsed.Outcome == recovery.OutcomeNoGo {
								successor, successorErr := recovery.InstantiateCorrective(plan, result.task, parsed, canonicalResult)
								if successorErr == nil {
									if _, appendErr := result.ledger.Append("corrective_successor", result.attemptID, successor, time.Now().UTC()); appendErr != nil {
										persistErr = appendErr
										cancelBatch()
										continue
									}
									display.CorrectiveLineage(result.task, successor)
								} else if !errors.Is(successorErr, recovery.ErrNoAuthorizedTemplate) {
									persistErr = successorErr
									cancelBatch()
									continue
								}
							}
							display.Failed(result.task, parsed.Summary)
							if adviceErr := adviseStructuredBlocker(ctx, recoveryRuntime, plan, hash, result.task, entry, recovery.ReasonExhaustedTiers, parsed.Summary, display); adviceErr != nil {
								persistErr = adviceErr
								cancelBatch()
								continue
							}
						default:
							entry.Status, entry.Error = voyage.Failed, "result_contract_failure: invalid transition"
						}
					}
				}
			} else if result.err != nil {
				if ctx.Err() != nil {
					entry.Status, entry.Error = voyage.Pending, "captain canceled the voyage; task is safe to resume"
					entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
					display.Blocked(result.task, "canceled; pending resume")
				} else if isManagedSessionFailure(result.err) {
					entry.Error = boundedSailText(result.err.Error(), 2048)
					advice, advised, adviceErr := recoveryRuntime.advise(ctx, plan, hash, result.task, entry, recovery.ReasonManagedEnvironmentMismatch, result.err.Error())
					if adviceErr != nil {
						persistErr = adviceErr
						cancelBatch()
						continue
					}
					if advised && (advice.Decision == recovery.DecisionResume || advice.Decision == recovery.DecisionContinue) {
						if err := activateDerivative(recoveryRuntime.takeDerivative(advice.Fingerprint)); err != nil {
							persistErr = err
							cancelBatch()
							continue
						}
						display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), result.err.Error(), "accepted: retry/resume")
						entry.Status, entry.Attempt = voyage.Pending, 0
						entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
						display.Escalated(result.task, entry.Attempt, "auto-captain authorized bounded recovery")
					} else if advised && advice.Decision == recovery.DecisionAmendmentRequired {
						display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), result.err.Error(), "rejected: amendment inert; Captain approval required")
						entry.Status = voyage.NeedsInput
						display.InputRequired(result.task, "Captain approval is required for a material amendment")
					} else {
						entry.Status = voyage.Failed
						display.Failed(result.task, entry.Error+"; no authorized recovery remains")
					}
				} else {
					entry.Error = boundedSailText(result.err.Error(), 2048)
					if entry.Attempt+1 < result.task.TierCount() {
						entry.Attempt++
						entry.Status = voyage.Pending
						entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
						display.Escalated(result.task, entry.Attempt, entry.Error)
					} else {
						entry.Status = voyage.Failed
						advice, advised, adviceErr := recoveryRuntime.advise(ctx, plan, hash, result.task, entry, recovery.ReasonExhaustedTiers, result.err.Error())
						if adviceErr != nil {
							persistErr = adviceErr
							cancelBatch()
							continue
						}
						if advised && (advice.Decision == recovery.DecisionResume || advice.Decision == recovery.DecisionContinue) {
							if err := activateDerivative(recoveryRuntime.takeDerivative(advice.Fingerprint)); err != nil {
								persistErr = err
								cancelBatch()
								continue
							}
							display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), result.err.Error(), "accepted: retry/resume")
							entry.Status, entry.Attempt = voyage.Pending, 0
							entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
							display.Escalated(result.task, entry.Attempt, "auto-captain authorized bounded recovery")
						} else if advised && advice.Decision == recovery.DecisionAmendmentRequired {
							display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), result.err.Error(), "rejected: amendment inert; Captain approval required")
							entry.Status = voyage.NeedsInput
							display.InputRequired(result.task, "Captain approval is required for a material amendment")
						} else {
							display.Failed(result.task, entry.Error+"; no authorized recovery remains")
						}
					}
				}
			} else if question, needsInput := sailInputRequest(result.output); needsInput {
				advice, advised, adviceErr := recoveryRuntime.advise(ctx, plan, hash, result.task, entry, recovery.ReasonUncertainty, question)
				if adviceErr != nil {
					persistErr = adviceErr
					cancelBatch()
					continue
				}
				if advised && (advice.Decision == recovery.DecisionResume || advice.Decision == recovery.DecisionContinue) {
					if err := activateDerivative(recoveryRuntime.takeDerivative(advice.Fingerprint)); err != nil {
						persistErr = err
						cancelBatch()
						continue
					}
					display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), question, "accepted: retry/resume")
					entry.Status, entry.Attempt = voyage.Pending, 0
					entry.Error, entry.Summary = "", ""
					display.Escalated(result.task, entry.Attempt, "auto-captain rejected false pushback and resumed task")
				} else {
					if advised {
						display.Recovery(result.task, entry.Attempt, advice, string(advice.Reason), question, "rejected: Captain input required")
					}
					entry.Status, entry.Error = voyage.NeedsInput, question
					entry.Summary = ""
					display.InputRequired(result.task, question)
				}
			} else {
				if err := voyage.ApplyAcceptanceOutput(state, plan, result.task.ID, result.output, time.Now().UTC()); err != nil {
					// A malformed or unauthorized marker invalidates the result
					// before task completion is persisted and can never publish pass.
					persistErr = err
					cancelBatch()
					continue
				}
				entry.Status, entry.Summary = voyage.Completed, boundedSailText(result.output, 8192)
				display.Completed(result.task, entry.Summary)
			}
			if err := beadGraph.finish(ctx, entry); err != nil {
				display.BeadWarning(result.task, err)
			}
			state.Tasks[result.task.ID] = entry
			if err := save(); err != nil {
				persistErr = err
				cancelBatch()
			}
		}
		cancelBatch()
		if persistErr != nil {
			return persistErr
		}
	}

	completed, failed, blocked := 0, 0, 0
	for _, entry := range state.Tasks {
		switch entry.Status {
		case voyage.Completed:
			completed++
		case voyage.Failed:
			failed++
		case voyage.Blocked:
			blocked++
		case voyage.NeedsInput:
			blocked++
		}
	}
	display.Footer(completed, failed, blocked, statePath)
	if failed > 0 || blocked > 0 || completed != len(plan.Tasks) {
		return fmt.Errorf("voyage incomplete: %d completed, %d failed, %d blocked", completed, failed, blocked)
	}
	return nil
}

func safeVoyagePlanPath(path string) (string, error) {
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return "", errors.New("voyage project root is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("voyage plan path is invalid")
	}
	abs = filepath.Clean(abs)
	originalInfo, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !originalInfo.Mode().IsRegular() || originalInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("voyage plan must be a regular file")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("voyage plan path is invalid")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("voyage plan path is invalid")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("voyage plan must be inside the project")
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("voyage plan must be a regular file")
	}
	return resolved, nil
}

func safeVoyageStatePath(path string) (string, error) {
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return "", errors.New("voyage project root is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("voyage state path is invalid")
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > voyage.MaxStateBytes {
		return "", errors.New("voyage state must be a bounded regular file")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("voyage state path is invalid")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("voyage state must be inside the project")
	}
	return resolved, nil
}

func sailTaskPrompt(plan *voyage.Plan, task voyage.Task) string {
	prompt := fmt.Sprintf("You are crew on the captain-approved voyage %q.\nObjective: %s\nYour bounded job: %s\n\n%s\n\nComplete the job in the workspace, verify your work, and report concrete results. Do not broaden the approved scope or recursively invoke Shipmates. If progress genuinely requires a Captain decision or unavailable input, do not guess or claim completion; return exactly SHIPMATES_NEEDS_INPUT: followed by one bounded question and its relevant options.", plan.Title, plan.Objective, task.Summary, task.Prompt)
	if structuredEnabled(task) {
		prompt += "\n\nThis task opted into crew.result.v1. Your final response must be exactly one JSON object matching the closed crew.result.v1 contract; do not return prose, markdown, credentials, paths, or extra JSON. Bind the result to the supplied task and attempt identity, report only bounded redacted evidence digests, and set verifier.status=no_go whenever the verifier proves NO-GO."
	}
	return prompt
}

func sailInputRequest(output string) (string, bool) {
	const prefix = "SHIPMATES_NEEDS_INPUT:"
	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, prefix) {
		return "", false
	}
	question := boundedSailText(strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)), 2048)
	return question, question != ""
}

func sailTaskConfig(task voyage.Task, attempt int) (project.PersonaConfig, error) {
	cfg, err := project.ResolvePersonaConfig(task.Persona)
	if err != nil {
		return cfg, err
	}
	models := append([]string(nil), task.Models...)
	efforts := append([]string(nil), task.Efforts...)
	if structuredEnabled(task) {
		models = append([]string(nil), task.Recovery.Models...)
		efforts = append([]string(nil), task.Recovery.Efforts...)
	}
	if len(models) == 0 {
		ladder, ladderErr := project.ModelLadderAt(".")
		if ladderErr != nil {
			return cfg, ladderErr
		}
		if len(ladder) == 0 {
			return cfg, errors.New("shipmates.yaml modelLadder is required for sail")
		}
		models = []string{ladder[0]}
	}
	if structuredEnabled(task) && len(efforts) == 0 {
		return cfg, errors.New("structured recovery effort ladder is required")
	}
	if len(efforts) == 0 {
		efforts = []string{"low"}
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(models)*len(efforts) {
		attempt = len(models)*len(efforts) - 1
	}
	cfg.Model = strings.TrimSpace(models[attempt/len(efforts)])
	cfg.Effort = efforts[attempt%len(efforts)]
	return cfg, nil
}

func validateSailModelLadders(plan *voyage.Plan) error {
	ladder, err := project.ModelLadderAt(".")
	if err != nil {
		return err
	}
	if len(ladder) == 0 {
		return errors.New("shipmates.yaml modelLadder is required for sail")
	}
	rank := make(map[string]int, len(ladder))
	for i, model := range ladder {
		rank[model] = i
	}
	for _, task := range plan.Tasks {
		last := -1
		models := task.Models
		if structuredEnabled(task) {
			models = task.Recovery.Models
		}
		for _, model := range models {
			i, ok := rank[strings.TrimSpace(model)]
			if !ok {
				return fmt.Errorf("task %q model %q is not in shipmates.yaml modelLadder", task.ID, model)
			}
			if i < last {
				return fmt.Errorf("task %q model ladder must progress from least to most capable", task.ID)
			}
			last = i
		}
	}
	return nil
}

func dispatchManagedSailTask(ctx context.Context, installed *project.InstalledPersona, prompt string, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
	return dispatchManagedCodex(ctx, installed, prompt, false, cfg, nil, stdout, stderr)
}

func dispatchManagedCodex(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, images []turninput.ImageDescriptorV1, stdout, stderr io.Writer) error {
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return err
	}
	manager := livesession.NewAt(root, nil, codexapp.StartOptions{})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		manager.ShutdownAll(shutdownCtx)
	}()
	trace, traced := ctx.Value(sailTraceContextKey{}).(sailTrace)
	session, err := manager.StartLive(ctx, livesession.StartOptions{Persona: installed.Name, Prompt: prompt, Fresh: fresh, Images: images, Config: &cfg, ExposeActivityDetails: traced})
	if err != nil {
		return err
	}
	var after uint64
	var final string
	for {
		feed := session.Feed(after)
		for _, event := range feed.Events {
			if event.Sequence > after {
				after = event.Sequence
			}
			switch event.Kind {
			case "agent.message":
				if text, ok := event.Data["text"].(string); ok {
					final = text
					partial, _ := event.Data["partial"].(bool)
					if traced && !partial {
						trace.Agent(text)
					}
				}
			case "activity":
				if category, ok := event.Data["category"].(string); ok {
					detail, _ := event.Data["detail"].(string)
					if traced {
						trace.Activity(category, detail)
					} else {
						_, _ = fmt.Fprintln(stderr, "activity: "+category)
					}
				}
			case "approval.pending":
				manager.CancelPendingApproval(ctx, event.SessionID, event.ThreadID, event.TurnID)
			}
		}
		snapshot := session.Snapshot()
		if snapshot.State == livesession.Failed || snapshot.State == livesession.Stopped {
			if snapshot.FailureCode != "" {
				return &managedSessionFailure{state: snapshot.State, code: string(snapshot.FailureCode)}
			}
			return &managedSessionFailure{state: snapshot.State}
		}
		if snapshot.State == livesession.Idle && snapshot.TurnID == "" {
			if strings.TrimSpace(final) == "" {
				return errors.New("managed Codex task completed without a final response")
			}
			_, _ = fmt.Fprintln(stdout, final)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
		case <-session.Notify():
		case <-time.After(250 * time.Millisecond):
		}
	}
}

type managedSessionFailure struct {
	state livesession.State
	code  string
}

func (e *managedSessionFailure) Error() string {
	if e.code != "" {
		return fmt.Sprintf("managed Codex task ended in state %s: %s", e.state, e.code)
	}
	return fmt.Sprintf("managed Codex task ended in state %s", e.state)
}

func isManagedSessionFailure(err error) bool {
	var failure *managedSessionFailure
	return errors.As(err, &failure)
}

func persistedInfrastructureFailure(message string) bool {
	return strings.HasPrefix(strings.TrimSpace(message), "managed Codex task ended in state ")
}

type sailDisplay struct {
	w        io.Writer
	color    bool
	mu       *sync.Mutex
	activity map[string]sailActivityState
	verbose  bool
}

type sailTraceContextKey struct{}

type sailTrace struct {
	display sailDisplay
	task    voyage.Task
	cfg     project.PersonaConfig
}

func (t sailTrace) Activity(category, detail string) {
	if detail != "" {
		t.display.Detail(t.task, t.cfg, category, detail)
		return
	}
	t.display.Activity(t.task, t.cfg, category)
}

func (t sailTrace) Agent(text string) { t.display.Agent(t.task, t.cfg, text) }

type sailActivityState struct {
	text string
	at   time.Time
}

func newSailDisplay(w io.Writer, requested bool) sailDisplay {
	enabled := requested && os.Getenv("NO_COLOR") == ""
	if file, ok := w.(*os.File); ok {
		if info, err := file.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
			enabled = false
		}
	} else {
		enabled = false
	}
	return sailDisplay{w: w, color: enabled, mu: &sync.Mutex{}, activity: make(map[string]sailActivityState)}
}

func (d sailDisplay) persona(name string) string {
	if !d.color {
		return name
	}
	return fmt.Sprintf("\x1b[1;38;5;%dm%s\x1b[0m", personaColor(name), name)
}

func (d sailDisplay) personaLabel(name string) string {
	label := fmt.Sprintf("%-16s", name)
	if !d.color {
		return label
	}
	return fmt.Sprintf("\x1b[1;38;5;%dm%s\x1b[0m", personaColor(name), label)
}

func personaColor(name string) int {
	known := map[string]int{
		"skipper": 45, "quartermaster": 214, "architect": 141,
		"backend": 39, "frontend": 170, "security": 208, "tester": 75,
	}
	if color, ok := known[name]; ok {
		return color
	}
	palette := []int{33, 69, 81, 111, 177}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return palette[int(h.Sum32())%len(palette)]
}

func (d sailDisplay) Header(p *voyage.Plan, hash string, dry bool) {
	mode := "SAILING"
	if dry {
		mode = "DRY RUN"
	}
	d.printf("\nSHIPMATES %s  %s\n%s\nVoyage %s\nAUTO-CAPTAIN  Sail validates bounded machine-approved derivatives; Captain approval required for material amendments\nCONTROL  Ctrl+C cancels active crew and preserves resumable state\n\n", mode, hash, strings.Repeat("=", 64), p.Title)
}
func (d sailDisplay) RecoveryConfig(enabled bool) {
	status := "disabled"
	if enabled {
		status = "enabled"
	}
	d.printf("AUTO-CAPTAIN STATE  %s | Sol=gpt-5.6-sol | derivatives=bounded-envelope-only | Captain approval=required for material amendments\n", status)
}
func (d sailDisplay) Lineage(state *voyage.State, hash string) {
	if state == nil {
		return
	}
	line := "LINEAGE  successor=" + shortHash(hash)
	if state.Lineage != nil {
		line += " predecessor=" + shortHash(state.Lineage.PredecessorPlanHash)
	}
	d.printf("%s\n", line)
}
func (d sailDisplay) PlanTask(t voyage.Task, s voyage.TaskState) {
	if s.Inherited != nil {
		d.printf("  %-10s %s %s (from predecessor %s)\n", "INHERITED", d.personaLabel(t.Persona), t.Summary, s.Inherited.PredecessorPlanHash[:12])
		return
	}
	d.printf("  %-10s %s %s\n", strings.ToUpper(string(s.Status)), d.personaLabel(t.Persona), t.Summary)
}
func (d sailDisplay) Started(t voyage.Task, cfg project.PersonaConfig) {
	d.printf("\n> %s STARTED  %s\n  MODEL %-24s EFFORT %s\n", d.personaLabel(t.Persona), t.Summary, cfg.Model, cfg.Effort)
}
func (d sailDisplay) Brief(t voyage.Task, cfg project.PersonaConfig, prompt string) {
	d.printf("\n  %s  [%s | %s]  TASK BRIEF\n%s\n", d.persona(t.Persona), cfg.Model, cfg.Effort, indentSailText(prompt, "    "))
}
func (d sailDisplay) Blocked(t voyage.Task, why string) {
	d.printf("! %s blocked   %s (%s)\n", d.personaLabel(t.Persona), t.Summary, why)
}
func (d sailDisplay) Failed(t voyage.Task, why string) {
	d.printf("x %s failed    %s\n  %s\n", d.personaLabel(t.Persona), t.Summary, boundedSailText(why, 512))
}
func (d sailDisplay) BeadWarning(t voyage.Task, err error) {
	d.printf("! %s beads     %s\n", d.personaLabel(t.Persona), boundedSailText(err.Error(), 512))
}
func (d sailDisplay) InputRequired(t voyage.Task, question string) {
	d.printf("? %s needs Captain input for %s\n  %s\n", d.personaLabel(t.Persona), t.Summary, boundedSailText(question, 512))
}
func (d sailDisplay) Escalated(t voyage.Task, attempt int, why string) {
	d.printf("^ %s escalating %s to tier %d after: %s\n", d.personaLabel(t.Persona), t.Summary, attempt+1, boundedSailText(why, 256))
}
func (d sailDisplay) StructuredResult(t voyage.Task, result recovery.CrewResult, transition recovery.RecoveryTransition) {
	remaining := uint32(0)
	if result.Tokens.Reserved >= result.Tokens.Used {
		remaining = result.Tokens.Reserved - result.Tokens.Used
	}
	evidence := "none"
	if len(result.Evidence)+len(result.Verifier.Evidence) > 0 {
		evidence = fmt.Sprintf("%d refs", len(result.Evidence)+len(result.Verifier.Evidence))
	}
	d.printf("  STRUCTURED %s | slot=crew-result | current=%s/%s | next=%s | outcome=%s | reason=%s | tokens=%d/%d used/remaining=%d | evidence=%s | verifier=%s\n", t.ID, safeSailWord(result.Model), safeSailWord(result.Effort), structuredNextLabel(transition), result.Outcome, result.Reason, result.Tokens.Used, result.Tokens.Reserved, remaining, evidence, result.Verifier.Status)
	if transition.To == "retrying" && transition.NextModel != "" {
		d.printf("  TRANSITION %s: %s/%s -> %s/%s | cause=%s | action=%s\n", t.ID, safeSailWord(result.Model), safeSailWord(result.Effort), safeSailWord(transition.NextModel), safeSailWord(transition.NextEffort), result.Reason, transition.Action)
	} else {
		d.printf("  TRANSITION %s: running -> %s | cause=%s | action=%s\n", t.ID, transition.To, result.Reason, transition.Action)
	}
}
func (d sailDisplay) StructuredInfrastructureRetry(t voyage.Task, cfg project.PersonaConfig, attempt uint8) {
	d.printf("  STRUCTURED %s | slot=infrastructure | current=%s/%s | retry=%d | outcome=infrastructure_retry | next=same-tier | action=retry\n", t.ID, safeSailWord(cfg.Model), safeSailWord(cfg.Effort), attempt)
}
func (d sailDisplay) CorrectiveLineage(t voyage.Task, successor recovery.CorrectiveSuccessor) {
	d.printf("  CORRECTIVE %s -> %s | bead=%s | verify=%s bead=%s | evidence=%s\n", t.ID, successor.Task.ID, safeSailWord(successor.TaskBeadID), successor.VerificationTask.ID, safeSailWord(successor.VerificationBeadID), shortHash(successor.Lineage.ResultHash))
}
func (d sailDisplay) Recovery(t voyage.Task, attempt int, response recovery.ResponseV1, reason, evidence, accepted string) {
	decision := string(response.Decision)
	if decision == "" {
		decision = "not assessed"
	}
	fp := response.Fingerprint
	if len(fp) > 12 {
		fp = fp[:12]
	}
	base := response.PersonaDigest
	if len(base) > 12 {
		base = base[:12]
	}
	d.printf("  AUTO-CAPTAIN %s | model=%s | skipper=%s/v%d | enabled=yes | blocker=%s | attempts=%d | fingerprint=%s | recommendation=%s | action=%s | approval=%s | safe-options=retry/resume-or-stop | evidence=sail#%s\n", t.ID, response.EffectiveModelOrDefault(), base, response.PersonaVersion, reason, attempt+1, fp, decision, accepted, "required for amendment", shortHash(digest(evidence)))
}
func (d sailDisplay) Completed(t voyage.Task, summary string) {
	d.printf("+ %s completed %s\n", d.personaLabel(t.Persona), t.Summary)
	if summary != "" && !d.verbose {
		d.printf("  %s\n", boundedSailText(summary, 1024))
	}
}

func (d sailDisplay) Detail(t voyage.Task, cfg project.PersonaConfig, category, detail string) {
	label := strings.ToUpper(strings.ReplaceAll(category, "_", " "))
	d.printf("\n  %s  [%s | %s]  %s\n%s\n", d.persona(t.Persona), cfg.Model, cfg.Effort, label, indentSailText(detail, "    "))
}

func (d sailDisplay) Agent(t voyage.Task, cfg project.PersonaConfig, text string) {
	d.printf("\n  %s  [%s | %s]  REPORT\n%s\n", d.persona(t.Persona), cfg.Model, cfg.Effort, indentSailText(boundedSailText(text, 32<<10), "    "))
}
func (d sailDisplay) Footer(completed, failed, blocked int, path string) {
	d.printf("\n%s\nDONE %d  FAILED %d  BLOCKED %d\nState: %s\n", strings.Repeat("=", 64), completed, failed, blocked, path)
}

func (d sailDisplay) Canceling() {
	d.printf("\n! Captain interrupt received; canceling crew and preserving resumable voyage state...\n")
}

func (d sailDisplay) Activity(t voyage.Task, cfg project.PersonaConfig, text string) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "activity:"))
	if text == "" || text == "other" {
		return
	}
	text = boundedSailText(text, 256)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.activity == nil {
		d.activity = make(map[string]sailActivityState)
	}
	previous := d.activity[t.ID]
	if now.Sub(previous.at) < 2*time.Second || (previous.text == text && now.Sub(previous.at) < 10*time.Second) {
		return
	}
	d.activity[t.ID] = sailActivityState{text: text, at: now}
	_, _ = fmt.Fprintf(d.w, "· %s %-9s model=%s effort=%s\n", d.personaLabel(t.Persona), text, cfg.Model, cfg.Effort)
}

func (d sailDisplay) printf(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprintf(d.w, format, args...)
}

func safeSailWord(value string) string {
	value = boundedSailText(value, 96)
	value = strings.NewReplacer(" ", "_", "\n", "?", "\t", "?").Replace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func structuredNextLabel(transition recovery.RecoveryTransition) string {
	if transition.NextModel == "" {
		return "none"
	}
	return safeSailWord(transition.NextModel) + "/" + safeSailWord(transition.NextEffort)
}

type sailActivityWriter struct {
	display sailDisplay
	task    voyage.Task
	cfg     project.PersonaConfig
}

func (w sailActivityWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line != "" {
			w.display.Activity(w.task, w.cfg, line)
		}
	}
	return len(p), nil
}

func boundedSailText(text string, limit int) string {
	text = strings.ToValidUTF8(strings.TrimSpace(text), "?")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '?'
		}
		return r
	}, text)
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + "..."
}

func indentSailText(text, prefix string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return prefix + "(no detail)"
	}
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}

type boundedOutput struct {
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func newBoundedOutput(limit int) *boundedOutput { return &boundedOutput{remaining: limit} }
func (w *boundedOutput) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
		w.truncated = true
	}
	_, _ = w.buf.Write(p)
	w.remaining -= len(p)
	return original, nil
}
func (w *boundedOutput) Len() int { return w.buf.Len() }
func (w *boundedOutput) String() string {
	s := w.buf.String()
	if w.truncated {
		s += "\n[output truncated]"
	}
	return s
}
