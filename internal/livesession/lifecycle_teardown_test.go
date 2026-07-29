package livesession

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
)

// stubBackend is a Backend that only records teardown. Every RPC other than
// StartThread/ResumeThread/Close is unreachable on these paths, so reaching one
// is a test failure rather than a silent no-op.
type stubBackend struct {
	mu     sync.Mutex
	closed int
	t      *testing.T
	events chan codexapp.Event
	// startThread, when set, runs inside StartThread — the seam these tests use
	// to interleave a Shutdown with a startup RPC that is still in flight.
	startThread func()
	thread      string
}

func newStubBackend(t *testing.T) *stubBackend {
	return &stubBackend{t: t, events: make(chan codexapp.Event), thread: "thread"}
}

func (b *stubBackend) StartThread(context.Context, codexapp.ThreadOptions) (codexapp.Thread, error) {
	if b.startThread != nil {
		b.startThread()
	}
	return codexapp.Thread{ID: b.thread}, nil
}

func (b *stubBackend) ResumeThread(_ context.Context, id string, _ codexapp.ThreadOptions) (codexapp.Thread, error) {
	if b.startThread != nil {
		b.startThread()
	}
	return codexapp.Thread{ID: id}, nil
}

func (b *stubBackend) StartTurn(context.Context, string, codexapp.TurnInput) (codexapp.Turn, error) {
	b.unexpected("StartTurn")
	return codexapp.Turn{}, nil
}

func (b *stubBackend) SteerTurn(context.Context, string, string, string) error {
	b.unexpected("SteerTurn")
	return nil
}

func (b *stubBackend) InterruptTurn(context.Context, string, string) error {
	b.unexpected("InterruptTurn")
	return nil
}

func (b *stubBackend) ResolveApproval(context.Context, codexapp.ApprovalResponse, codexapp.ApprovalDecision) (bool, error) {
	b.unexpected("ResolveApproval")
	return false, nil
}

func (b *stubBackend) Events() <-chan codexapp.Event { return b.events }

func (b *stubBackend) ProcessGroupHandle() codexapp.ProcessGroupHandle { return nil }

func (b *stubBackend) Close(context.Context) error {
	b.mu.Lock()
	b.closed++
	b.mu.Unlock()
	return nil
}

func (b *stubBackend) closes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *stubBackend) unexpected(op string) {
	if b.t != nil {
		b.t.Errorf("backend %s reached on a fail-closed teardown path", op)
	}
}

// assertFinishedClosed asserts the complete set of effects only Session.finish
// performs. Every one of them was missing from the hand-rolled
// "s.state, s.shutting = Failed, true" teardown, and every one of them is what
// keeps the persona usable afterwards.
func assertFinishedClosed(t *testing.T, s *Session, backend *stubBackend, released <-chan struct{}) {
	t.Helper()
	s.mu.Lock()
	state, shutting, turnID := s.state, s.shutting, s.turnID
	feed := s.feedLocked(0)
	s.mu.Unlock()

	if state != Failed {
		t.Errorf("state = %s, want %s", state, Failed)
	}
	if shutting {
		t.Error("shutting still set: finish clears it once the teardown has landed")
	}
	if turnID != "" {
		t.Errorf("turn %q still owned after teardown", turnID)
	}
	if got := backend.closes(); got != 1 {
		t.Errorf("adapter closed %d times, want 1: the backend child keeps executing the turn otherwise", got)
	}
	select {
	case <-released:
	default:
		t.Error("dispatch lock never released: the persona would fail Busy on every later StartIdle")
	}
	select {
	case <-s.Done():
	default:
		t.Error("Done() never closed: every waiter on this session blocks forever")
	}
	raw, err := json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"session.failed"`) {
		t.Errorf("session.failed not published; feed=%s", raw)
	}
}

// localInterruptFixture is interruptFixture's session wired for teardown
// observation: a real adapter to close and a real dispatch-lock release to hand
// back.
func localInterruptFixture(t *testing.T) (*Manager, *Session, *stubBackend, chan struct{}) {
	t.Helper()
	m, s, _, _, _, _ := interruptFixture(t)
	backend := newStubBackend(t)
	released := make(chan struct{})
	s.adapter = backend
	s.release = func() { close(released) }
	return m, s, backend, released
}

// TestLocalInterruptApprovalUncertaintyFailsClosed is LocalInterrupt's half of
// the same defect. It had no test at all, which is how it kept the bypass after
// the remote path was reviewed.
func TestLocalInterruptApprovalUncertaintyFailsClosed(t *testing.T) {
	m, s, backend, released := localInterruptFixture(t)
	c := NewRemoteInterruptCoordinator(m, nil, nil)
	i := &fakeExactInterrupter{result: ExactTurnInterrupted}
	got := c.LocalInterrupt(context.Background(), "backend", "session", "thread", "turn", &fakeApprovalCanceller{result: ApprovalUnknown}, i)
	if got != ExactTurnUnknown {
		t.Fatalf("outcome = %s, want %s", got, ExactTurnUnknown)
	}
	if i.calls != 0 {
		t.Fatalf("exact-turn interrupt attempted %d times after unprovable approval cancellation", i.calls)
	}
	assertFinishedClosed(t, s, backend, released)
}

// TestLocalInterruptApprovalUncertaintyLeavesForeignTurnAlone covers the second
// half of the LocalInterrupt defect: it released the session lock across the
// CancelPendingApproval call and then failed the session without re-checking
// that it still owned the tuple it was asked about, so an unprovable
// cancellation for a turn that had already ended killed the session's
// successor turn instead.
func TestLocalInterruptApprovalUncertaintyLeavesForeignTurnAlone(t *testing.T) {
	m, s, backend, released := localInterruptFixture(t)
	c := NewRemoteInterruptCoordinator(m, nil, nil)
	approvals := &retargetingCanceller{session: s, turnID: "successor"}
	got := c.LocalInterrupt(context.Background(), "backend", "session", "thread", "turn", approvals, &fakeExactInterrupter{result: ExactTurnInterrupted})
	if got != ExactTurnUnknown {
		t.Fatalf("outcome = %s, want %s", got, ExactTurnUnknown)
	}
	s.mu.Lock()
	state, turnID := s.state, s.turnID
	s.mu.Unlock()
	if state == Failed || turnID != "successor" {
		t.Fatalf("successor turn collateral-damaged: state=%s turn=%q", state, turnID)
	}
	if backend.closes() != 0 {
		t.Error("adapter closed for a turn this session no longer owns")
	}
	select {
	case <-released:
		t.Error("dispatch lock released for a turn this session no longer owns")
	default:
	}
}

// retargetingCanceller simulates the turn ending and a successor starting while
// the approval-cancel RPC is in flight — the exact window LocalInterrupt opens
// by releasing the session lock around that call.
type retargetingCanceller struct {
	session *Session
	turnID  string
}

func (r *retargetingCanceller) CancelPendingApproval(context.Context, string, string, string) ApprovalCancelOutcome {
	r.session.mu.Lock()
	r.session.turnID, r.session.state = r.turnID, Working
	r.session.mu.Unlock()
	return ApprovalUnknown
}

// TestStartIdleContinuationRefusesResurrectionAfterFinish is the unit-level
// regression for the StartIdle-vs-Shutdown race. StartIdle re-locks after its
// blocking StartThread/ResumeThread RPC and used to install
// "s.threadID, s.state = thread.ID, Idle" while checking only ctx.Err(). The
// session has been visible in m.sessions since before the dispatch lock, so a
// concurrent Shutdown/ShutdownAll can already have run finish — and finish is
// one-shot. Overwriting Stopped with Idle therefore produced a live-looking
// session whose doneOnce was spent, which is unrecoverable: no later finish can
// ever run, so Shutdown stops closing the adapter and the persona reports Busy
// forever.
func TestStartIdleContinuationRefusesResurrectionAfterFinish(t *testing.T) {
	s := startingSession()
	released := make(chan struct{})
	s.release = func() { close(released) }
	backend := newStubBackend(t)
	s.adapter = backend

	// The concurrent Shutdown/ShutdownAll.
	s.finish(Stopped, backend, "")

	s.mu.Lock()
	claimed := s.startupClaimedLocked()
	s.mu.Unlock()
	if !claimed {
		t.Fatal("StartIdle would have overwritten a session finish already tore down")
	}

	// Prove why resurrection is unrecoverable rather than merely untidy: the
	// one-shot is spent, so the terminal transition can never happen again.
	s.finish(Failed, backend, codexapp.Internal)
	if got := s.Snapshot().State; got != Stopped {
		t.Fatalf("state = %s after a second finish; the one-shot was not spent", got)
	}
	select {
	case <-released:
	default:
		t.Fatal("dispatch lock not released by the finish that won the race")
	}
}

// TestStartIdleDoesNotInstallReleaseAfterFinish covers the same race one step
// earlier: StartIdle installed s.release unconditionally after acquiring the
// dispatch lock, and finish reads that field exactly once. A finish that won
// the race never sees the release, so the persona's lock file stays held until
// the process exits.
func TestStartIdleDoesNotInstallReleaseAfterFinish(t *testing.T) {
	s := startingSession()
	s.finish(Stopped, nil, "")

	released := false
	s.mu.Lock()
	claimed := s.startupClaimedLocked()
	if !claimed {
		s.release = func() { released = true }
	}
	s.mu.Unlock()
	if !claimed {
		t.Fatal("StartIdle would have handed the dispatch-lock release to a session that can never run finish again")
	}
	if released {
		t.Fatal("release installed on a torn-down session")
	}
}

// TestStartupClaimedLockedCoversEveryTerminalRace pins the predicate both
// StartIdle re-locks share, so the two checks cannot drift apart again.
func TestStartupClaimedLockedCoversEveryTerminalRace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    State
		shutting bool
		want     bool
	}{
		{"still starting", Starting, false, false},
		{"shutdown claimed the teardown", Starting, true, true},
		{"finish ran with Stopped", Stopped, false, true},
		{"finish ran with Failed", Failed, false, true},
		{"already advanced", Idle, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{state: tc.state, shutting: tc.shutting}
			s.mu.Lock()
			got := s.startupClaimedLocked()
			s.mu.Unlock()
			if got != tc.want {
				t.Fatalf("startupClaimedLocked() = %v, want %v", got, tc.want)
			}
		})
	}
}

func startingSession() *Session {
	return &Session{persona: "backend", sessionID: "sid", state: Starting, done: make(chan struct{}), notify: make(chan struct{}, 1), nextSequence: 1}
}

// TestStartIdleDoesNotResurrectSessionShutDownMidStartup drives the real race
// end to end: Shutdown runs while StartIdle is blocked inside its StartThread
// RPC, and StartIdle must abandon the startup instead of publishing
// session.ready on a session whose doneOnce is already spent.
func TestStartIdleDoesNotResurrectSessionShutDownMidStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		// StartIdle writes the live-continuity marker, and
		// project.WriteLiveContinuityBackendAt fsyncs the containing directory.
		// Windows refuses FlushFileBuffers on a read-only directory handle, so
		// every StartIdle test in this package fails there before reaching the
		// code under test. Pre-existing and outside this package.
		t.Skip("project.WriteLiveContinuityBackendAt cannot fsync a directory on Windows")
	}
	fixture(t, "backend")
	backend := newStubBackend(t)
	inStartThread := make(chan struct{})
	proceed := make(chan struct{})
	// The hook fires for ResumeThread as well as StartThread, and the
	// reusability check at the end of this test starts a second session, so it
	// runs more than once. Signal the first entry only; both channels are
	// closed by then, so later calls fall straight through.
	var signalOnce sync.Once
	backend.startThread = func() {
		signalOnce.Do(func() { close(inStartThread) })
		<-proceed
	}
	start := func(context.Context, codexapp.StartOptions) (Backend, codexapp.Capabilities, error) {
		return backend, codexapp.Capabilities{}, nil
	}
	m := New(start, codexapp.StartOptions{})

	type result struct {
		s   *Session
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
		done <- result{s, err}
	}()

	select {
	case <-inStartThread:
	case <-time.After(10 * time.Second):
		t.Fatal("StartThread never reached")
	}
	s, err := m.Session("backend")
	if err != nil {
		t.Fatalf("session not registered mid-startup: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown mid-startup: %v", err)
	}
	close(proceed)

	got := <-done
	if got.err == nil {
		t.Fatal("StartIdle reported success for a session Shutdown already tore down")
	}
	if snap := s.Snapshot(); snap.State != Stopped {
		t.Fatalf("state = %s, want %s: the terminal state was overwritten", snap.State, Stopped)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done() not closed")
	}
	// The adapter must be closed by whichever of Shutdown or the abandoned
	// startup reaches it; the real Backends guard Close with a sync.Once, so
	// what matters is that it is never left running.
	if n := backend.closes(); n == 0 {
		t.Fatal("adapter never closed: the backend child outlives the abandoned startup")
	}
	// The persona is reusable: the dispatch lock came back.
	if _, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"}); err != nil && ErrorCode(err) == Busy {
		t.Fatal("persona still Busy: the dispatch lock was never released")
	}
}
