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
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
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
	statePath := filepath.Join(project.Dir, "voyages", hash[:16]+".json")
	var releaseVoyage func()
	if !c.Bool("dry-run") {
		releaseVoyage, err = project.AcquireDispatchLock("voyage-" + hash[:16])
		if err != nil {
			return fmt.Errorf("voyage is already sailing: %w", err)
		}
		defer releaseVoyage()
	}
	state, err := voyage.LoadState(statePath, plan, hash)
	if err != nil {
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
	order, _ := plan.Order()
	byID := make(map[string]voyage.Task, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byID[task.ID] = task
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
		for _, task := range ready {
			cfg, cfgErr := sailTaskConfig(task, state.Tasks[task.ID].Attempt)
			if cfgErr != nil {
				return cfgErr
			}
			dispatchConfigs[task.ID] = cfg
			entry := state.Tasks[task.ID]
			entry.Status, entry.StartedAt = voyage.Running, time.Now().UTC()
			entry.Error = ""
			state.Tasks[task.ID] = entry
			display.Started(task, cfg)
			if c.Bool("verbose") {
				display.Brief(task, cfg, beadGraph.prompt(sailTaskPrompt(plan, task), task, entry))
			}
		}
		if err := save(); err != nil {
			return err
		}

		type result struct {
			task   voyage.Task
			output string
			err    error
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
			work := dispatch{task: task, attempt: entry.Attempt, cfg: dispatchConfigs[task.ID],
				prompt: beadGraph.prompt(sailTaskPrompt(plan, task), task, entry)}
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
				results <- result{task: task, output: strings.TrimSpace(stdout.String()), err: err}
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
			if result.err != nil {
				if ctx.Err() != nil {
					entry.Status, entry.Error = voyage.Pending, "captain canceled the voyage; task is safe to resume"
					entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
					display.Blocked(result.task, "canceled; pending resume")
				} else if isManagedSessionFailure(result.err) {
					entry.Status = voyage.Failed
					entry.Error = boundedSailText(result.err.Error(), 2048)
					display.Failed(result.task, entry.Error+"; infrastructure failures do not consume model escalation")
				} else {
					entry.Error = boundedSailText(result.err.Error(), 2048)
					if entry.Attempt+1 < result.task.TierCount() {
						entry.Attempt++
						entry.Status = voyage.Pending
						entry.StartedAt, entry.FinishedAt = time.Time{}, time.Time{}
						display.Escalated(result.task, entry.Attempt, entry.Error)
					} else {
						entry.Status = voyage.Failed
						display.Failed(result.task, entry.Error)
					}
				}
			} else if question, needsInput := sailInputRequest(result.output); needsInput {
				entry.Status, entry.Error = voyage.NeedsInput, question
				entry.Summary = ""
				display.InputRequired(result.task, question)
			} else {
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

func sailTaskPrompt(plan *voyage.Plan, task voyage.Task) string {
	return fmt.Sprintf("You are crew on the captain-approved voyage %q.\nObjective: %s\nYour bounded job: %s\n\n%s\n\nComplete the job in the workspace, verify your work, and report concrete results. Do not broaden the approved scope or recursively invoke Shipmates. If progress genuinely requires a Captain decision or unavailable input, do not guess or claim completion; return exactly SHIPMATES_NEEDS_INPUT: followed by one bounded question and its relevant options.", plan.Title, plan.Objective, task.Summary, task.Prompt)
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
	efforts := append([]string(nil), task.Efforts...)
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
		for _, model := range task.Models {
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
	d.printf("\nSHIPMATES %s  %s\n%s\nVoyage %s\nCONTROL  Ctrl+C cancels active crew and preserves resumable state\n\n", mode, hash, strings.Repeat("=", 64), p.Title)
}
func (d sailDisplay) PlanTask(t voyage.Task, s voyage.TaskState) {
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
