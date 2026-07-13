package livesession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/project"
)

type steerFakeClock struct {
	mu sync.Mutex
	at time.Time
}

func TestRemoteSteerDurableTerminalReplayAndRestartUncertainty(t *testing.T) {
	for _, decision := range []RemoteSteerDecision{{RemoteSteerAccepted, ""}, {RemoteSteerRefused, "not_steerable"}, {RemoteSteerIndeterminate, "ack_lost"}} {
		t.Run(string(decision.Outcome), func(t *testing.T) {
			m, _, clock, op, req, target := remoteFixture(t)
			dir := filepath.Join(t.TempDir(), "ops")
			c, err := OpenRemoteSteerCoordinator(m, clock.Now, dir)
			if err != nil {
				t.Fatal(err)
			}
			fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
			fake.release <- decision
			want, _ := c.Steer(context.Background(), op, req, target, fake)
			c, err = OpenRemoteSteerCoordinator(m, clock.Now, dir)
			if err != nil {
				t.Fatal(err)
			}
			replay, _ := c.Steer(context.Background(), op, req, target, fake)
			if replay != want || fake.calls != 1 {
				t.Fatalf("replay=%+v want=%+v calls=%d", replay, want, fake.calls)
			}
		})
	}

	m, _, clock, op, req, target := remoteFixture(t)
	dir := filepath.Join(t.TempDir(), "ops")
	c, err := OpenRemoteSteerCoordinator(m, clock.Now, dir)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	fp := remoteSteerFingerprint(op, req, target)
	c.records[req.OperationID] = &remoteSteerRecord{fingerprint: fp, request: req, target: target, operator: op, firstReceipt: clock.Now(), expiresAt: clock.Now().Add(RemoteSteerRetention), committing: true, done: make(chan struct{})}
	c.nonces[req.OperationNonce] = req.OperationID
	err = c.persistLocked()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c, err = OpenRemoteSteerCoordinator(m, clock.Now, dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	r, _ := c.Steer(context.Background(), op, req, target, fake)
	if r.Outcome != RemoteSteerIndeterminate || r.ReasonCode != "fleet_restarted" || fake.calls != 0 {
		t.Fatalf("restart=%+v calls=%d", r, fake.calls)
	}
}

func TestRemoteSteerDurableConflictTombstoneAndStorageFailure(t *testing.T) {
	m, s, clock, op, req, target := remoteFixture(t)
	dir := filepath.Join(t.TempDir(), "ops")
	c, err := OpenRemoteSteerCoordinator(m, clock.Now, dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	fake.release <- RemoteSteerDecision{RemoteSteerAccepted, ""}
	if r, _ := c.Steer(context.Background(), op, req, target, fake); r.Outcome != RemoteSteerAccepted {
		t.Fatal(r)
	}
	c2, err := OpenRemoteSteerCoordinator(m, clock.Now, dir)
	if err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.Message = "changed"
	if r, _ := c2.Steer(context.Background(), op, changed, target, fake); r.ReasonCode != "operation_conflict" {
		t.Fatalf("conflict=%+v", r)
	}
	s.mu.Lock()
	s.state, s.turnID = Idle, ""
	s.mu.Unlock()
	clock.Advance(2 * RemoteSteerRetention)
	c2.mu.Lock()
	c2.expireLocked(clock.Now())
	c2.mu.Unlock()
	clock.Advance(RemoteSteerRetention)
	c2.mu.Lock()
	c2.expireLocked(clock.Now())
	c2.mu.Unlock()
	c2, err = OpenRemoteSteerCoordinator(m, clock.Now, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := c2.Steer(context.Background(), op, req, target, fake); r.ReasonCode != "dedup_expired" {
		t.Fatalf("tomb=%+v", r)
	}

	m, s, clock, op, req, target = remoteFixture(t)
	c = NewRemoteSteerCoordinator(m, clock.Now)
	parent := filepath.Join(t.TempDir(), "file")
	if err = os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.storeDir = filepath.Join(parent, "ops")
	if r, _ := c.Steer(context.Background(), op, req, target, fake); r.Outcome != RemoteSteerRefused || r.ReasonCode != "shutdown" {
		t.Fatalf("failure=%+v", r)
	}
	if fake.calls != 1 {
		t.Fatalf("execution occurred after failed commit: calls=%d", fake.calls)
	}
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != Working {
		t.Fatalf("state=%s", state)
	}
}

func (c *steerFakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *steerFakeClock) Advance(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

type barrierSteerer struct {
	entered chan struct{}
	release chan RemoteSteerDecision
	mu      sync.Mutex
	calls   int
}

func (f *barrierSteerer) SteerExactTurn(_ context.Context, sid, tid, turn, message string) RemoteSteerDecision {
	if sid != "session" || tid != "thread" || turn != "turn" || message != "hello" {
		return RemoteSteerDecision{RemoteSteerRefused, "stale_target"}
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	return <-f.release
}

func remoteFixture(t *testing.T) (*Manager, *Session, *steerFakeClock, RemoteSteerOperator, RemoteSteerRequest, RemoteSteerTarget) {
	t.Helper()
	clock := &steerFakeClock{at: time.Unix(1000, 0)}
	m := &Manager{sessions: make(map[string]*Session)}
	s := &Session{persona: "backend", sessionID: "session", threadID: "thread", turnID: "turn", state: Working}
	m.sessions["backend"] = s
	op := RemoteSteerOperator{SubjectID: "subject", CredentialID: "credential", CredentialGeneration: 7, FleetID: "fleet", Capability: RemoteSteerCapability}
	d := sha256.Sum256([]byte("hello"))
	req := RemoteSteerRequest{ProtocolVersion: 1, OperationID: "operation-00000001", OperationNonce: "nonce-0000000000000001", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", TargetReference: "target", Message: "hello", MessageSHA256: hex.EncodeToString(d[:]), MessageUTF8Bytes: 5}
	target := RemoteSteerTarget{Reference: "target", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: "session", Backend: project.CodexAppServerBackend, ThreadID: "thread", TurnID: "turn", ExpiresAt: clock.Now().Add(time.Minute)}
	return m, s, clock, op, req, target
}

func TestRemoteSteerExactTupleM6AndExpiryRefusal(t *testing.T) {
	m, s, clock, op, req, target := remoteFixture(t)
	c := NewRemoteSteerCoordinator(m, clock.Now)
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	s.approval = &pendingApproval{}
	if r, _ := c.Steer(context.Background(), op, req, target, fake); r.ReasonCode != "approval_pending" {
		t.Fatalf("pending=%+v", r)
	}
	s.approval = nil
	bad := target
	bad.TurnID = "successor"
	if r, _ := c.Steer(context.Background(), op, req, bad, fake); r.ReasonCode != "stale_target" {
		t.Fatalf("tuple=%+v", r)
	}
	clock.Advance(time.Minute)
	if r, _ := c.Steer(context.Background(), op, req, target, fake); r.ReasonCode != "target_expired" {
		t.Fatalf("expiry=%+v", r)
	}
	if fake.calls != 0 {
		t.Fatalf("adapter calls=%d", fake.calls)
	}
}

func TestRemoteSteerAtMostOnceDuplicateAndConflict(t *testing.T) {
	m, _, clock, op, req, target := remoteFixture(t)
	c := NewRemoteSteerCoordinator(m, clock.Now)
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	type pair struct {
		r RemoteSteerResult
		a RemoteSteerAuditCandidate
	}
	first := make(chan pair, 1)
	go func() { r, a := c.Steer(context.Background(), op, req, target, fake); first <- pair{r, a} }()
	<-fake.entered
	duplicate := make(chan RemoteSteerResult, 1)
	go func() { r, _ := c.Steer(context.Background(), op, req, target, fake); duplicate <- r }()
	conflict := req
	conflict.Message = "changed"
	if r, _ := c.Steer(context.Background(), op, conflict, target, fake); r.ReasonCode != "operation_conflict" {
		t.Fatalf("conflict=%+v", r)
	}
	fake.release <- RemoteSteerDecision{Outcome: RemoteSteerAccepted}
	a, b := (<-first), (<-duplicate)
	if a.r.Outcome != RemoteSteerAccepted || b != a.r || a.a.Outcome != RemoteSteerAccepted {
		t.Fatalf("results=%+v %+v", a, b)
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if a.a.TargetHash == target.Reference || a.a.MessageSHA256 == req.Message || a.a.Layer != "ship" {
		t.Fatalf("audit=%+v", a.a)
	}
}

func TestRemoteSteerSharesLocalSerializationAndBoundedDedup(t *testing.T) {
	m, s, clock, op, req, target := remoteFixture(t)
	c := NewRemoteSteerCoordinator(m, clock.Now)
	c.maxRecords = 1
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	done := make(chan RemoteSteerResult, 1)
	go func() { r, _ := c.Steer(context.Background(), op, req, target, fake); done <- r }()
	<-fake.entered
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	if state != Steering {
		t.Fatalf("state=%s", state)
	}
	if _, err := m.Tell(context.Background(), "backend", "session", "thread", "turn", "local"); ErrorCode(err) != Busy {
		t.Fatalf("local race=%v", err)
	}
	req2 := req
	req2.OperationID, req2.OperationNonce = "operation-2", "nonce-2"
	if r, _ := c.Steer(context.Background(), op, req2, target, fake); r.ReasonCode != "busy" {
		t.Fatalf("capacity=%+v", r)
	}
	fake.release <- RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "not_steerable"}
	if r := <-done; r.Outcome != RemoteSteerRefused {
		t.Fatalf("result=%+v", r)
	}
}

func TestRemoteSteerCoordinatorUsesRealManagerExactTurnPrimitive(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-remote-real", "SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record")}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()
	now := time.Unix(2000, 0)
	target := RemoteSteerTarget{Reference: "target-real", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", SessionID: snap.SessionID, Backend: snap.Backend, ThreadID: snap.ThreadID, TurnID: snap.TurnID, ExpiresAt: now.Add(time.Minute)}
	op := RemoteSteerOperator{SubjectID: "subject", CredentialID: "credential", CredentialGeneration: 7, FleetID: "fleet", Capability: RemoteSteerCapability}
	digest := sha256.Sum256([]byte("remote real"))
	req := RemoteSteerRequest{ProtocolVersion: 1, OperationID: "operation-real-0001", OperationNonce: "nonce-real-000000001", FleetID: "fleet", FleetEpoch: 2, ShipID: "ship", ConnectionGeneration: 3, Persona: "backend", TargetReference: target.Reference, Message: "remote real", MessageSHA256: hex.EncodeToString(digest[:]), MessageUTF8Bytes: 11}
	c := NewRemoteSteerCoordinator(m, func() time.Time { return now })
	got, _ := c.Steer(context.Background(), op, req, target, m)
	if got.Outcome != RemoteSteerAccepted {
		t.Fatalf("result=%+v", got)
	}
	if replay, _ := c.Steer(context.Background(), op, req, target, m); replay != got {
		t.Fatalf("replay=%+v want=%+v", replay, got)
	}
}

func TestRemoteSteerRetentionTombstoneAndIndeterminate(t *testing.T) {
	m, s, clock, op, req, target := remoteFixture(t)
	c := NewRemoteSteerCoordinator(m, clock.Now)
	fake := &barrierSteerer{entered: make(chan struct{}, 1), release: make(chan RemoteSteerDecision, 1)}
	fake.release <- RemoteSteerDecision{Outcome: RemoteSteerIndeterminate, ReasonCode: "ack_lost"}
	r, _ := c.Steer(context.Background(), op, req, target, fake)
	if r.Outcome != RemoteSteerIndeterminate || r.RetryDisposition != RemoteSteerReplaySameOperation {
		t.Fatalf("result=%+v", r)
	}
	s.mu.Lock()
	s.state, s.turnID = Idle, ""
	s.mu.Unlock()
	clock.Advance(RemoteSteerRetention)
	if retained, _ := c.Steer(context.Background(), op, req, target, fake); retained.Outcome != RemoteSteerIndeterminate {
		t.Fatalf("terminal retention=%+v", retained)
	}
	clock.Advance(RemoteSteerRetention)
	if expired, _ := c.Steer(context.Background(), op, req, target, fake); expired.ReasonCode != "dedup_expired" || expired.RetryDisposition != RemoteSteerFreshObservation {
		t.Fatalf("expired=%+v", expired)
	}
}
