package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/recovery"
	"github.com/luthermonson/shipmates/internal/voyage"
	"github.com/urfave/cli/v3"
)

func TestSailRefusesRecursiveManagedSession(t *testing.T) {
	t.Setenv(codexapp.ManagedSessionEnvironment, "1")
	cmd := Sail()
	cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
	err := cmd.Run(context.Background(), []string{"sail"})
	if err == nil || !strings.Contains(err.Error(), "Captain must use /sail") {
		t.Fatalf("recursive sail error = %v", err)
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
	for _, persona := range []string{"skipper", "quartermaster", "architect", "backend", "frontend", "security", "tester"} {
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
	if !strings.Contains(got, "Captain interrupt received") || !strings.Contains(got, "resumable voyage state") {
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

func TestSailRecoveryDisplayIsBoundedPlainAndInert(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.RecoveryConfig(true)
	display.Lineage(&voyage.State{Lineage: &voyage.Lineage{PredecessorPlanHash: strings.Repeat("b", 64)}}, strings.Repeat("c", 64))
	task := voyage.Task{ID: "recover", Persona: "backend", Summary: "recover safely"}
	display.Recovery(task, 1, recovery.ResponseV1{Decision: recovery.DecisionAmendmentRequired, Reason: recovery.ReasonCaptainDecisionRequired, Fingerprint: strings.Repeat("a", 64)}, "captain_decision_required", "sensitive raw detail must not be rendered", "rejected: amendment inert; Captain approval required")
	got := out.String()
	for _, want := range []string{"AUTO-CAPTAIN STATE  enabled", "Sol=gpt-5.6-sol", "derivatives=bounded-envelope-only", "successor=cccccccccccc", "predecessor=bbbbbbbbbbbb", "model=gpt-5.6-sol", "enabled=yes", "blocker=captain_decision_required", "attempts=2", "fingerprint=aaaaaaaaaaaa", "recommendation=amendment_required", "rejected: amendment inert", "safe-options=retry/resume-or-stop", "approval=required for amendment"} {
		if !strings.Contains(got, want) {
			t.Fatalf("recovery display missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "sensitive raw detail") || strings.Contains(got, "\x1b[") {
		t.Fatalf("recovery display leaked detail/color: %q", got)
	}
}

func TestVerboseSailShowsBriefCommandsAndAgentReport(t *testing.T) {
	var out bytes.Buffer
	display := newSailDisplay(&out, false)
	display.verbose = true
	task := voyage.Task{ID: "review", Persona: "security", Summary: "review"}
	cfg := project.PersonaConfig{Model: "gpt-5.6-luna", Effort: "medium"}
	display.Started(task, cfg)
	display.Brief(task, cfg, "Inspect the authentication flow.")
	display.Detail(task, cfg, "command", "git diff -- internal/auth.go")
	display.Agent(task, cfg, "No credential exposure found.")
	display.Completed(task, "No credential exposure found.")
	got := out.String()
	for _, want := range []string{"MODEL gpt-5.6-luna", "EFFORT medium", "[gpt-5.6-luna | medium]", "TASK BRIEF", "Inspect the authentication flow.", "COMMAND", "git diff -- internal/auth.go", "REPORT", "No credential exposure found."} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q: %s", want, got)
		}
	}
	if strings.Count(got, "No credential exposure found.") != 1 {
		t.Fatalf("agent report was duplicated: %s", got)
	}
}

func TestManagedInfrastructureFailuresDoNotConsumeEscalation(t *testing.T) {
	err := &managedSessionFailure{state: livesession.Failed, code: "malformed_frame"}
	if !isManagedSessionFailure(err) {
		t.Fatal("managed session failure was not classified as infrastructure")
	}
	if !persistedInfrastructureFailure(err.Error()) {
		t.Fatal("persisted managed session failure will not reset its model attempt")
	}
	if persistedInfrastructureFailure("codex task returned an incorrect result") {
		t.Fatal("task failure was classified as infrastructure")
	}
}

func TestSafeVoyagePlanPathRejectsOutsideAndSymlink(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir(".git", 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestSailRejectsUnknownAndDescendingModelLadders(t *testing.T) {
	for name, models := range map[string][]string{"unknown": {"missing"}, "descending": {"large", "small"}} {
		t.Run(name, func(t *testing.T) {
			root := sailTestProject(t, voyage.Plan{Version: 1, Title: "test", Objective: "validate", Approved: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work", Models: models, RetrySafe: len(models) > 1}}})
			t.Chdir(root)
			if err := runSailCommand(context.Background(), io.Discard); err == nil {
				t.Fatal("invalid model ladder accepted")
			}
		})
	}
}

func TestSailExecutesDAGSerializesPersonaAndEscalates(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "test", Objective: "verify scheduler", Approved: true, Tasks: []voyage.Task{
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
	sailTaskDispatcher = func(ctx context.Context, persona *project.InstalledPersona, prompt string, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
		mu.Lock()
		active[persona.Name]++
		if active[persona.Name] > 1 {
			mu.Unlock()
			return errors.New("same persona dispatched concurrently")
		}
		calls[persona.Name] = append(calls[persona.Name], cfg.Model+"/"+cfg.Effort)
		mu.Unlock()
		defer func() { mu.Lock(); active[persona.Name]--; mu.Unlock() }()
		time.Sleep(10 * time.Millisecond)
		if persona.Name == "backend" && strings.Contains(prompt, "bounded job: one") && cfg.Model == "small" {
			return errors.New("small tier insufficient")
		}
		_, _ = fmt.Fprintln(stdout, "done")
		return nil
	}

	var output bytes.Buffer
	if err := runSailCommand(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls["backend"], ","); !strings.Contains(got, "small/low,small/high,large/low") {
		t.Fatalf("backend escalation calls = %q", got)
	}
	if !strings.Contains(output.String(), "DONE 3") || !strings.Contains(output.String(), "escalating") || !strings.Contains(output.String(), "MODEL small") || !strings.Contains(output.String(), "EFFORT low") {
		t.Fatalf("output = %s", output.String())
	}
}

func TestSailEscalatesEffortBeforeModelCapability(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "tiers", Objective: "tiers", Approved: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}})
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

func TestVoyageSidebarShowsCompletedObjectiveAndAcceptance(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "done", Objective: "Deliver the report", AcceptanceCriteria: []string{"Report is fact-checked"}, Approved: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	canonical, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	hash := voyage.Hash(canonical)
	state := voyage.NewState(&plan, hash)
	entry := state.Tasks["work"]
	entry.Status = voyage.Completed
	state.Tasks["work"] = entry
	statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")
	if err := voyage.SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	sidebar := voyageSidebar(filepath.Join(root, ".shipmates", "voyage.json"))
	text := sidebar.Status
	for _, section := range sidebar.Sections {
		text += " " + strings.Join(section.Items, " ")
	}
	for _, want := range []string{"COMPLETED - acceptance unknown", "[completed] work"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sidebar missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"PASS - Deliver the report", "PASS - Report is fact-checked"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("legacy sidebar unexpectedly contains %q: %s", forbidden, text)
		}
	}
}

func TestVoyageSidebarShowsExplicitAcceptancePass(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "pass", Objective: "Deliver the report", AcceptanceCriteria: []string{"Report is fact-checked"}, AcceptanceGateTask: "gate", Approved: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	canonical, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	hash := voyage.Hash(canonical)
	state := voyage.NewState(&plan, hash)
	entry := state.Tasks["gate"]
	entry.Status = voyage.Completed
	state.Tasks["gate"] = entry
	state.Acceptance = &voyage.AcceptanceVerdict{Status: voyage.AcceptancePass, RecordedAt: time.Now().UTC(), EvidenceRefs: []string{"evidence:gate"}}
	if err := voyage.SaveState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), state); err != nil {
		t.Fatal(err)
	}
	sidebar := voyageSidebar(filepath.Join(root, ".shipmates", "voyage.json"))
	if !strings.Contains(sidebar.Status, "COMPLETED - acceptance criteria passed") {
		t.Fatalf("status = %q", sidebar.Status)
	}
	var sectionText []string
	for _, section := range sidebar.Sections {
		sectionText = append(sectionText, section.Items...)
	}
	text := strings.Join(sectionText, " ")
	if !strings.Contains(text, "PASS - Deliver the report") || !strings.Contains(text, "PASS - Report is fact-checked") {
		t.Fatalf("pass projection missing: %s", text)
	}
}

func TestVoyageSidebarCompletedM3NoGoFixture(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "M3 completion", Objective: "Prove one bounded Commander lifecycle", AcceptanceCriteria: []string{"M7 remains healthy", "Commander lifecycle is verified"}, AcceptanceGateTask: "gate", Approved: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "record final M3 verdict", Prompt: "record the completed M3 NO-GO report"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	canonical, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	hash := voyage.Hash(canonical)
	state := voyage.NewState(&plan, hash)
	entry := state.Tasks["gate"]
	entry.Status = voyage.Completed
	state.Tasks["gate"] = entry
	state.Acceptance = &voyage.AcceptanceVerdict{Status: voyage.AcceptanceNoGo, RecordedAt: time.Now().UTC(), EvidenceRefs: []string{"report:fleet-beta/m3-no-go"}}
	if err := voyage.SaveState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), state); err != nil {
		t.Fatal(err)
	}
	sidebar := voyageSidebar(filepath.Join(root, ".shipmates", "voyage.json"))
	if !strings.Contains(sidebar.Status, "COMPLETED - acceptance failed") {
		t.Fatalf("status = %q", sidebar.Status)
	}
	for _, section := range sidebar.Sections {
		for _, item := range section.Items {
			if strings.Contains(item, "PASS -") {
				t.Fatalf("no-go sidebar passed: %q", item)
			}
		}
	}
}

func TestPlanTUIAcceptanceVerdictIntegrationIsTruthful(t *testing.T) {
	tests := []struct {
		name    string
		verdict *voyage.AcceptanceVerdict
		legacy  bool
		want    []string
		forbid  []string
	}{
		{name: "no-go", verdict: &voyage.AcceptanceVerdict{Status: voyage.AcceptanceNoGo, RecordedAt: time.Unix(10, 0).UTC(), EvidenceRefs: []string{"host:[31mNO-GO[0m"}}, want: []string{"NO-GO -", "COMPLETED - acceptance failed"}, forbid: []string{"COMPLETED - acceptance criteria passed", "PASS -"}},
		{name: "pass", verdict: &voyage.AcceptanceVerdict{Status: voyage.AcceptancePass, RecordedAt: time.Unix(10, 0).UTC(), EvidenceRefs: []string{"report:pass"}}, want: []string{"PASS -", "COMPLETED - acceptance criteria passed"}},
		{name: "unset", verdict: nil, want: []string{"COMPLETED - acceptance unknown"}, forbid: []string{"PASS -", "NO-GO -"}},
		{name: "legacy", legacy: true, want: []string{"COMPLETED - acceptance unknown"}, forbid: []string{"PASS -", "NO-GO -"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := voyage.Plan{Version: 1, Title: "acceptance fixture", Objective: "Verify bounded completion", AcceptanceCriteria: []string{"Criteria " + strings.Repeat("x", 180)}, AcceptanceGateTask: "gate", Approved: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "final gate", Prompt: "record verdict"}}}
			root := sailTestProject(t, plan)
			t.Chdir(root)
			canonical, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			hash := voyage.Hash(canonical)
			state := voyage.NewState(&plan, hash)
			entry := state.Tasks["gate"]
			entry.Status = voyage.Completed
			state.Tasks["gate"] = entry
			if tt.verdict != nil {
				state.Acceptance = tt.verdict
			}
			statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")
			if tt.legacy {
				if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
					t.Fatal(err)
				}
				legacy := voyage.State{Version: 1, PlanHash: hash, Title: plan.Title, StartedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC(), Tasks: state.Tasks}
				if err := os.WriteFile(statePath, mustJSON(legacy), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := voyage.SaveState(statePath, state); err != nil {
				t.Fatal(err)
			}
			sidebar := voyageSidebar(filepath.Join(root, ".shipmates", "voyage.json"))
			model := &dashboard.Model{Persona: "skipper", State: livesession.Idle, Controller: true, Connected: true, Sidebar: sidebar}
			wide := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 80}).Lines, "\n")
			for _, want := range tt.want {
				if !strings.Contains(wide, want) {
					t.Fatalf("wide screen missing %q:\n%s", want, wide)
				}
			}
			for _, forbidden := range tt.forbid {
				if strings.Contains(wide, forbidden) {
					t.Fatalf("wide screen contains contradictory %q:\n%s", forbidden, wide)
				}
			}
			if strings.Contains(wide, "\x1b") {
				t.Fatal("semantic plain renderer emitted ANSI")
			}
			for _, line := range strings.Split(wide, "\n") {
				if len([]rune(line)) > 140 {
					t.Fatalf("wide line exceeds bound: %q", line)
				}
			}
			narrow := strings.Join(dashboard.Render(model, dashboard.Size{Width: 56, Height: 8}).Lines, "\n")
			if strings.Contains(narrow, "COMPLETED - acceptance criteria passed") || strings.Contains(narrow, "PASS -") {
				t.Fatalf("narrow fallback surfaced success: %s", narrow)
			}
			model.SidebarScroll = 1 << 30
			scrolled := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 12}).Lines, "\n")
			for _, line := range strings.Split(scrolled, "\n") {
				if len([]rune(line)) > 140 {
					t.Fatalf("scrolled line exceeds bound: %q", line)
				}
			}
		})
	}
}

func TestPlanRuntimeReloadRefreshesPersistedNoGoSidebar(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "reload", Objective: "Reload truthfully", AcceptanceCriteria: []string{"Hostile evidence \u001b[31mnever executes\u001b[0m"}, AcceptanceGateTask: "gate", Approved: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	planPath := filepath.Join(root, ".shipmates", "voyage.json")
	writeState := func(status voyage.AcceptanceStatus) {
		_, canonical, err := voyage.Load(planPath)
		if err != nil {
			t.Fatal(err)
		}
		hash := voyage.Hash(canonical)
		state := voyage.NewState(&plan, hash)
		entry := state.Tasks["gate"]
		entry.Status = voyage.Completed
		state.Tasks["gate"] = entry
		if status != voyage.AcceptanceUnset {
			state.Acceptance = &voyage.AcceptanceVerdict{Status: status, RecordedAt: time.Unix(5, 0).UTC(), EvidenceRefs: []string{"reload:bounded"}}
		}
		if err := voyage.SaveState(filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json"), state); err != nil {
			t.Fatal(err)
		}
	}
	writeState(voyage.AcceptancePass)
	model := &dashboard.Model{Persona: "skipper", State: livesession.Idle, Controller: true, Connected: true}
	refreshPlanSidebar(model, planPath)
	first := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 40}).Lines, "\n")
	if !strings.Contains(first, "acceptance criteria passed") {
		t.Fatalf("initial reload missing pass: %s", first)
	}
	writeState(voyage.AcceptanceNoGo)
	refreshPlanSidebar(model, planPath)
	reloaded := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 40}).Lines, "\n")
	if !strings.Contains(reloaded, "NO-GO -") || !strings.Contains(reloaded, "acceptance failed") || strings.Contains(reloaded, "acceptance criteria passed") || strings.Contains(reloaded, "PASS -") {
		t.Fatalf("reloaded no-go screen is stale or contradictory: %s", reloaded)
	}
	refreshPlanSidebar(model, planPath)
	if got := strings.Join(dashboard.Render(model, dashboard.Size{Width: 56, Height: 8}).Lines, "\n"); strings.Contains(got, "acceptance criteria passed") || strings.Contains(got, "PASS -") {
		t.Fatalf("narrow reload surfaced stale success: %s", got)
	}
}

func TestPlanRuntimeRejectsMismatchedOrMalformedAcceptanceAsUnknown(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "binding", Objective: "Bounded objective", AcceptanceCriteria: []string{"Bounded criterion"}, AcceptanceGateTask: "gate", Approved: true, Tasks: []voyage.Task{{ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	planPath := filepath.Join(root, ".shipmates", "voyage.json")
	loadedPlan, planBytes, err := voyage.Load(planPath)
	if err != nil {
		t.Fatal(err)
	}
	hash := voyage.Hash(planBytes)
	state := voyage.NewState(loadedPlan, hash)
	entry := state.Tasks["gate"]
	entry.Status = voyage.Completed
	state.Tasks["gate"] = entry
	state.Acceptance = &voyage.AcceptanceVerdict{Status: voyage.AcceptancePass, RecordedAt: time.Unix(8, 0).UTC(), EvidenceRefs: []string{"binding:pass"}}
	statePath := filepath.Join(root, ".shipmates", "voyages", hash[:16]+".json")
	if err := voyage.SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	model := &dashboard.Model{Persona: "skipper", State: livesession.Idle, Controller: true, Connected: true}
	refreshPlanSidebar(model, planPath)
	pass := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 30}).Lines, "\n")
	if !strings.Contains(pass, "acceptance criteria passed") {
		t.Fatalf("valid pass missing: %s", pass)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	badBinding := bytes.Replace(raw, []byte(state.GlobalFingerprint), []byte(strings.Repeat("f", 64)), 1)
	if bytes.Equal(raw, badBinding) {
		t.Fatal("test did not change binding")
	}
	if err := os.WriteFile(statePath, badBinding, 0o600); err != nil {
		t.Fatal(err)
	}
	refreshPlanSidebar(model, planPath)
	mismatch := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 30}).Lines, "\n")
	if !strings.Contains(mismatch, "acceptance unknown") || strings.Contains(mismatch, "PASS -") || strings.Contains(mismatch, "acceptance criteria passed") {
		t.Fatalf("mismatched binding was not unknown: %s", mismatch)
	}
	if err := os.WriteFile(statePath, []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshPlanSidebar(model, planPath)
	malformed := strings.Join(dashboard.Render(model, dashboard.Size{Width: 140, Height: 30}).Lines, "\n")
	if !strings.Contains(malformed, "acceptance unknown") || strings.Contains(malformed, "PASS -") {
		t.Fatalf("malformed state was not unknown: %s", malformed)
	}
}

func TestSailCancellationReturnsTaskToPending(t *testing.T) {
	plan := voyage.Plan{Version: 1, Title: "cancel", Objective: "cancel safely", Approved: true, Tasks: []voyage.Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	started := make(chan struct{})
	sailTaskDispatcher = func(ctx context.Context, _ *project.InstalledPersona, _ string, _ project.PersonaConfig, _, _ io.Writer) error {
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
	plan := voyage.Plan{Version: 1, Title: "persist", Objective: "persist immediately", Approved: true, Tasks: []voyage.Task{
		{ID: "fast", Persona: "backend", Summary: "fast", Prompt: "fast"},
		{ID: "slow", Persona: "tester", Summary: "slow", Prompt: "slow"},
	}}
	root := sailTestProject(t, plan)
	t.Chdir(root)
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	sailTaskDispatcher = func(ctx context.Context, persona *project.InstalledPersona, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		if persona.Name == "tester" {
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
	deadline := time.Now().Add(2 * time.Second)
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

func sailTestProject(t *testing.T, plan voyage.Plan) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{".git", ".codex/agents", ".shipmates/policies"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "shipmates.yaml"), []byte("sessionPrefix: test\nmodelLadder: [small, large]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".shipmates", "policy.yaml"), []byte("version: 1\nallow: []\nask: []\ndeny: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, persona := range []string{"backend", "tester"} {
		body := []byte("developer_instructions = \"test " + persona + "\"\n")
		if err := os.WriteFile(filepath.Join(root, ".codex", "agents", persona+".toml"), body, 0o600); err != nil {
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

func runSailCommand(ctx context.Context, output io.Writer) error {
	root := &cli.Command{Name: "shipmates", Writer: output, ErrWriter: output, Commands: []*cli.Command{Sail()}}
	return root.Run(ctx, []string{"shipmates", "sail", "--no-color", "--max-concurrent", "3"})
}

func TestSailTaskPromptCarriesApprovedScope(t *testing.T) {
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
		t.Fatal("ordinary result became Captain input")
	}
	question, ok := sailInputRequest("SHIPMATES_NEEDS_INPUT: Choose blue or green")
	if !ok || question != "Choose blue or green" {
		t.Fatalf("question=%q ok=%v", question, ok)
	}
}

func TestPublicSailAmendmentInheritsTaskOneAndDispatchesChangedSuccessor(t *testing.T) {
	t.Setenv(codexapp.ManagedSessionEnvironment, "")
	pre := voyage.Plan{Version: 1, Title: "Fleet-shaped amendment", Objective: "resume safely", Scope: []string{"observation", "control"}, NonGoals: []string{"remote start"}, BlastArea: []string{"Fleet"}, Risks: []string{"stale target"}, AcceptanceCriteria: []string{"Task 1 is preserved"}, OpenDecisions: []string{"Linux"}, Approved: true, Tasks: []voyage.Task{
		{ID: "zero-to-observed-real-codex", Persona: "backend", Summary: "Observe", Prompt: "observe one active turn"},
		{ID: "real-exact-turn-control", Persona: "tester", Summary: "Control", Prompt: "amended control contract", DependsOn: []string{"zero-to-observed-real-codex"}},
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
	entry := preState.Tasks["zero-to-observed-real-codex"]
	entry.Status, entry.Summary, entry.FinishedAt, entry.BeadID = voyage.Completed, "original completion evidence", time.Date(2026, 7, 14, 3, 0, 0, 0, time.UTC), "Shipmates-auz"
	preState.Tasks["zero-to-observed-real-codex"] = entry
	if err := voyage.SaveState(preStatePath, preState); err != nil {
		t.Fatal(err)
	}
	preBytes := mustCommandRead(t, preStatePath)

	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	var dispatched []string
	sailTaskDispatcher = func(_ context.Context, persona *project.InstalledPersona, _ string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		dispatched = append(dispatched, persona.Name)
		_, _ = fmt.Fprintln(stdout, "changed task completed")
		return nil
	}
	var output bytes.Buffer
	rootCommand := &cli.Command{Name: "shipmates", Writer: &output, ErrWriter: &output, Commands: []*cli.Command{Sail()}}
	if err := rootCommand.Run(context.Background(), []string{"shipmates", "sail", "--no-color", "--predecessor-plan", prePlanPath, "--predecessor-state", preStatePath}); err != nil {
		t.Fatal(err)
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
	if successorState.Tasks["zero-to-observed-real-codex"].Inherited == nil || successorState.Tasks["zero-to-observed-real-codex"].BeadID != "Shipmates-auz" {
		t.Fatalf("Task 1 inheritance = %+v", successorState.Tasks["zero-to-observed-real-codex"])
	}
	if successorState.Tasks["real-exact-turn-control"].Status != voyage.Completed {
		t.Fatalf("Task 2 state = %+v", successorState.Tasks["real-exact-turn-control"])
	}
	if after := mustCommandRead(t, preStatePath); !bytes.Equal(preBytes, after) {
		t.Fatal("public amendment changed predecessor bytes")
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
