package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime/env"
	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

// isolateSailEnvironment shields a sail test from the developer machine: the
// runtime selector reads a throwaway user home, and the managed-session and
// runtime environment variables are cleared.
func isolateSailEnvironment(t *testing.T) {
	t.Helper()
	prev := selector
	selector = &env.Selector{UserHome: t.TempDir()}
	t.Cleanup(func() { selector = prev })
	t.Setenv("SHIPMATES_RUNTIME", "")
	t.Setenv(codexapp.ManagedSessionEnvironment, "")
}

func sailTestProject(t *testing.T, plan voyage.Plan) string {
	t.Helper()
	isolateSailEnvironment(t)
	root := t.TempDir()
	// t.TempDir may live behind a symlink (macOS /tmp); sail resolves its
	// project root through EvalSymlinks, so fixtures must too.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".claude/agents", ".shipmates"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "shipmates.yaml"), []byte("sessionPrefix: test\nmodelLadder: [small, large]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, persona := range []string{"backend", "tester"} {
		body := []byte("---\nmodel: \n---\n\nYou are the test " + persona + " persona.\n")
		if err := os.WriteFile(filepath.Join(root, ".claude", "agents", persona+".md"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".shipmates", "voyage.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func runSailCommand(ctx context.Context, output io.Writer, extra ...string) error {
	root := &cli.Command{Name: "shipmates", Writer: output, ErrWriter: output, Commands: []*cli.Command{Sail()}}
	args := append([]string{"shipmates", "sail", "--no-color", "--max-concurrent", "3"}, extra...)
	return root.Run(ctx, args)
}

func TestSailRefusesNonClaudeRuntime(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "t", Objective: "o", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}})
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".shipmates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".shipmates", "config.yaml"), []byte("runtime: codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runSailCommand(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot drive codex") || !strings.Contains(err.Error(), "Claude Code's CLI") {
		t.Fatalf("non-claude runtime error = %v", err)
	}
}

func TestSailRefusesRecursiveManagedSession(t *testing.T) {
	isolateSailEnvironment(t)
	t.Setenv(codexapp.ManagedSessionEnvironment, "1")
	cmd := Sail()
	cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
	err := cmd.Run(context.Background(), []string{"sail"})
	if err == nil || !strings.Contains(err.Error(), "managed Shipmates crew session") {
		t.Fatalf("recursive sail error = %v", err)
	}
}

// The load-bearing structural rule: the first mate writes the plan, only the
// admiral commissions it. An uncommissioned voyage refuses to sail, and the
// commission command refuses to run from inside any agent turn, so nothing a
// persona can do through Shipmates flips the commissioned state.
func TestSailRefusesUncommissionedVoyageAndCommissionIsAdmiralOnly(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "t", Objective: "o", Commissioned: false, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	err := runSailCommand(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not commissioned") || !strings.Contains(err.Error(), "shipmates commission") {
		t.Fatalf("uncommissioned sail error = %v", err)
	}
	runCommissionCommand := func() (string, error) {
		var out bytes.Buffer
		cmd := &cli.Command{Name: "shipmates", Writer: &out, ErrWriter: &out, Commands: []*cli.Command{Commission()}}
		err := cmd.Run(context.Background(), []string{"shipmates", "commission"})
		return out.String(), err
	}
	// A crew turn (sail-managed session) cannot commission.
	t.Setenv(codexapp.ManagedSessionEnvironment, "1")
	if _, err := runCommissionCommand(); err == nil || !strings.Contains(err.Error(), "admiral act") {
		t.Fatalf("managed-session commission error = %v", err)
	}
	t.Setenv(codexapp.ManagedSessionEnvironment, "")
	// Any Claude Code tool subprocess (every persona turn) cannot commission.
	t.Setenv("CLAUDECODE", "1")
	if _, err := runCommissionCommand(); err == nil || !strings.Contains(err.Error(), "admiral act") {
		t.Fatalf("agent-turn commission error = %v", err)
	}
	if p, _, err := voyage.LoadDraft(filepath.Join(root, ".shipmates", "voyage.json")); err != nil || p.Commissioned {
		t.Fatalf("refused commission still flipped the flag: %+v err=%v", p, err)
	}
	t.Setenv("CLAUDECODE", "")
	// The admiral, at their own terminal, commissions; then the voyage sails.
	out, err := runCommissionCommand()
	if err != nil || !strings.Contains(out, "commissioned") || !strings.Contains(out, "shipmates sail") {
		t.Fatalf("commission = %v\n%s", err, out)
	}
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	sailTaskDispatcher = func(_ context.Context, _, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		_, _ = fmt.Fprintln(stdout, "done")
		return nil
	}
	if err := runSailCommand(context.Background(), io.Discard); err != nil {
		t.Fatalf("commissioned voyage refused to sail: %v", err)
	}
}

func TestSailRefusesUninstalledPersona(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "t", Objective: "o", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "quartermaster", Summary: "work", Prompt: "work"}}})
	t.Chdir(root)
	err := runSailCommand(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `persona "quartermaster" is not installed`) || !strings.Contains(err.Error(), "shipmates add quartermaster") {
		t.Fatalf("uninstalled persona error = %v", err)
	}
}

func TestSailModelLadderIsRequiredOnlyWhenTasksEscalate(t *testing.T) {
	for name, models := range map[string][]string{"unknown": {"missing"}, "descending": {"large", "small"}} {
		t.Run(name, func(t *testing.T) {
			root := sailTestProject(t, voyage.Plan{Version: 1, Title: "test", Objective: "validate", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work", Models: models, RetrySafe: len(models) > 1}}})
			t.Chdir(root)
			if err := runSailCommand(context.Background(), io.Discard); err == nil {
				t.Fatal("invalid model ladder accepted")
			}
		})
	}
	t.Run("no ladder needed without model overrides", func(t *testing.T) {
		plan := voyage.Plan{Version: 1, Title: "test", Objective: "validate", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}}
		root := sailTestProject(t, plan)
		t.Chdir(root)
		// Remove the modelLadder entirely: persona-config dispatch needs none.
		if err := os.WriteFile(filepath.Join(root, "shipmates.yaml"), []byte("sessionPrefix: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runSailCommand(context.Background(), io.Discard, "--dry-run"); err != nil {
			t.Fatalf("ladderless plan refused: %v", err)
		}
	})
	t.Run("escalating tasks demand a ladder", func(t *testing.T) {
		plan := voyage.Plan{Version: 1, Title: "test", Objective: "validate", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work", Models: []string{"small", "large"}, RetrySafe: true}}}
		root := sailTestProject(t, plan)
		t.Chdir(root)
		if err := os.WriteFile(filepath.Join(root, "shipmates.yaml"), []byte("sessionPrefix: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runSailCommand(context.Background(), io.Discard, "--dry-run")
		if err == nil || !strings.Contains(err.Error(), "modelLadder") {
			t.Fatalf("escalating plan without ladder = %v", err)
		}
	})
}

func TestSailExecutesDAGSerializesPersonaAndEscalates(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "test", Objective: "verify scheduler", Commissioned: true, Tasks: []voyage.Task{
		{ID: "backend-one", Persona: "backend", Summary: "one", Prompt: "one", Models: []string{"small", "large"}, Efforts: []string{"low", "high"}, RetrySafe: true},
		{ID: "backend-two", Persona: "backend", Summary: "two", Prompt: "two"},
		{ID: "verify", Persona: "tester", Summary: "verify", Prompt: "verify", DependsOn: []string{"backend-one", "backend-two"}},
	}})
	t.Chdir(root)

	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	var mu sync.Mutex
	active := map[string]int{}
	calls := map[string][]string{}
	sailTaskDispatcher = func(ctx context.Context, persona, prompt string, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
		mu.Lock()
		active[persona]++
		if active[persona] > 1 {
			mu.Unlock()
			return errors.New("same persona dispatched concurrently")
		}
		calls[persona] = append(calls[persona], cfg.Model+"/"+cfg.Effort)
		mu.Unlock()
		defer func() { mu.Lock(); active[persona]--; mu.Unlock() }()
		time.Sleep(10 * time.Millisecond)
		if persona == "backend" && strings.Contains(prompt, "bounded job: one") && cfg.Model == "small" {
			return errors.New("small tier insufficient")
		}
		_, _ = fmt.Fprintln(stdout, "done")
		return nil
	}

	var output bytes.Buffer
	if err := runSailCommand(context.Background(), &output); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if got := strings.Join(calls["backend"], ","); !strings.Contains(got, "small/low,small/high,large/low") {
		t.Fatalf("backend escalation calls = %q", got)
	}
	if !strings.Contains(output.String(), "DONE 3") || !strings.Contains(output.String(), "escalating") || !strings.Contains(output.String(), "MODEL small") || !strings.Contains(output.String(), "EFFORT low") {
		t.Fatalf("output = %s", output.String())
	}
}

func TestSailEscalatesEffortBeforeModelCapability(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "tiers", Objective: "tiers", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}})
	t.Chdir(root)
	task := voyage.Task{Persona: "backend", Models: []string{"small", "large", "largest"}, Efforts: []string{"medium", "high"}, RetrySafe: true}
	var got []string
	for attempt := 0; attempt < task.TierCount(); attempt++ {
		cfg, err := sailTaskConfig(task, attempt)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, cfg.Model+"/"+cfg.Effort)
	}
	want := []string{"small/medium", "small/high", "large/medium", "large/high", "largest/medium", "largest/high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tiers = %v, want %v", got, want)
	}
}

// A task without explicit ladders dispatches with the persona's own
// configured model/effort — including the empty "claude default".
func TestSailTaskConfigWithoutLaddersUsesPersonaConfig(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "t", Objective: "o", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}})
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, ".claude", "agents", "backend.md"), []byte("---\nmodel: claude-opus-4-7\neffort: high\n---\n\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := sailTaskConfig(voyage.Task{Persona: "backend"}, 0)
	if err != nil || cfg.Model != "claude-opus-4-7" || cfg.Effort != "high" {
		t.Fatalf("cfg = %+v err=%v", cfg, err)
	}
}

// sailClaudeArgs is the exact argv contract for a crew turn: claude -p with
// the session identity, the tier's model/effort flags, then the prompt.
func TestSailClaudeArgsCarriesTierFlags(t *testing.T) {
	cfg := project.PersonaConfig{Model: "claude-opus-4-7", Effort: "high", Mode: "acceptEdits"}
	got := sailClaudeArgs(cfg, []string{"--resume", "uuid-1", "--agent", "backend"}, "do the work")
	want := []string{"-p", "--resume", "uuid-1", "--agent", "backend", "--permission-mode", "acceptEdits", "--model", "claude-opus-4-7", "--effort", "high", "do the work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestSailInfrastructureFailureClassification(t *testing.T) {
	launch := &sailInfrastructureFailure{err: &exec.Error{Name: "claude", Err: exec.ErrNotFound}}
	if !isSailInfrastructureFailure(launch) {
		t.Fatal("launch failure was not classified as infrastructure")
	}
	if !persistedInfrastructureFailure(launch.Error()) {
		t.Fatal("persisted launch failure will not reset its model attempt")
	}
	if persistedInfrastructureFailure("crew task returned an incorrect result") {
		t.Fatal("task failure was classified as infrastructure")
	}
	if isSailInfrastructureFailure(errors.New("exit status 1")) {
		t.Fatal("crew exit status was classified as infrastructure")
	}
}

func TestSafeVoyagePlanPathRejectsOutsideAndSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	inside := filepath.Join(root, "plan.json")
	if err := os.WriteFile(inside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := safeVoyagePlanPath(inside); err != nil || got != inside {
		t.Fatalf("inside path: got %q err %v", got, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := safeVoyagePlanPath(outside); err == nil {
		t.Fatal("outside path accepted")
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(inside, link); err == nil {
		if _, err := safeVoyagePlanPath(link); err == nil {
			t.Fatal("symlink plan accepted")
		}
	}
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "plan.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(root, "outside")
	if err := os.Symlink(outsideDir, ancestor); err == nil {
		if _, err := safeVoyagePlanPath(filepath.Join(ancestor, "plan.json")); err == nil {
			t.Fatal("symlinked ancestor escape accepted")
		}
	}
}

func TestSailCancellationReturnsTaskToPending(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "cancel", Objective: "cancel safely", Commissioned: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	started := make(chan struct{})
	sailTaskDispatcher = func(ctx context.Context, _, _ string, _ project.PersonaConfig, _, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runSailCommand(ctx, io.Discard) }()
	<-started
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled sail succeeded")
	}
	b, _ := json.Marshal(plan)
	statePath := filepath.Join(root, ".shipmates", "voyages", voyage.Hash(b)[:16]+".json")
	state, err := voyage.LoadState(statePath, &plan, voyage.Hash(b))
	if err != nil {
		t.Fatal(err)
	}
	if state.Tasks["work"].Status != voyage.Pending {
		t.Fatalf("canceled state = %s", state.Tasks["work"].Status)
	}
}

func TestSailPersistsEachCompletionAndRejectsConcurrentProcess(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "persist", Objective: "persist immediately", Commissioned: true, Tasks: []voyage.Task{
		{ID: "fast", Persona: "backend", Summary: "fast", Prompt: "fast"},
		{ID: "slow", Persona: "tester", Summary: "slow", Prompt: "slow"},
	}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	sailTaskDispatcher = func(ctx context.Context, persona, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		if persona == "tester" {
			close(slowStarted)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseSlow:
			}
		}
		_, _ = fmt.Fprintln(stdout, "done")
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runSailCommand(context.Background(), io.Discard) }()
	<-slowStarted

	b, _ := json.Marshal(plan)
	hash := voyage.Hash(b)
	statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := voyage.LoadState(statePath, &plan, hash)
		if err == nil && state.Tasks["fast"].Status == voyage.Completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fast completion was not persisted while slow task ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := runSailCommand(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "already sailing") {
		t.Fatalf("concurrent sail error = %v", err)
	}
	close(releaseSlow)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestSailMarkdownTrackerMirrorsVoyage is the default no-dependencies
// experience end to end: sail on a machine without a Beads workspace mirrors
// the voyage into .shipmates/voyage/*.md, injects the task record into crew
// prompts, and closes the records as tasks complete.
func TestSailMarkdownTrackerMirrorsVoyage(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "markdown voyage", Objective: "prove the file backend", Commissioned: true, Tasks: []voyage.Task{
		{ID: "design", Persona: "backend", Summary: "Design", Prompt: "produce the design"},
		{ID: "verify", Persona: "tester", Summary: "Verify", Prompt: "verify the design", DependsOn: []string{"design"}},
	}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	var mu sync.Mutex
	prompts := map[string]string{}
	sailTaskDispatcher = func(_ context.Context, persona, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		mu.Lock()
		prompts[persona] = prompt
		mu.Unlock()
		_, _ = fmt.Fprintf(stdout, "%s finished the job\n", persona)
		return nil
	}
	var output bytes.Buffer
	if err := runSailCommand(context.Background(), &output); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "TRACKER  markdown") {
		t.Fatalf("markdown backend was not announced:\n%s", output.String())
	}

	b, _ := json.Marshal(plan)
	hash := voyage.Hash(b)
	state, err := voyage.LoadState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), &plan, hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"design", "verify"} {
		recordID := state.Tasks[id].BeadID
		if recordID == "" {
			t.Fatalf("task %s has no tracker record id", id)
		}
		raw, err := os.ReadFile(filepath.Join(root, ".shipmates", "voyage", recordID+".md"))
		if err != nil {
			t.Fatal(err)
		}
		record := string(raw)
		for _, want := range []string{"status: closed", "close_reason: Shipmates voyage task completed", "finished the job"} {
			if !strings.Contains(record, want) {
				t.Fatalf("task record %s missing %q:\n%s", recordID, want, record)
			}
		}
	}
	verifyRecord, _ := os.ReadFile(filepath.Join(root, ".shipmates", "voyage", state.Tasks["verify"].BeadID+".md"))
	if !strings.Contains(string(verifyRecord), "depends_on:") || !strings.Contains(string(verifyRecord), state.Tasks["design"].BeadID) {
		t.Fatalf("voyage DAG edge not mirrored:\n%s", verifyRecord)
	}
	mu.Lock()
	testerPrompt := prompts["tester"]
	mu.Unlock()
	for _, want := range []string{"VOYAGE TASK TRACKER", "Task record: .shipmates/voyage/" + state.Tasks["verify"].BeadID + ".md", "Current task record:", "status: open"} {
		if !strings.Contains(testerPrompt, want) {
			t.Fatalf("tester prompt missing %q:\n%s", want, testerPrompt)
		}
	}
}

func TestSailMarkdownTrackerBlocksFailedAndDependentTasks(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "blocked voyage", Objective: "prove terminal sync", Commissioned: true, Tasks: []voyage.Task{
		{ID: "design", Persona: "backend", Summary: "Design", Prompt: "produce the design"},
		{ID: "verify", Persona: "tester", Summary: "Verify", Prompt: "verify the design", DependsOn: []string{"design"}},
	}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	sailTaskDispatcher = func(_ context.Context, persona, _ string, _ project.PersonaConfig, _, _ io.Writer) error {
		if persona == "backend" {
			return errors.New("the design could not be produced")
		}
		return errors.New("tester should never have been dispatched")
	}
	err := runSailCommand(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "voyage incomplete") {
		t.Fatalf("failing voyage error = %v", err)
	}
	b, _ := json.Marshal(plan)
	hash := voyage.Hash(b)
	state, err := voyage.LoadState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), &plan, hash)
	if err != nil {
		t.Fatal(err)
	}
	for id, wantNote := range map[string]string{"design": "the design could not be produced", "verify": "dependency design did not complete"} {
		raw, err := os.ReadFile(filepath.Join(root, ".shipmates", "voyage", state.Tasks[id].BeadID+".md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "status: blocked") || !strings.Contains(string(raw), wantNote) {
			t.Fatalf("task record %s not blocked with reason %q:\n%s", id, wantNote, raw)
		}
	}
}

func TestPublicSailAmendmentInheritsTaskOneAndDispatchesChangedSuccessor(t *testing.T) {
	pre := voyage.Plan{Version: 1, Title: "Fleet-shaped amendment", Objective: "resume safely", Scope: []string{"observation", "control"}, NonGoals: []string{"remote start"}, BlastArea: []string{"Fleet"}, Risks: []string{"stale target"}, AcceptanceCriteria: []string{"Task 1 is preserved"}, OpenDecisions: []string{"Linux"}, Commissioned: true, Tasks: []voyage.Task{
		{ID: "zero-to-observed", Persona: "backend", Summary: "Observe", Prompt: "observe one active turn"},
		{ID: "exact-turn-control", Persona: "tester", Summary: "Control", Prompt: "amended control contract", DependsOn: []string{"zero-to-observed"}},
	}}
	successor := pre
	successor.Tasks = append([]voyage.Task(nil), pre.Tasks...)
	successor.Tasks[1].Prompt = "changed control contract"
	root := sailTestProject(t, successor)
	t.Chdir(root)
	prePlanPath := filepath.Join(root, "original-voyage.json")
	preHash, _ := writeCommandVoyagePlan(t, prePlanPath, pre)
	preStatePath := filepath.Join(root, "original-voyage-state.json")
	preState := voyage.NewState(&pre, preHash)
	entry := preState.Tasks["zero-to-observed"]
	entry.Status, entry.Summary, entry.FinishedAt, entry.BeadID = voyage.Completed, "original completion evidence", time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC), "Shipmates-auz"
	preState.Tasks["zero-to-observed"] = entry
	if err := voyage.SaveState(preStatePath, preState); err != nil {
		t.Fatal(err)
	}
	preBytes := mustCommandRead(t, preStatePath)

	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	var dispatched []string
	sailTaskDispatcher = func(_ context.Context, persona, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		dispatched = append(dispatched, persona)
		_, _ = fmt.Fprintln(stdout, "changed task completed")
		return nil
	}
	var output bytes.Buffer
	rootCommand := &cli.Command{Name: "shipmates", Writer: &output, ErrWriter: &output, Commands: []*cli.Command{Sail()}}
	if err := rootCommand.Run(context.Background(), []string{"shipmates", "sail", "--no-color", "--predecessor-plan", prePlanPath, "--predecessor-state", preStatePath}); err != nil {
		t.Fatalf("%v\n%s", err, output.String())
	}
	if !reflect.DeepEqual(dispatched, []string{"tester"}) {
		t.Fatalf("dispatched personas = %v", dispatched)
	}
	if !strings.Contains(output.String(), "INHERITED") || !strings.Contains(output.String(), "Observe") {
		t.Fatalf("inherited status was not rendered: %s", output.String())
	}
	_, canonical, err := voyage.Load(filepath.Join(root, ".shipmates", "voyage.json"))
	if err != nil {
		t.Fatal(err)
	}
	successorHash := voyage.Hash(canonical)
	successorState, err := voyage.LoadState(filepath.Join(root, ".shipmates", "voyages", successorHash[:16]+".json"), &successor, successorHash)
	if err != nil {
		t.Fatal(err)
	}
	if successorState.Tasks["zero-to-observed"].Inherited == nil || successorState.Tasks["zero-to-observed"].BeadID != "Shipmates-auz" {
		t.Fatalf("Task 1 inheritance = %+v", successorState.Tasks["zero-to-observed"])
	}
	if successorState.Tasks["exact-turn-control"].Status != voyage.Completed {
		t.Fatalf("Task 2 state = %+v", successorState.Tasks["exact-turn-control"])
	}
	if after := mustCommandRead(t, preStatePath); !bytes.Equal(preBytes, after) {
		t.Fatal("public amendment changed predecessor bytes")
	}
	// The inherited predecessor record was never recreated in this workspace's
	// markdown tracker; the changed successor task was.
	if _, err := os.Stat(filepath.Join(root, ".shipmates", "voyage", successorHash[:16]+"-zero-to-observed.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited task record was recreated: %v", err)
	}
	if successorState.Tasks["exact-turn-control"].BeadID == "" {
		t.Fatal("changed successor task has no tracker record")
	}
}

func TestSailDisplayUsesStablePersonaColorsAndPlainFallback(t *testing.T) {
	var colored bytes.Buffer
	display := sailDisplay{w: &colored, color: true}
	backend := display.persona("backend")
	tester := display.persona("tester")
	if !strings.Contains(backend, "\x1b[") || !strings.Contains(tester, "\x1b[") {
		t.Fatal("colored personas are missing ANSI color")
	}
	if backend == tester {
		t.Fatal("backend and tester should have distinct stable colors")
	}
	colors := map[int]bool{}
	for _, persona := range []string{"captain", "first-mate", "architect", "backend", "frontend", "security", "tester"} {
		color := personaColor(persona)
		if colors[color] {
			t.Fatalf("shipped persona %q shares color %d", persona, color)
		}
		colors[color] = true
	}
	if got := (sailDisplay{w: &colored}).persona("backend"); got != "backend" {
		t.Fatalf("plain persona = %q", got)
	}
}

func TestSailCancellationFeedbackIsImmediateAndExplicit(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.Canceling()
	got := out.String()
	if !strings.Contains(got, "Admiral interrupt received") || !strings.Contains(got, "resumable voyage state") {
		t.Fatalf("cancellation feedback = %q", got)
	}
}

func TestSailHeaderAdvertisesCancellationControl(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.Header(&voyage.Plan{Title: "test"}, "abc123", false)
	got := out.String()
	if !strings.Contains(got, "CONTROL") || !strings.Contains(got, "Ctrl+C") || !strings.Contains(got, "preserves resumable state") {
		t.Fatalf("header controls = %q", got)
	}
}

func TestSailLineageDisplayIsBoundedAndPlain(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.Lineage(&voyage.State{Lineage: &voyage.Lineage{PredecessorPlanHash: strings.Repeat("b", 64)}}, strings.Repeat("c", 64))
	got := out.String()
	for _, want := range []string{"successor=cccccccccccc", "predecessor=bbbbbbbbbbbb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lineage display missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("lineage display emitted color: %q", got)
	}
}

func TestVerboseSailShowsBriefAndReport(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.verbose = true
	task := voyage.Task{ID: "review", Persona: "security", Summary: "review"}
	cfg := project.PersonaConfig{Model: "claude-opus-4-7", Effort: "medium"}
	display.Started(task, cfg)
	display.Brief(task, cfg, "Inspect the authentication flow.")
	display.Completed(task, "No credential exposure found.")
	got := out.String()
	for _, want := range []string{"MODEL claude-opus-4-7", "EFFORT medium", "[claude-opus-4-7 | medium]", "TASK BRIEF", "Inspect the authentication flow.", "REPORT", "No credential exposure found."} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q: %s", want, got)
		}
	}
	if strings.Count(got, "No credential exposure found.") != 1 {
		t.Fatalf("agent report was duplicated: %s", got)
	}
}

func TestSailTaskPromptCarriesCommissionedScope(t *testing.T) {
	p := &voyage.Plan{Title: "Release", Objective: "Ship verified work"}
	task := voyage.Task{Summary: "Run tests", Prompt: "Run the full suite."}
	got := sailTaskPrompt(p, task)
	for _, want := range []string{"Release", "Ship verified work", "Run tests", "Run the full suite", "Do not broaden"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestSailInputRequestIsExplicitAndBounded(t *testing.T) {
	if _, ok := sailInputRequest("ordinary result"); ok {
		t.Fatal("ordinary result became captain input")
	}
	question, ok := sailInputRequest("SHIPMATES_NEEDS_INPUT: Choose blue or green")
	if !ok || question != "Choose blue or green" {
		t.Fatalf("question=%q ok=%v", question, ok)
	}
}

func writeCommandVoyagePlan(t *testing.T, path string, plan voyage.Plan) (string, []byte) {
	t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return voyage.Hash(b), b
}

func mustCommandRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
