package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

type injectedInterruptAudit struct {
	calls   int
	failAt  int
	order   *[]string
	records []RemoteInterruptAuditCandidate
}

func (a *injectedInterruptAudit) AppendRemoteInterrupt(v RemoteInterruptAuditCandidate) error {
	a.calls++
	if a.order != nil {
		*a.order = append(*a.order, "audit:"+v.Kind)
	}
	if a.calls == a.failAt {
		return errors.New("injected")
	}
	a.records = append(a.records, v)
	return nil
}

type interruptFakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *interruptFakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *interruptFakeClock) Advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

type fakeApprovalCanceller struct {
	mu     sync.Mutex
	calls  int
	order  *[]string
	result ApprovalCancelOutcome
}

func (f *fakeApprovalCanceller) CancelPendingApproval(context.Context, string, string, string) ApprovalCancelOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "approval")
	}
	return f.result
}

type fakeExactInterrupter struct {
	mu      sync.Mutex
	calls   int
	order   *[]string
	result  ExactTurnInterruptOutcome
	entered chan struct{}
	release chan struct{}
}

func (f *fakeExactInterrupter) InterruptExactTurn(context.Context, string, string, string) ExactTurnInterruptOutcome {
	f.mu.Lock()
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "interrupt")
	}
	f.mu.Unlock()
	if f.entered != nil {
		close(f.entered)
	}
	if f.release != nil {
		<-f.release
	}
	return f.result
}

func interruptFixture(t *testing.T) (*Manager, *Session, *interruptFakeClock, RemoteInterruptOperator, RemoteInterruptRequest, RemoteInterruptTarget) {
	t.Helper()
	clock := &interruptFakeClock{at: time.Unix(500, 0)}
	m := &Manager{sessions: make(map[string]*Session)}
	// done/notify/nextSequence are what Session.finish and publishLocked need:
	// the fail-closed interrupt paths now go through finish, and closing a nil
	// done channel would panic instead of reporting the defect.
	s := &Session{persona: "backend", sessionID: "session", threadID: "thread", turnID: "turn", state: Working, done: make(chan struct{}), notify: make(chan struct{}, 1), nextSequence: 1}
	m.sessions["backend"] = s
	op := RemoteInterruptOperator{SubjectID: "subject", CredentialID: "credential", CredentialGeneration: 9, FleetID: "fleet", Capability: RemoteInterruptCapability}
	req := RemoteInterruptRequest{ProtocolVersion: 1, OperationID: "operation", OperationNonce: "nonce", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", TargetReference: "target"}
	target := RemoteInterruptTarget{Reference: "target", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: "session", Backend: project.CodexAppServerBackend, ThreadID: "thread", TurnID: "turn", ExpiresAt: clock.Now().Add(time.Minute)}
	return m, s, clock, op, req, target
}

func TestRemoteInterruptApprovalBeforeInterruptAndReplay(t *testing.T) {
	m, _, clock, op, req, target := interruptFixture(t)
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	order := []string{}
	a := &fakeApprovalCanceller{order: &order, result: ApprovalCancelled}
	i := &fakeExactInterrupter{order: &order, result: ExactTurnInterrupted}
	r, audit := c.Interrupt(context.Background(), op, req, target, a, i)
	if r.Outcome != RemoteInterruptInterrupted || strings.Join(order, ",") != "approval,interrupt" || !audit.ApprovalCancelled {
		t.Fatalf("result=%+v order=%v audit=%+v", r, order, audit)
	}
	replay, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if replay != r || a.calls != 1 || i.calls != 1 {
		t.Fatalf("replay=%+v approval=%d interrupt=%d", replay, a.calls, i.calls)
	}
}

func TestRemoteInterruptAuditFailuresPreventEffects(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		m, s, clock, op, req, target := interruptFixture(t)
		c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
		c.audit = &injectedInterruptAudit{failAt: failAt}
		a := &fakeApprovalCanceller{result: ApprovalCancelled}
		i := &fakeExactInterrupter{result: ExactTurnInterrupted}
		r, _ := c.Interrupt(context.Background(), op, req, target, a, i)
		s.mu.Lock()
		state := s.state
		s.mu.Unlock()
		if r.Outcome != RemoteInterruptIndeterminate || r.ReasonCode != "audit_unavailable" || a.calls != 0 || i.calls != 0 || state != Working {
			t.Fatalf("failAt=%d result=%+v calls=%d/%d state=%s", failAt, r, a.calls, i.calls, state)
		}
	}
}

func TestRemoteInterruptAuditOrdering(t *testing.T) {
	m, _, clock, op, req, target := interruptFixture(t)
	order := []string{}
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	c.audit = &injectedInterruptAudit{order: &order}
	c.persist = func() error { order = append(order, "journal"); return c.persistLocked() }
	a := &fakeApprovalCanceller{order: &order, result: ApprovalAbsent}
	i := &fakeExactInterrupter{order: &order, result: ExactTurnInterrupted}
	c.Interrupt(context.Background(), op, req, target, a, i)
	want := "audit:remote_interrupt.received,journal,audit:remote_interrupt.accepted,approval,interrupt,journal,audit:remote_interrupt.interrupted"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

func TestRemoteInterruptAlreadyTerminalJournalPrecedesAudit(t *testing.T) {
	m, s, clock, op, req, target := interruptFixture(t)
	s.state = Idle
	order := []string{}
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	c.audit = &injectedInterruptAudit{order: &order}
	c.persist = func() error { order = append(order, "journal"); return c.persistLocked() }
	r, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
	if r.Outcome != RemoteInterruptAlreadyTerminal {
		t.Fatal(r)
	}
	want := "audit:remote_interrupt.received,journal,audit:remote_interrupt.already_terminal"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("order=%s want=%s", got, want)
	}
}

func TestRemoteInterruptTerminalJournalFailuresDoNotPublishDefinitiveAudit(t *testing.T) {
	for _, alreadyTerminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "already-terminal"}[alreadyTerminal], func(t *testing.T) {
			m, s, clock, op, req, target := interruptFixture(t)
			if alreadyTerminal {
				s.state = Idle
			}
			dir := filepath.Join(t.TempDir(), "ops")
			c, err := OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			audit := &injectedInterruptAudit{}
			c.audit = audit
			calls := 0
			c.persist = func() error {
				calls++
				failAt := 1
				if !alreadyTerminal {
					failAt = 2 // committing succeeds; terminal commit fails
				}
				if calls == failAt {
					return errors.New("injected journal failure")
				}
				return c.persistLocked()
			}
			r, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
			if r.Outcome != RemoteInterruptIndeterminate || r.ReasonCode != "internal_uncertain" {
				t.Fatal(r)
			}
			for _, got := range audit.records {
				if got.Outcome == RemoteInterruptInterrupted || got.Outcome == RemoteInterruptAlreadyTerminal {
					t.Fatalf("published definitive terminal audit: %+v", got)
				}
			}
			replay, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
			if replay != r {
				t.Fatalf("retained replay=%+v want=%+v", replay, r)
			}
			c, err = OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			reopened, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
			if reopened.Outcome != RemoteInterruptIndeterminate {
				t.Fatalf("reopened replay=%+v", reopened)
			}
		})
	}
}

func TestRemoteInterruptTerminalAuditFailuresPersistFailClosedReplay(t *testing.T) {
	for _, alreadyTerminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "already-terminal"}[alreadyTerminal], func(t *testing.T) {
			m, s, clock, op, req, target := interruptFixture(t)
			if alreadyTerminal {
				s.state = Idle
			}
			dir := filepath.Join(t.TempDir(), "ops")
			c, err := OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			failAt := 3
			if alreadyTerminal {
				failAt = 2
			}
			c.audit = &injectedInterruptAudit{failAt: failAt}
			r, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
			if r.Outcome != RemoteInterruptIndeterminate || r.ReasonCode != "internal_uncertain" {
				t.Fatal(r)
			}
			c, err = OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			replay, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
			if replay != r {
				t.Fatalf("reopened replay=%+v want=%+v", replay, r)
			}
		})
	}
}

func TestRemoteInterruptAuditRestartReadbackModeAndSymlink(t *testing.T) {
	m, _, clock, op, req, target := interruptFixture(t)
	dir := filepath.Join(t.TempDir(), "ops")
	c, err := OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
	p := filepath.Join(dir, "audit", remoteInterruptAuditFile)
	records, err := readRemoteInterruptAudit(p)
	if err != nil || len(records) != 3 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode=%v err=%v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(p)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode=%v err=%v", st, err)
	}
	if _, err := OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil); err != nil {
		t.Fatalf("restart: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "ops")
	if err := os.MkdirAll(filepath.Join(bad, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "victim"), filepath.Join(bad, "audit", remoteInterruptAuditFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRemoteInterruptCoordinator(m, clock.Now, bad, nil); err == nil {
		t.Fatal("accepted audit symlink")
	}
}

func TestRemoteInterruptConcurrentDuplicateIsAtMostOnce(t *testing.T) {
	m, _, clock, op, req, target := interruptFixture(t)
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	a := &fakeApprovalCanceller{result: ApprovalAbsent}
	i := &fakeExactInterrupter{result: ExactTurnInterrupted, entered: make(chan struct{}), release: make(chan struct{})}
	first := make(chan RemoteInterruptResult, 1)
	go func() { r, _ := c.Interrupt(context.Background(), op, req, target, a, i); first <- r }()
	<-i.entered
	second := make(chan RemoteInterruptResult, 1)
	go func() { r, _ := c.Interrupt(context.Background(), op, req, target, a, i); second <- r }()
	close(i.release)
	r1, r2 := <-first, <-second
	if r1.Outcome != RemoteInterruptInterrupted || r2 != r1 || a.calls != 1 || i.calls != 1 {
		t.Fatalf("first=%+v second=%+v calls=%d/%d", r1, r2, a.calls, i.calls)
	}
}

func TestRemoteInterruptExactTupleConflictExpiryAndCapacity(t *testing.T) {
	m, s, clock, op, req, target := interruptFixture(t)
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	a := &fakeApprovalCanceller{result: ApprovalAbsent}
	i := &fakeExactInterrupter{result: ExactTurnInterrupted}
	bad := target
	bad.TurnID = "successor"
	if r, _ := c.Interrupt(context.Background(), op, req, bad, a, i); r.ReasonCode != "stale_target" {
		t.Fatal(r)
	}
	expired := target
	expired.ExpiresAt = clock.Now()
	if r, _ := c.Interrupt(context.Background(), op, req, expired, a, i); r.ReasonCode != "target_expired" {
		t.Fatal(r)
	}
	if r, _ := c.Interrupt(context.Background(), op, req, target, a, i); r.Outcome != RemoteInterruptInterrupted {
		t.Fatal(r)
	}
	changed := req
	changed.ShipID = "other"
	if r, _ := c.Interrupt(context.Background(), op, changed, target, a, i); r.ReasonCode != "operation_conflict" {
		t.Fatal(r)
	}
	s.mu.Lock()
	s.state, s.turnID = Idle, ""
	s.mu.Unlock()
	clock.Advance(RemoteInterruptRetention)
	c.mu.Lock()
	c.expireLocked(clock.Now())
	c.mu.Unlock()
	if r, _ := c.Interrupt(context.Background(), op, req, target, a, i); r.ReasonCode != "dedup_expired" {
		t.Fatal(r)
	}

	m, _, clock, op, req, target = interruptFixture(t)
	c = NewRemoteInterruptCoordinator(m, clock.Now, nil)
	c.maxRecords = 0
	if r, _ := c.Interrupt(context.Background(), op, req, target, a, i); r.ReasonCode != "busy" {
		t.Fatal(r)
	}
}

// TestRemoteInterruptApprovalUncertaintyFailsClosed asserts the whole
// fail-closed contract, not just the state label. Unprovable approval
// cancellation must close the exact owner through Session.finish: the version
// that hand-set "s.state, s.shutting = Failed, true" produced the same
// snapshot while leaving the backend child executing the turn, the dispatch
// lock held, session.failed unpublished and Done() waiters blocked forever.
func TestRemoteInterruptApprovalUncertaintyFailsClosed(t *testing.T) {
	m, s, clock, op, req, target := interruptFixture(t)
	backend := &stubBackend{}
	released := make(chan struct{})
	s.adapter = backend
	s.release = func() { close(released) }
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	a := &fakeApprovalCanceller{result: ApprovalUnknown}
	i := &fakeExactInterrupter{result: ExactTurnInterrupted}
	r, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if r.Outcome != RemoteInterruptIndeterminate || r.ReasonCode != "approval_cancel_unknown" || i.calls != 0 {
		t.Fatalf("result=%+v interrupt calls=%d", r, i.calls)
	}
	assertFinishedClosed(t, s, backend, released)

	// The wedge the bypass caused: Shutdown saw Failed, treated the session as
	// already terminal and returned nil WITHOUT ever closing the adapter. It is
	// only correct for Shutdown to short-circuit because finish did the work.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after fail-closed interrupt: %v", err)
	}
	if got := backend.closes(); got != 1 {
		t.Fatalf("adapter closed %d times across teardown, want exactly 1", got)
	}
}

func TestRemoteInterruptExactRetainedTerminalIsRecordedWithoutEffects(t *testing.T) {
	m, s, clock, op, req, target := interruptFixture(t)
	s.state = Idle
	c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
	a := &fakeApprovalCanceller{result: ApprovalAbsent}
	i := &fakeExactInterrupter{result: ExactTurnInterrupted}
	r, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if r.Outcome != RemoteInterruptAlreadyTerminal || a.calls != 0 || i.calls != 0 {
		t.Fatalf("result=%+v calls=%d/%d", r, a.calls, i.calls)
	}
	replay, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if replay != r {
		t.Fatalf("replay=%+v want=%+v", replay, r)
	}
}

func TestRemoteInterruptAuditRedactionAndTerminalClassification(t *testing.T) {
	for _, tc := range []struct {
		decision ExactTurnInterruptOutcome
		want     RemoteInterruptOutcome
	}{{ExactTurnInterrupted, RemoteInterruptInterrupted}, {ExactTurnAlreadyTerminal, RemoteInterruptAlreadyTerminal}, {ExactTurnUnknown, RemoteInterruptIndeterminate}} {
		m, _, clock, op, req, target := interruptFixture(t)
		target.SessionID, target.ThreadID, target.TurnID = "SECRET_SESSION", "SECRET_THREAD", "SECRET_TURN"
		m.sessions["backend"].sessionID, m.sessions["backend"].threadID, m.sessions["backend"].turnID = target.SessionID, target.ThreadID, target.TurnID
		target.Reference = "SECRET_TARGET"
		req.TargetReference = target.Reference
		req.OperationNonce = "SECRET_NONCE"
		c := NewRemoteInterruptCoordinator(m, clock.Now, nil)
		r, audit := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: tc.decision})
		if r.Outcome != tc.want {
			t.Fatalf("decision=%s result=%+v", tc.decision, r)
		}
		b, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, secret := range []string{"SECRET_TARGET", "SECRET_NONCE", "SECRET_SESSION", "SECRET_THREAD", "SECRET_TURN"} {
			if strings.Contains(text, secret) {
				t.Fatalf("audit leaked %q: %s", secret, text)
			}
		}
	}
}

func TestRemoteInterruptBarrierAllowsDeterministicCompletionRace(t *testing.T) {
	m, s, clock, op, req, target := interruptFixture(t)
	reached, release := make(chan struct{}), make(chan struct{})
	c := NewRemoteInterruptCoordinator(m, clock.Now, func(stage string) {
		if stage == "before_validation" {
			close(reached)
			<-release
		}
	})
	done := make(chan RemoteInterruptResult, 1)
	go func() {
		r, _ := c.Interrupt(context.Background(), op, req, target, &fakeApprovalCanceller{result: ApprovalAbsent}, &fakeExactInterrupter{result: ExactTurnInterrupted})
		done <- r
	}()
	<-reached
	s.mu.Lock()
	s.state, s.turnID = Idle, ""
	s.mu.Unlock()
	close(release)
	if r := <-done; r.Outcome != RemoteInterruptRefused || r.ReasonCode != "stale_target" {
		t.Fatal(r)
	}
}

func TestRemoteInterruptDurableReplayAndRestartUncertainty(t *testing.T) {
	m, _, clock, op, req, target := interruptFixture(t)
	dir := filepath.Join(t.TempDir(), "ops")
	c, err := OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeApprovalCanceller{result: ApprovalAbsent}
	i := &fakeExactInterrupter{result: ExactTurnInterrupted}
	r, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if r.Outcome != RemoteInterruptInterrupted {
		t.Fatal(r)
	}
	c, err = OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, _ := c.Interrupt(context.Background(), op, req, target, a, i)
	if replay != r || i.calls != 1 {
		t.Fatalf("replay=%+v calls=%d", replay, i.calls)
	}

	// A journaled accepted operation is never invoked after reconstruction.
	m, _, clock, op, req, target = interruptFixture(t)
	dir = filepath.Join(t.TempDir(), "ops")
	c, err = OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := remoteInterruptFingerprint(op, req, target)
	c.mu.Lock()
	c.records[req.OperationID] = &remoteInterruptRecord{fingerprint: fp, request: req, target: target, operator: op, firstReceipt: clock.Now(), expiresAt: clock.Now().Add(RemoteInterruptRetention), committing: true, done: make(chan struct{})}
	c.nonces[req.OperationNonce] = req.OperationID
	if err = c.persistLocked(); err != nil {
		t.Fatal(err)
	}
	c.mu.Unlock()
	c, err = OpenRemoteInterruptCoordinator(m, clock.Now, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	i = &fakeExactInterrupter{result: ExactTurnInterrupted}
	replay, _ = c.Interrupt(context.Background(), op, req, target, a, i)
	if replay.Outcome != RemoteInterruptIndeterminate || replay.ReasonCode != "ship_restarted" || i.calls != 0 {
		t.Fatalf("replay=%+v calls=%d", replay, i.calls)
	}
}
