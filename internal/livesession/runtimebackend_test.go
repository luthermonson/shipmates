package livesession

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
)

// fakeRuntime is a scriptable runtime.Runtime for exercising the live
// session state machine on a non-codex backend.
type fakeRuntime struct {
	caps   runtime.Caps
	events chan runtime.Event

	mu           sync.Mutex
	sessionID    string
	turnCounter  int
	sentTurns    []runtime.TurnInput
	steers       []string
	steerInputs  []runtime.TurnInput
	interrupts   [][2]string
	closed       bool
	resumedIDs   []string
	startedSpecs []runtime.SessionSpec
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		caps:      runtime.Caps{Streaming: true, Interrupt: true, Steer: true, Attachments: true},
		events:    make(chan runtime.Event, 32),
		sessionID: "claude-session-1",
	}
}

func (f *fakeRuntime) Name() string                 { return "claude" }
func (f *fakeRuntime) Capabilities() runtime.Caps   { return f.caps }
func (f *fakeRuntime) Events() <-chan runtime.Event { return f.events }
func (f *fakeRuntime) CloseSession(context.Context, string) error {
	return nil
}

func (f *fakeRuntime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startedSpecs = append(f.startedSpecs, spec)
	return fakeRTSession{id: f.sessionID, persona: spec.Persona, dir: spec.ProjectDir}, nil
}

func (f *fakeRuntime) ResumeSession(_ context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumedIDs = append(f.resumedIDs, id)
	return fakeRTSession{id: id, persona: spec.Persona, dir: spec.ProjectDir}, nil
}

func (f *fakeRuntime) SendTurn(_ context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	f.mu.Lock()
	f.turnCounter++
	id := "claude-turn-1"
	if f.turnCounter > 1 {
		id = "claude-turn-" + string(rune('0'+f.turnCounter))
	}
	f.sentTurns = append(f.sentTurns, in)
	f.mu.Unlock()
	return fakeRTTurn{id: id, sessionID: sessionID}, nil
}

func (f *fakeRuntime) SteerTurn(_ context.Context, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
	return nil
}

func (f *fakeRuntime) SteerTurnInput(_ context.Context, _, _ string, in runtime.TurnInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerInputs = append(f.steerInputs, in)
	return nil
}

func (f *fakeRuntime) InterruptTurn(_ context.Context, sessionID, turnID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, [2]string{sessionID, turnID})
	return nil
}

func (f *fakeRuntime) ResolveApproval(context.Context, runtime.ApprovalResponse, runtime.ApprovalDecision) (bool, error) {
	return false, &runtime.ErrUnsupported{Runtime: "claude", Feature: "ResolveApproval"}
}
func (f *fakeRuntime) InstallPersona(context.Context, string, runtime.PersonaSpec) error { return nil }
func (f *fakeRuntime) UninstallPersona(context.Context, string, string) error            { return nil }
func (f *fakeRuntime) InstallMemoryHook(context.Context, string) error                   { return nil }
func (f *fakeRuntime) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}

func (f *fakeRuntime) emit(ev runtime.Event) { f.events <- ev }

type fakeRTSession struct{ id, persona, dir string }

func (s fakeRTSession) ID() string         { return s.id }
func (s fakeRTSession) Persona() string    { return s.persona }
func (s fakeRTSession) ProjectDir() string { return s.dir }

type fakeRTTurn struct{ id, sessionID string }

func (t fakeRTTurn) ID() string           { return t.id }
func (t fakeRTTurn) SessionID() string    { return t.sessionID }
func (t fakeRTTurn) StartedAt() time.Time { return time.Now() }

func runtimeManager(t *testing.T, rt runtime.Runtime) *Manager {
	t.Helper()
	sel := runtime.SelectorFunc(func(context.Context, string, string) (runtime.Runtime, string, error) {
		return rt, "test", nil
	})
	// The codex StartAdapter must never be reached on this path.
	start := func(context.Context, codexapp.StartOptions) (Backend, codexapp.Capabilities, error) {
		t.Error("codex app-server started for a claude-selected session")
		return nil, codexapp.Capabilities{}, failure(Internal)
	}
	return NewWithRuntime("", sel, "", start, codexapp.StartOptions{})
}

// TestLiveRunsOnRuntimeBackend is the end-to-end claude live path: the
// session reports the claude backend, the runtime's session id occupies the
// thread slot, its turn id the turn slot, and its events land in the feed.
func TestLiveRunsOnRuntimeBackend(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "review the diff"})
	if err != nil {
		t.Fatalf("StartLive: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	snap := s.Snapshot()
	if snap.Backend != "claude" {
		t.Errorf("backend = %q, want claude", snap.Backend)
	}
	if snap.ThreadID != "claude-session-1" || snap.TurnID != "claude-turn-1" {
		t.Fatalf("identity = thread %q turn %q", snap.ThreadID, snap.TurnID)
	}
	if snap.State != Working {
		t.Fatalf("state = %s, want working", snap.State)
	}
	if len(rt.sentTurns) != 1 || rt.sentTurns[0].Text != "review the diff" {
		t.Fatalf("sent turns = %+v", rt.sentTurns)
	}

	rt.emit(runtime.Event{Kind: runtime.KindToolCall, SessionID: snap.ThreadID, TurnID: snap.TurnID, Payload: claude.ToolCall{Name: "Bash"}})
	rt.emit(runtime.Event{Kind: runtime.KindText, SessionID: snap.ThreadID, TurnID: snap.TurnID, Payload: "here is the answer"})
	rt.emit(runtime.Event{Kind: runtime.KindTurnDone, SessionID: snap.ThreadID, TurnID: snap.TurnID})
	waitForState(t, s, Idle)

	feed, _ := json.Marshal(s.Feed(0))
	for _, want := range []string{`"backend":"claude"`, `"kind":"activity"`, `"category":"command"`, "here is the answer", `"kind":"turn.completed"`} {
		if !strings.Contains(string(feed), want) {
			t.Errorf("feed missing %q: %s", want, feed)
		}
	}
}

// TestRuntimeBackendPreservesExactTurnTargeting keeps the safety property
// that made tell/interrupt exact-tuple operations: a stale turn id is
// refused, never redirected to the current turn.
func TestRuntimeBackendPreservesExactTurnTargeting(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	if _, err := m.Tell(context.Background(), "backend", snap.SessionID, snap.ThreadID, "claude-turn-999", "wrong turn"); ErrorCode(err) != StaleTarget {
		t.Fatalf("stale tell err = %v, want stale_target", err)
	}
	if _, err := m.Interrupt(context.Background(), "backend", snap.SessionID, snap.ThreadID, "claude-turn-999"); ErrorCode(err) != StaleTarget {
		t.Fatalf("stale interrupt err = %v, want stale_target", err)
	}
	if len(rt.steers) != 0 || len(rt.interrupts) != 0 {
		t.Fatalf("stale target reached the runtime: steers=%v interrupts=%v", rt.steers, rt.interrupts)
	}

	if _, err := m.Tell(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID, "mid-turn note"); err != nil {
		t.Fatalf("exact tell: %v", err)
	}
	if len(rt.steers) != 1 || rt.steers[0] != "mid-turn note" {
		t.Fatalf("steers = %v", rt.steers)
	}
	if _, err := m.Interrupt(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID); err != nil {
		t.Fatalf("exact interrupt: %v", err)
	}
	if len(rt.interrupts) != 1 || rt.interrupts[0] != [2]string{snap.ThreadID, snap.TurnID} {
		t.Fatalf("interrupts = %v", rt.interrupts)
	}
}

// TestRuntimeBackendSurfacesCapabilityGaps proves a missing capability is
// reported as a runtime-scoped error rather than silently ignored.
func TestRuntimeBackendSurfacesCapabilityGaps(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	rt.caps = runtime.Caps{Streaming: true}
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	_, err = m.Tell(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID, "steer me")
	if err == nil || !runtimeBackendError(err) || !strings.Contains(err.Error(), "steer") {
		t.Fatalf("steer without capability err = %v", err)
	}
	if len(rt.steers) != 0 {
		t.Fatalf("steer reached a runtime that cannot steer: %v", rt.steers)
	}
	_, err = m.Interrupt(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID)
	if err == nil || !runtimeBackendError(err) || !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("interrupt without capability err = %v", err)
	}
}

// TestRuntimeBackendResumesItsOwnMarker proves the continuity marker is
// per-backend: a second session on the same persona resumes the claude
// session id rather than starting a new one.
func TestRuntimeBackendResumesItsOwnMarker(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt2 := newFakeRuntime()
	m2 := runtimeManager(t, rt2)
	s2, err := m2.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	if len(rt2.resumedIDs) != 1 || rt2.resumedIDs[0] != "claude-session-1" {
		t.Fatalf("resumed ids = %v, want [claude-session-1]", rt2.resumedIDs)
	}
	if len(rt2.startedSpecs) != 0 {
		t.Fatalf("started a fresh session despite a marker: %+v", rt2.startedSpecs)
	}
}

// TestRuntimeBackendTranslatesOnlyAttributableEvents keeps the owner's
// protocol invariant intact: events that cannot be pinned to the live turn
// are dropped, not forwarded as a tuple mismatch that would fail the
// session.
func TestRuntimeBackendTranslatesOnlyAttributableEvents(t *testing.T) {
	rt := newFakeRuntime()
	b := newRuntimeBackend(rt, "backend", t.TempDir())
	for _, tc := range []struct {
		name    string
		in      runtime.Event
		forward bool
		kind    codexapp.EventKind
	}{
		{"text with turn", runtime.Event{Kind: runtime.KindText, SessionID: "s", TurnID: "t", Payload: "hi"}, true, codexapp.AgentMessage},
		{"text without turn", runtime.Event{Kind: runtime.KindText, SessionID: "s", Payload: "hi"}, false, ""},
		{"empty text", runtime.Event{Kind: runtime.KindText, SessionID: "s", TurnID: "t", Payload: ""}, false, ""},
		{"tool call", runtime.Event{Kind: runtime.KindToolCall, SessionID: "s", TurnID: "t", Payload: claude.ToolCall{Name: "Edit"}}, true, codexapp.Activity},
		{"turn done", runtime.Event{Kind: runtime.KindTurnDone, SessionID: "s", TurnID: "t"}, true, codexapp.TurnCompleted},
		{"turn error", runtime.Event{Kind: runtime.KindError, SessionID: "s", TurnID: "t"}, true, codexapp.TurnFailed},
		{"tool result", runtime.Event{Kind: runtime.KindToolResult, SessionID: "s", TurnID: "t"}, false, ""},
		{"backend noise", runtime.Event{Kind: runtime.KindBackend, SessionID: "s", TurnID: "t"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, forward := b.translate(tc.in)
			if forward != tc.forward {
				t.Fatalf("forward = %v, want %v", forward, tc.forward)
			}
			if forward && out.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", out.Kind, tc.kind)
			}
			if forward && (out.ThreadID != tc.in.SessionID || out.TurnID != tc.in.TurnID) {
				t.Fatalf("identity = %q/%q", out.ThreadID, out.TurnID)
			}
		})
	}
}
