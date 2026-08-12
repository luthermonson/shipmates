package commands

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

// Plan validates and inspects the voyage draft and its persisted state.
//
// The reference implementation of this command (the closed
// feat/runtime-interface branch) was an interactive planning room built on
// that branch's managed-session TUI. Main has no in-process interactive
// attach for a planning conversation, so this command does the honest
// subset: strict host-side validation of the draft, and a truthful
// projection of the plan joined with its persisted voyage state, acceptance
// verdict, and structured-recovery ledgers. Planning conversations happen
// with the first mate persona through the ordinary session surfaces
// (`shipmates open first-mate`, `shipmates ask first-mate ...`).
func Plan() *cli.Command {
	return &cli.Command{
		Name:  "plan",
		Usage: "validate the voyage draft and show its status; the admiral commissions it, then sail executes it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "plan", Value: defaultVoyagePlan, Usage: "voyage plan path"},
			&cli.BoolFlag{Name: "fresh", Usage: "clear the active voyage draft"},
		},
		Action: runPlan,
	}
}

func runPlan(ctx context.Context, c *cli.Command) error {
	path := c.String("plan")
	if c.Bool("fresh") {
		if err := clearActiveVoyage(path); err != nil {
			return err
		}
		fmt.Fprintf(c.Writer, "cleared voyage draft %s\n", path)
		return nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(strings.TrimSpace(string(raw))) == 0) {
		fmt.Fprintf(c.Writer, "no voyage draft at %s\nPlan a voyage with the first mate (shipmates open first-mate), let it write the structured draft, then commission it with: shipmates commission\nSchema: docs/sailing.md\n", path)
		return nil
	}
	if err != nil {
		return err
	}
	summary, err := voyageSummary(path)
	if err != nil {
		return err
	}
	_, err = io.WriteString(c.Writer, summary)
	return err
}

// Commission records the admiral's execution authorization on the voyage
// draft. Structurally, commissioning is reserved to the human: the first mate
// (or any persona) writes the plan with commissioned false, and this command
// refuses to run inside any agent-managed turn, so nothing a persona can do
// through Shipmates flips the commissioned state.
func Commission() *cli.Command {
	return &cli.Command{
		Name:  "commission",
		Usage: "commission the voyage draft for execution (an admiral act; refused inside agent turns)",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "plan", Value: defaultVoyagePlan, Usage: "voyage plan path"},
		},
		Action: runCommission,
	}
}

// agentTurnEnvironment reports whether this process is running inside an
// agent's turn: a sail-managed crew session (SHIPMATES_MANAGED_SESSION) or
// any Claude Code tool subprocess (CLAUDECODE). Commissioning from either is
// refused — the commission is the admiral's own act at their own terminal.
func agentTurnEnvironment() string {
	if os.Getenv(codexapp.ManagedSessionEnvironment) == "1" {
		return codexapp.ManagedSessionEnvironment
	}
	if os.Getenv("CLAUDECODE") != "" {
		return "CLAUDECODE"
	}
	return ""
}

func runCommission(ctx context.Context, c *cli.Command) error {
	if marker := agentTurnEnvironment(); marker != "" {
		return fmt.Errorf("commissioning is an admiral act and cannot be performed from inside an agent turn (%s is set); the admiral runs shipmates commission from their own terminal", marker)
	}
	path := c.String("plan")
	plan, _, err := voyage.LoadDraft(path)
	if err != nil {
		return fmt.Errorf("cannot commission: %w", err)
	}
	if plan.Commissioned {
		fmt.Fprintf(c.Writer, "voyage is already commissioned; run: shipmates sail\n")
		return nil
	}
	plan.Commissioned = true
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".voyage-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := project.DurableRename(name, path); err != nil {
		return err
	}
	summary, err := voyageSummary(path)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(c.Writer, summary); err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Writer, "\ncommissioned %s — set sail with: shipmates sail\n", path)
	return err
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

// invalidVoyageDraft returns a stable content hash and the validation error
// when the draft exists but fails host-side validation; a missing or empty
// draft is normal before planning begins.
func invalidVoyageDraft(path string) ([32]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return [32]byte{}, nil
	}
	_, _, err = voyage.LoadDraft(path)
	if err == nil {
		return [32]byte{}, nil
	}
	return sha256Sum(raw), err
}

// voyageSummary is the host-side projection of a voyage draft joined with its
// persisted state. It deliberately re-reads both plan and state on every call
// instead of retaining a prior acceptance projection: a missing, legacy,
// malformed, stale, or mismatched persisted state cannot prove acceptance and
// renders as unknown, never as a cached success.
func voyageSummary(path string) (string, error) {
	var b strings.Builder
	p, canonical, err := voyage.LoadDraft(path)
	if err != nil {
		return "", fmt.Errorf("voyage draft is invalid: %w", err)
	}
	hash := voyage.Hash(canonical)
	status := "DRAFT - awaiting the admiral's commission (run: shipmates commission)"
	if p.Commissioned {
		status = "COMMISSIONED - run: shipmates sail"
	}
	var state *voyage.State
	var stateLoadErr error
	stateExists := false
	statePath := filepath.Join(project.Dir, "voyages", hash[:16]+".json")
	if _, statErr := os.Stat(statePath); statErr == nil {
		stateExists = true
		state, stateLoadErr = voyage.LoadState(statePath, p, hash)
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
		status = "INCOMPLETE - review blockers, then shipmates sail --retry-failed"
	} else if p.Commissioned && stateExists && (state == nil || stateLoadErr != nil) {
		// A legacy, malformed, stale, or mismatched persisted state cannot
		// prove acceptance. Keep the plan actionable, but make unknown
		// explicit rather than allowing a cached or inferred success notice.
		// (No state file at all is simply a not-yet-sailed commissioned plan.)
		status = "COMMISSIONED - acceptance unknown"
	}
	fmt.Fprintf(&b, "%s\n%s\nPlan %s  (%s)\n\n", p.Title, status, shortHash(hash), path)
	section := func(name string, values []string) {
		if len(values) == 0 {
			return
		}
		fmt.Fprintf(&b, "%s:\n", name)
		for _, value := range values {
			fmt.Fprintf(&b, "  - %s\n", boundedSailText(value, 512))
		}
	}
	section("Objective", []string{objective})
	section("Scope", p.Scope)
	section("Non-goals", p.NonGoals)
	section("Blast area", p.BlastArea)
	section("Risks", p.Risks)
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
	section("Acceptance", acceptance)
	section("Open decisions", p.OpenDecisions)
	jobs := make([]string, 0, len(p.Tasks))
	for _, task := range p.Tasks {
		taskStatus := "pending"
		record := ""
		if state != nil {
			taskStatus = string(state.Tasks[task.ID].Status)
			if id := state.Tasks[task.ID].BeadID; id != "" {
				record = " task=" + id
			}
			if state.Tasks[task.ID].Inherited != nil {
				taskStatus = "inherited"
			}
		}
		jobs = append(jobs, fmt.Sprintf("[%s] %s [%s]%s %s", taskStatus, task.ID, task.Persona, record, task.Summary))
	}
	section("Jobs", jobs)
	if items := structuredRecoverySummary(hash, p); len(items) > 0 {
		section("Structured Sail recovery", items)
	}
	return b.String(), nil
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

// structuredRecoverySummary is a bounded projection of the append-only
// semantic ledger. It never renders prompts, summaries, paths, or raw records.
func structuredRecoverySummary(planHash string, p *voyage.Plan) []string {
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
				next = safeSummary(entry.NextModel, 32) + "/" + safeSummary(entry.NextEffort, 16)
			}
			line := fmt.Sprintf("%s: slot=crew-result | current=%s/%s | next=%s | budget=%d reserved,%d used,%d remaining | outcome=%s | reason=%s", task.ID, safeSummary(entry.Model, 64), safeSummary(entry.Effort, 16), next, entry.Tokens.Reserved, entry.Tokens.Used, remaining, safeSummary(stringOrUnknown(entry.Outcome), 32), safeSummary(stringOrUnknown(entry.Reason), 40))
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
				line += " | transition=" + safeSummary(entry.Model, 32) + "/" + safeSummary(entry.Effort, 16) + " -> " + safeSummary(entry.NextModel, 32) + "/" + safeSummary(entry.NextEffort, 16) + " cause=" + safeSummary(entry.Cause, 32)
			}
			if entry.To == "needs_input" {
				line += " | pause=admiral input required"
			}
			if entry.Verifier == recovery.VerifierNoGo {
				line += " | verifier=NO-GO"
			}
			if entry.Corrective != "" {
				line += " | successor=" + safeSummary(entry.Corrective, 32) + " task=" + safeSummary(entry.CorrectiveBead, 24) + " follow-up=" + safeSummary(entry.Verification, 32) + " task=" + safeSummary(entry.VerificationBead, 24)
			}
			if entry.To != "" {
				line += " | state=" + safeSummary(entry.To, 32)
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

func safeSummary(value string, limit int) string { return boundedSailText(value, limit) }

func sha256Sum(raw []byte) [32]byte { return sha256.Sum256(raw) }
