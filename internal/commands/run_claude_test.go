package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/turninput"
)

// fakeRuntime is a hand-rolled runtime.Runtime for testing the ask dispatch
// path. It emits a scripted event sequence on SendTurn and records the
// calls made to it, so tests can assert both what got printed and what the
// command layer handed to the runtime.
type fakeRuntime struct {
	name   string
	events chan runtime.Event
	script []runtime.Event

	startCalls  []runtime.SessionSpec
	resumeCalls []string
	sentTurns   []runtime.TurnInput
	closed      bool

	sessionID string
}

func newFakeRuntime(script []runtime.Event) *fakeRuntime {
	return &fakeRuntime{
		name:      "claude",
		events:    make(chan runtime.Event, len(script)+1),
		script:    script,
		sessionID: "fake-session-id",
	}
}

func (f *fakeRuntime) Name() string                               { return f.name }
func (f *fakeRuntime) Capabilities() runtime.Caps                 { return runtime.Caps{Streaming: true} }
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
func (f *fakeRuntime) ResolveApproval(context.Context, runtime.ApprovalResponse, runtime.ApprovalDecision) (bool, error) {
	return false, nil
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
	if err := dispatchAskToImages(context.Background(), "claude", "security", "review this", false, nil, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskToImages: %v", err)
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
	if err := dispatchAskToImages(context.Background(), "claude", "security", "again", false, nil, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskToImages: %v", err)
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
	if err := dispatchAskToImages(context.Background(), "claude", "security", "again", true, nil, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskToImages: %v", err)
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
	if err := dispatchAskToImages(context.Background(), "", "security", "review this", false, nil, &stdout, &stderr); err != nil {
		t.Fatalf("dispatchAskToImages: %v", err)
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
	err := dispatchAskToImages(context.Background(), "claude", "security", "hi", false, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom: model timed out") {
		t.Fatalf("expected error carrying payload, got %v", err)
	}
	// A failed turn must NOT overwrite the session marker.
	if _, ok := project.ReadBackendSessionMeta("security", "claude"); ok {
		t.Error("session marker written despite failed turn")
	}
}

// TestAskRuntimePathRejectsImages verifies --image is refused on the
// runtime-interface path with an actionable message.
func TestAskRuntimePathRejectsImages(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	rt := newFakeRuntime(turnScript("unused"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	err := dispatchAskToImages(context.Background(), "claude", "security", "hi", false, []string{"x.png"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not yet supported") {
		t.Fatalf("expected image rejection, got %v", err)
	}
}

// TestAskRuntimePathReservesCaptain keeps the captain guard on the runtime
// path.
func TestAskRuntimePathReservesCaptain(t *testing.T) {
	t.Chdir(t.TempDir())
	rt := newFakeRuntime(nil)
	swapSelector(t, rt)
	var stdout, stderr bytes.Buffer
	err := dispatchAskToImages(context.Background(), "claude", "captain", "hi", false, nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected captain-reserved error, got %v", err)
	}
}
