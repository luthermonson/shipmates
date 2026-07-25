package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/turninput"
	"github.com/urfave/cli/v3"
)

// fakeRuntime is a hand-rolled runtime.Runtime for testing the ask dispatch
// path. It emits a scripted event sequence on SendTurn and records the
// calls made to it, so tests can assert both what got printed and what the
// command layer handed to the runtime.
type fakeRuntime struct {
	name   string
	caps   runtime.Caps
	events chan runtime.Event
	script []runtime.Event

	startCalls  []runtime.SessionSpec
	resumeCalls []string
	sentTurns   []runtime.TurnInput
	closed      bool
	// approvals records every ResolveApproval call so the ask path's
	// policy-driven answers can be asserted.
	approvals []runtime.ApprovalDecision

	sessionID string
}

func newFakeRuntime(script []runtime.Event) *fakeRuntime {
	return &fakeRuntime{
		name:      "claude",
		caps:      runtime.Caps{Streaming: true, Attachments: true},
		events:    make(chan runtime.Event, len(script)+1),
		script:    script,
		sessionID: "fake-session-id",
	}
}

func (f *fakeRuntime) Name() string               { return f.name }
func (f *fakeRuntime) Capabilities() runtime.Caps { return f.caps }
func (f *fakeRuntime) Events() <-chan runtime.Event               { return f.events }
func (f *fakeRuntime) CloseSession(context.Context, string) error { return nil }

func (f *fakeRuntime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	f.startCalls = append(f.startCalls, spec)
	return &fakeSession{id: f.sessionID, persona: spec.Persona, projectDir: spec.ProjectDir}, nil
}

func (f *fakeRuntime) ResumeSession(_ context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	f.resumeCalls = append(f.resumeCalls, id)
	return &fakeSession{id: id, persona: spec.Persona, projectDir: spec.ProjectDir}, nil
}

func (f *fakeRuntime) SendTurn(_ context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	f.sentTurns = append(f.sentTurns, in)
	go func() {
		for _, ev := range f.script {
			if ev.SessionID == "" {
				ev.SessionID = sessionID
			}
			f.events <- ev
		}
	}()
	return &fakeTurn{id: "fake-turn", sessionID: sessionID, startedAt: time.Now()}, nil
}

func (f *fakeRuntime) InterruptTurn(context.Context, string, string) error { return nil }
func (f *fakeRuntime) SteerTurn(context.Context, string, string, string) error {
	return &runtime.ErrUnsupported{Runtime: "fake", Feature: "SteerTurn"}
}
func (f *fakeRuntime) ResolveApproval(_ context.Context, _ runtime.ApprovalResponse, d runtime.ApprovalDecision) (bool, error) {
	f.approvals = append(f.approvals, d)
	return true, nil
}
func (f *fakeRuntime) InstallPersona(context.Context, string, runtime.PersonaSpec) error { return nil }
func (f *fakeRuntime) UninstallPersona(context.Context, string, string) error            { return nil }
func (f *fakeRuntime) InstallMemoryHook(context.Context, string) error                   { return nil }
func (f *fakeRuntime) Close(context.Context) error                                       { f.closed = true; return nil }

type fakeSession struct{ id, persona, projectDir string }

func (s *fakeSession) ID() string         { return s.id }
func (s *fakeSession) Persona() string    { return s.persona }
func (s *fakeSession) ProjectDir() string { return s.projectDir }

type fakeTurn struct {
	id, sessionID string
	startedAt     time.Time
}

func (t *fakeTurn) ID() string           { return t.id }
func (t *fakeTurn) SessionID() string    { return t.sessionID }
func (t *fakeTurn) StartedAt() time.Time { return t.startedAt }

// swapSelector installs a Selector that always returns rt.
func swapSelector(t *testing.T, rt runtime.Runtime) {
	t.Helper()
	prev := selector
	selector = runtime.SelectorFunc(func(context.Context, string, string) (runtime.Runtime, string, error) {
		return rt, "test", nil
	})
	t.Cleanup(func() { selector = prev })
}

// swapSelectorErr installs a Selector that always fails with err.
func swapSelectorErr(t *testing.T, err error) {
	t.Helper()
	prev := selector
	selector = runtime.SelectorFunc(func(context.Context, string, string) (runtime.Runtime, string, error) {
		return nil, "test", err
	})
	t.Cleanup(func() { selector = prev })
}

func turnScript(text string) []runtime.Event {
	return []runtime.Event{
		{Kind: runtime.KindText, Payload: text},
		{Kind: runtime.KindTurnDone},
	}
}

// TestAskDispatchesThroughRuntimeSelector verifies ask routes through the
// runtime interface when the selection resolves to claude: StartSession →
// SendTurn → streamed text → session marker persisted with the runtime's
// session id.
func TestAskDispatchesThroughRuntimeSelector(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	rt := newFakeRuntime(turnScript("claude answer"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "review this", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}

	if !strings.Contains(stdout.String(), "claude answer") {
		t.Errorf("stdout missing runtime text: %q", stdout.String())
	}
	if len(rt.startCalls) != 1 || len(rt.resumeCalls) != 0 {
		t.Errorf("start/resume calls = %d/%d, want 1/0", len(rt.startCalls), len(rt.resumeCalls))
	}
	if rt.startCalls[0].Persona != "security" {
		t.Errorf("SessionSpec.Persona = %q", rt.startCalls[0].Persona)
	}
	if len(rt.sentTurns) != 1 || rt.sentTurns[0].Text != "review this" {
		t.Errorf("SendTurn input = %+v", rt.sentTurns)
	}
	if !rt.closed {
		t.Error("runtime not closed after dispatch")
	}
	meta, ok := project.ReadBackendSessionMeta("security", "claude")
	if !ok || meta.ID != "fake-session-id" {
		t.Errorf("session marker = %#v (ok=%v), want ID fake-session-id", meta, ok)
	}
}

// TestAskResumesRuntimeSessionOnMetaMatch verifies the persisted marker is
// used to resume when the config fingerprint still matches.
func TestAskResumesRuntimeSessionOnMetaMatch(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	cfg, err := project.ResolvePersonaConfig("security")
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "claude", "security", "prior-session-id", cfg.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("resumed"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "again", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.resumeCalls) != 1 || rt.resumeCalls[0] != "prior-session-id" {
		t.Errorf("resume calls = %v, want [prior-session-id]", rt.resumeCalls)
	}
	if len(rt.startCalls) != 0 {
		t.Errorf("unexpected StartSession calls: %d", len(rt.startCalls))
	}
}

// TestAskFreshFlagStartsNewRuntimeSession verifies --fresh skips resume even
// when a marker exists.
func TestAskFreshFlagStartsNewRuntimeSession(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	cfg, err := project.ResolvePersonaConfig("security")
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WriteBackendSessionMeta("security", "claude", "security", "prior-session-id", cfg.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("fresh"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "again", true, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.startCalls) != 1 || len(rt.resumeCalls) != 0 {
		t.Errorf("start/resume calls = %d/%d, want 1/0", len(rt.startCalls), len(rt.resumeCalls))
	}
}

// TestAskFallsBackToCodexNativePath verifies that when the selector reports
// codex (via ErrNotConfigured, its by-design answer for codex), ask uses
// the existing codex-native dispatcher unchanged.
func TestAskFallsBackToCodexNativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	swapSelectorErr(t, &runtime.ErrNotConfigured{Runtime: "codex", Reason: "needs app-server transport"})

	invoked := 0
	prev := codexTurnDispatcher
	codexTurnDispatcher = func(_ context.Context, installed *project.InstalledPersona, prompt string, _ bool, _ project.PersonaConfig, _ []turninput.ImageDescriptorV1, stdout, _ io.Writer) error {
		invoked++
		if installed.Name != "security" || prompt != "review this" {
			t.Errorf("codex dispatcher got %q/%q", installed.Name, prompt)
		}
		_, _ = io.WriteString(stdout, "codex answer\n")
		return nil
	}
	t.Cleanup(func() { codexTurnDispatcher = prev })

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "", "security", "review this", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if invoked != 1 {
		t.Errorf("codex dispatcher invoked %d times, want 1", invoked)
	}
	if !strings.Contains(stdout.String(), "codex answer") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestAskRuntimeErrorEventReturnsError verifies KindError terminates the
// stream with a non-nil error carrying the payload.
func TestAskRuntimeErrorEventReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	rt := newFakeRuntime([]runtime.Event{
		{Kind: runtime.KindText, Payload: "partial"},
		{Kind: runtime.KindError, Payload: "boom: model timed out"},
	})
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	err := dispatchAskTo(context.Background(), "claude", "security", "hi", false, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom: model timed out") {
		t.Fatalf("expected error carrying payload, got %v", err)
	}
	// A failed turn must NOT overwrite the session marker.
	if _, ok := project.ReadBackendSessionMeta("security", "claude"); ok {
		t.Error("session marker written despite failed turn")
	}
}

// TestRemovedImageFlagPointsAtShow verifies the removed `--image` spelling
// on ask and live still parses and answers with a pointer at `show`, rather
// than urfave/cli's bare "flag provided but not defined", and that it never
// dispatches a turn.
func TestRemovedImageFlagPointsAtShow(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	rt := newFakeRuntime(turnScript("unused"))
	swapSelector(t, rt)

	for _, tc := range []struct {
		name    string
		command func() *cli.Command
		argv    []string
	}{
		{"ask", Ask, []string{"ask", "--image", "x.png", "security", "hi"}},
		{"live", Live, []string{"live", "--image", "x.png", "security", "hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.command()
			cmd.Writer = &bytes.Buffer{}
			cmd.ErrWriter = &bytes.Buffer{}
			err := cmd.Run(context.Background(), tc.argv)
			if err == nil || !strings.Contains(err.Error(), "shipmates show") {
				t.Fatalf("err = %v, want a pointer at shipmates show", err)
			}
			if strings.Contains(err.Error(), "not defined") {
				t.Fatalf("removed flag surfaced as a parse error: %v", err)
			}
		})
	}
	if len(rt.sentTurns) != 0 {
		t.Fatalf("a refused --image still dispatched a turn: %+v", rt.sentTurns)
	}
}

// askApprovalScript is the event sequence a runtime that asks before
// running a tool produces: a permission request, then the turn's end once
// the request has been answered.
func askApprovalScript(command string) []runtime.Event {
	return []runtime.Event{
		{Kind: runtime.KindApprovalNeeded, TurnID: "fake-turn", Payload: claude.ApprovalRequest{
			RequestID: "ask-req-1",
			ToolName:  "Bash",
			InputJSON: json.RawMessage(`{"command":` + strconv.Quote(command) + `}`),
		}},
		{Kind: runtime.KindTurnDone},
	}
}

// requireSecurePolicyLoader skips a test that needs a real policy snapshot
// on platforms without the openat-based loader (everything but unix).
func requireSecurePolicyLoader(t *testing.T) {
	t.Helper()
	if !policy.SecureLoadSupported() {
		t.Skip("policy.Load is unix-only; approvals degrade to deny-all elsewhere")
	}
}

// writeAskPolicy replaces the persona overlay installed by
// installCodexPersona with a single rule.
func writeAskPolicy(t *testing.T, persona, effect, id, command string) {
	t.Helper()
	body := "version: 1\nallow: []\nask: []\ndeny: []\n"
	rule := "  - id: " + id + "\n    kind: process.exec\n    match:\n      command_exact: " + command + "\n    reason: test rule\n"
	body = strings.Replace(body, effect+": []\n", effect+":\n"+rule, 1)
	if err := os.WriteFile(filepath.Join(".shipmates", "policies", persona+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAskAnswersApprovalsFromPolicy proves `ask` never leaves a permission
// request unanswered — an unanswered one wedges the turn until the runtime
// times out — and that policy is the authority when there is no operator.
func TestAskAnswersApprovalsFromPolicy(t *testing.T) {
	requireSecurePolicyLoader(t)
	for _, tc := range []struct {
		name      string
		effect    string
		wantAllow bool
		wantLog   string
	}{
		{"an allow rule lets the tool run", "allow", true, "approval: allowed by policy"},
		{"no matching rule refuses, since ask cannot prompt", "deny", false, "approval: denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			installCodexPersona(t, "security")
			writeAskPolicy(t, "security", tc.effect, "gitstatus", "git status")
			rt := newFakeRuntime(askApprovalScript("git status"))
			rt.caps.Approvals = true
			swapSelector(t, rt)

			var stdout, stderr bytes.Buffer
			if err := dispatchAskTo(context.Background(), "claude", "security", "check the tree", false, &stdout, &stderr); err != nil {
				t.Fatalf("dispatchAskTo: %v (stderr: %s)", err, stderr.String())
			}
			if len(rt.approvals) != 1 {
				t.Fatalf("approvals answered = %d, want 1", len(rt.approvals))
			}
			if rt.approvals[0].Allow != tc.wantAllow {
				t.Errorf("Allow = %v, want %v", rt.approvals[0].Allow, tc.wantAllow)
			}
			if !tc.wantAllow && rt.approvals[0].Rationale == "" {
				t.Error("denial carried no rationale")
			}
			if !strings.Contains(stderr.String(), tc.wantLog) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantLog)
			}
		})
	}
}

// TestAskDoesNotLoadPolicyForUnmediatedRuntimes keeps the ask path working
// for a runtime that never asks: no policy is required and none is loaded.
func TestAskDoesNotLoadPolicyForUnmediatedRuntimes(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	if err := os.Remove(filepath.Join(".shipmates", "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("no tools here"))
	rt.caps.Approvals = false
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "hi", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if !strings.Contains(stdout.String(), "no tools here") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestAskRefusesTurnOnInvalidPolicyForMediatedRuntime proves a mediated
// runtime never gets a prompt when policy cannot be evaluated: shipmates
// would have nothing to answer approvals with.
func TestAskRefusesTurnOnInvalidPolicyForMediatedRuntime(t *testing.T) {
	requireSecurePolicyLoader(t)
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	if err := os.WriteFile(filepath.Join(".shipmates", "policy.yaml"), []byte("version: 1\nallow: [nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("never sent"))
	rt.caps.Approvals = true
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	err := dispatchAskTo(context.Background(), "claude", "security", "hi", false, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "policy validation failed") {
		t.Fatalf("err = %v, want policy validation failed", err)
	}
	if len(rt.sentTurns) != 0 {
		t.Fatalf("a turn was dispatched despite invalid policy: %+v", rt.sentTurns)
	}
}

// TestAskWithoutSecurePolicyLoaderDeniesEverything covers the non-unix
// posture: the turn still runs, but with no policy authority every request
// is refused rather than waved through, and the operator is told why.
func TestAskWithoutSecurePolicyLoaderDeniesEverything(t *testing.T) {
	if policy.SecureLoadSupported() {
		t.Skip("this platform has the secure loader; see TestAskAnswersApprovalsFromPolicy")
	}
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	rt := newFakeRuntime(askApprovalScript("git status"))
	rt.caps.Approvals = true
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := dispatchAskTo(context.Background(), "claude", "security", "check the tree", false, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskTo: %v", err)
	}
	if len(rt.approvals) != 1 || rt.approvals[0].Allow {
		t.Fatalf("approvals = %+v, want exactly one denial", rt.approvals)
	}
	if !strings.Contains(stderr.String(), "no secure policy loader on this platform") {
		t.Errorf("stderr did not explain the degraded posture: %q", stderr.String())
	}
}

// TestAskRuntimePathReservesCaptain keeps the captain guard on the runtime
// path.
func TestAskRuntimePathReservesCaptain(t *testing.T) {
	t.Chdir(t.TempDir())
	rt := newFakeRuntime(nil)
	swapSelector(t, rt)
	var stdout, stderr bytes.Buffer
	err := dispatchAskTo(context.Background(), "claude", "captain", "hi", false, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected captain-reserved error, got %v", err)
	}
}
