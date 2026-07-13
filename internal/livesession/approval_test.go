package livesession

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/policy"
)

type fakeApprovalClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []fakeWait
}
type fakeWait struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeApprovalClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeApprovalClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, fakeWait{c.now.Add(d), ch})
	return ch
}
func (c *fakeApprovalClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var fire []chan time.Time
	keep := c.waits[:0]
	for _, w := range c.waits {
		if !c.now.Before(w.at) {
			fire = append(fire, w.ch)
		} else {
			keep = append(keep, w)
		}
	}
	c.waits = keep
	now := c.now
	c.mu.Unlock()
	for _, ch := range fire {
		ch <- now
	}
}
func (c *fakeApprovalClock) Barrier(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		got := len(c.waits)
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("clock barrier: got %d timers, want %d", got, n)
		default:
		}
	}
}

type fakeApprovalAdapter struct {
	mu                 sync.Mutex
	calls              []ApprovalDecision
	entered            chan struct{}
	release            chan struct{}
	accept             *bool
	ignoreCancellation bool
}

func (a *fakeApprovalAdapter) ResolveApproval(ctx context.Context, _ AdapterApprovalRequest, d ApprovalDecision) (AdapterApprovalResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, d)
	if a.entered != nil {
		select {
		case a.entered <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	if a.release != nil {
		if a.ignoreCancellation {
			<-a.release
		} else {
			select {
			case <-a.release:
			case <-ctx.Done():
				return AdapterApprovalResult{}, ctx.Err()
			}
		}
	}
	accepted := true
	if a.accept != nil {
		accepted = *a.accept
	}
	return AdapterApprovalResult{Accepted: accepted}, nil
}
func (a *fakeApprovalAdapter) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.calls) }

func approvalFixture(t *testing.T) (*Manager, *Session, *fakeApprovalClock, ApprovalAuthority, NormalizedApprovalRequest, ApprovalEvaluation) {
	t.Helper()
	m, s := controllerTestManager()
	c := &fakeApprovalClock{now: time.Unix(100, 0)}
	m.now = c.Now
	m.after = c.After
	m.leaseDuration = time.Hour
	m.newApprovalID = func() (string, error) { return "approval-public", nil }
	s.state = Working
	s.turnID = "turn"
	s.policySnapshot = &policy.Snapshot{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	a, err := m.AttachController("backend", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := NormalizedApprovalRequest{Kind: policy.ProcessExec, CommandExact: "printf SECRET_COMMAND", ProjectRootID: "root-secret", SessionID: "session", ThreadID: "thread", TurnID: "turn", BackendRequestID: "backend-secret"}
	auth := ApprovalAuthority{Persona: "backend", SessionID: "session", Backend: s.snapshotLocked().Backend, ThreadID: "thread", TurnID: "turn", ControllerID: a.ControllerID, ControllerLeaseGeneration: a.ControllerLeaseGeneration, BackendRequestID: r.BackendRequestID}
	e := ApprovalEvaluation{Effect: policy.Ask, PolicySnapshotID: s.policySnapshot.ID, MatchedRules: []policy.MatchedRule{{Layer: policy.LayerPersona, Path: ".shipmates/policies/backend.yaml", ID: "safe.rule", Effect: policy.Ask}}}
	return m, s, c, auth, r, e
}

func TestApprovalFingerprintCanonicalAndSensitive(t *testing.T) {
	_, _, _, _, r, _ := approvalFixture(t)
	a, err := ApprovalRequestFingerprint(r)
	if err != nil || len(a) != 64 {
		t.Fatalf("fingerprint=%q %v", a, err)
	}
	r.CommandExact += " "
	b, _ := ApprovalRequestFingerprint(r)
	if a == b {
		t.Fatal("payload substitution retained fingerprint")
	}
}

func TestApprovalAllowOnceSingleWinnerAndRedactedEvents(t *testing.T) {
	m, s, _, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ResolveApproval(context.Background(), bound, AllowOnce)
	if err != nil || got.Outcome != "allowed_once" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	again, err := m.ResolveApproval(context.Background(), bound, AllowOnce)
	if err != nil || again != got {
		t.Fatalf("duplicate=%+v %v", again, err)
	}
	if adapter.count() != 1 {
		t.Fatalf("calls=%d", adapter.count())
	}
	raw, _ := json.Marshal(s.Feed(0))
	for _, secret := range []string{"SECRET_COMMAND", "root-secret", "backend-secret", "controller-one"} {
		if contains(string(raw), secret) {
			t.Fatalf("feed leaked %q: %s", secret, raw)
		}
	}
}

func TestApprovalPreCancelledAllowLeavesPendingAndCanRetry(t *testing.T) {
	m, s, _, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.ResolveApproval(ctx, bound, AllowOnce); err != context.Canceled {
		t.Fatalf("pre-cancelled allow = %v", err)
	}
	s.mu.Lock()
	pending := s.approval != nil && s.approval.authority == bound && s.approval.decision == "" && !s.approval.committed
	s.mu.Unlock()
	if !pending || adapter.count() != 0 {
		t.Fatalf("pending=%v calls=%d", pending, adapter.count())
	}
	if got, err := m.ResolveApproval(context.Background(), bound, AllowOnce); err != nil || got.Outcome != "allowed_once" {
		t.Fatalf("retry = %+v, %v", got, err)
	}
}

func TestApprovalCancellationRaceBeforeChoiceLeavesPending(t *testing.T) {
	m, s, _, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := m.ResolveApproval(ctx, bound, AllowOnce); done <- err }()
	// Resolve has passed its entry cancellation check and is now serialized on
	// the session decision lock.
	time.Sleep(time.Millisecond)
	cancel()
	s.mu.Unlock()
	if err = <-done; err != context.Canceled {
		t.Fatalf("racing cancellation = %v", err)
	}
	s.mu.Lock()
	pending := s.approval != nil && s.approval.decision == "" && !s.approval.committed
	s.mu.Unlock()
	if !pending || adapter.count() != 0 {
		t.Fatalf("pending=%v calls=%d", pending, adapter.count())
	}
}

func TestUnprovenAllowFailsExactSessionAndRetryDoesNotDispatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter func() *fakeApprovalAdapter
		timeout bool
	}{
		{name: "correlation", adapter: func() *fakeApprovalAdapter { accepted := false; return &fakeApprovalAdapter{accept: &accepted} }},
		{name: "deadline-late-write", adapter: func() *fakeApprovalAdapter {
			return &fakeApprovalAdapter{entered: make(chan struct{}, 1), release: make(chan struct{}), ignoreCancellation: true}
		}, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, clock, auth, r, e := approvalFixture(t)
			adapter := tc.adapter()
			bound, err := m.BeginApproval(r, e, auth, adapter)
			if err != nil {
				t.Fatal(err)
			}
			clock.Barrier(t, 2)
			done := make(chan error, 1)
			go func() { _, err := m.ResolveApproval(context.Background(), bound, AllowOnce); done <- err }()
			if tc.timeout {
				<-adapter.entered
				clock.Barrier(t, 3)
				clock.Advance(ApprovalResponseDeadline)
			}
			select {
			case err = <-done:
				if ErrorCode(err) != ResolutionFailed {
					t.Fatalf("resolution = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("unproven allow did not close")
			}
			if got := s.Snapshot(); got.State != Failed || got.FailureCode != codexapp.ProtocolViolation {
				t.Fatalf("snapshot = %+v", got)
			}
			if _, err = m.ResolveApproval(context.Background(), bound, AllowOnce); ErrorCode(err) != ResolutionFailed {
				t.Fatalf("retry = %v", err)
			}
			if adapter.count() != 1 {
				t.Fatalf("retry calls = %d", adapter.count())
			}
			if tc.timeout {
				close(adapter.release)
			}
		})
	}
}

func TestApprovalExactDeadlineAndDetachDeny(t *testing.T) {
	m, _, clock, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	clock.Barrier(t, 2)
	clock.Advance(ApprovalOperatorDeadline - time.Nanosecond)
	if _, err = m.ResolveApproval(context.Background(), bound, DenyOnce); err != nil {
		t.Fatal(err)
	}
	if adapter.count() != 1 {
		t.Fatalf("calls=%d", adapter.count())
	}
	m, _, clock, auth, r, e = approvalFixture(t)
	adapter = &fakeApprovalAdapter{entered: make(chan struct{}, 1)}
	bound, err = m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	clock.Barrier(t, 2)
	clock.Advance(ApprovalOperatorDeadline)
	<-adapter.entered
	deadline := time.After(time.Second)
	for adapter.count() != 1 {
		select {
		case <-deadline:
			t.Fatal("expiry hung")
		default:
		}
	}
	_ = bound
}

func TestApprovalLifecycleBeatsUncommittedAllow(t *testing.T) {
	m, _, _, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseController(auth.Persona, auth.SessionID, auth.ControllerID); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for adapter.count() != 1 {
		select {
		case <-deadline:
			t.Fatal("detach did not deny")
		default:
		}
	}
	if _, err = m.ResolveApproval(context.Background(), bound, AllowOnce); ErrorCode(err) != AlreadyResolved && ErrorCode(err) != StaleApproval {
		t.Fatalf("late allow=%v", err)
	}
}

func TestApprovalResponseDeadlineIsBounded(t *testing.T) {
	m, _, clock, auth, r, e := approvalFixture(t)
	adapter := &fakeApprovalAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	bound, err := m.BeginApproval(r, e, auth, adapter)
	if err != nil {
		t.Fatal(err)
	}
	clock.Barrier(t, 2)
	done := make(chan error, 1)
	go func() { _, err := m.ResolveApproval(context.Background(), bound, AllowOnce); done <- err }()
	<-adapter.entered
	clock.Barrier(t, 3)
	clock.Advance(ApprovalResponseDeadline)
	select {
	case err := <-done:
		if ErrorCode(err) != ResolutionFailed {
			t.Fatalf("resolution=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("adapter timeout left resolution blocked")
	}
	if adapter.count() != 1 {
		t.Fatalf("calls=%d", adapter.count())
	}
}

func TestPolicyDenyDoesNotDisplacePendingApproval(t *testing.T) {
	m, s, clock, auth, pendingRequest, ask := approvalFixture(t)
	pendingAdapter := &fakeApprovalAdapter{}
	bound, err := m.BeginApproval(pendingRequest, ask, auth, pendingAdapter)
	if err != nil {
		t.Fatal(err)
	}
	clock.Barrier(t, 2)

	denyRequest := pendingRequest
	denyRequest.BackendRequestID = "independent-deny"
	denyRequest.CommandExact = "rm harmless"
	denyAuth := auth
	denyAuth.BackendRequestID = denyRequest.BackendRequestID
	deny := ask
	deny.Effect = policy.Deny
	denyAdapter := &fakeApprovalAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	denied, err := m.BeginApproval(denyRequest, deny, denyAuth, denyAdapter)
	if err != nil {
		t.Fatal(err)
	}
	<-denyAdapter.entered

	s.mu.Lock()
	stillPending := s.approval != nil && s.approval.authority == bound
	s.mu.Unlock()
	if !stillPending {
		t.Fatal("concurrent policy deny displaced the existing pending approval")
	}
	if card := s.approvalCardLockedForTest(clock.Now()); card == nil || card.Authority != bound {
		t.Fatalf("pending card changed: %+v", card)
	}
	close(denyAdapter.release)
	deadline := time.After(time.Second)
	for {
		s.mu.Lock()
		rec, ok := s.approvalHistory[denied.BackendRequestID]
		s.mu.Unlock()
		if ok {
			if rec.result.Outcome != "denied" {
				t.Fatalf("deny result=%+v", rec.result)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("policy deny did not complete")
		default:
		}
	}
	if _, err := m.ResolveApproval(context.Background(), bound, AllowOnce); err != nil {
		t.Fatalf("preserved approval cannot resolve: %v", err)
	}
}

func (s *Session) approvalCardLockedForTest(now time.Time) *ApprovalCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approvalCardLocked(now)
}

func TestPolicyDenyIndependentTimeoutPreservesPending(t *testing.T) {
	m, s, clock, auth, r, ask := approvalFixture(t)
	bound, err := m.BeginApproval(r, ask, auth, &fakeApprovalAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	clock.Barrier(t, 2)
	dr, da := r, auth
	dr.BackendRequestID, dr.CommandExact = "deny-timeout", "blocked command"
	da.BackendRequestID = dr.BackendRequestID
	blocked := &fakeApprovalAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	deny := ask
	deny.Effect = policy.Deny
	if _, err = m.BeginApproval(dr, deny, da, blocked); err != nil {
		t.Fatal(err)
	}
	<-blocked.entered
	clock.Barrier(t, 3)
	clock.Advance(ApprovalResponseDeadline)
	deadline := time.After(time.Second)
	for {
		s.mu.Lock()
		rec, ok := s.approvalHistory[dr.BackendRequestID]
		preserved := s.approval != nil && s.approval.authority == bound
		s.mu.Unlock()
		if ok {
			if rec.result.Code != ResolutionFailed || !preserved {
				t.Fatalf("record=%+v preserved=%v", rec, preserved)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("deny timeout did not complete")
		default:
		}
	}
}

func TestApprovalSummaryRedactsCredentials(t *testing.T) {
	tests := []struct{ command, secret, want string }{
		{"deploy TOKEN=tok", "tok", "TOKEN=[redacted]"},
		{"deploy --password hunter2", "hunter2", "--password [redacted]"},
		{"deploy --api-key=keyvalue", "keyvalue", "--api-key=[redacted]"},
		{"curl -H Authorization: Bearer abc123", "abc123", "[redacted-header] [redacted]"},
		{"curl --header=Authorization: Bearer abc123", "abc123", "[redacted-header] [redacted]"},
		{"fetch https://user:pass@example.test", "user:pass", "https://[redacted]@example.test"},
		{"env AWS_SECRET_ACCESS_KEY=awsvalue run", "awsvalue", "AWS_SECRET_ACCESS_KEY=[redacted]"},
		{"deploy x-client-secret=shh", "shh", "x-client-secret=[redacted]"},
		{"TOKEN='two word secret' deploy", "two word secret", "TOKEN=[redacted]"},
		{"TOKEN=two\\ word\\ secret deploy", "two word secret", "TOKEN=[redacted]"},
		{"env TOKEN=\"two word secret\" deploy", "two word secret", "TOKEN=[redacted]"},
		{"curl -H 'Authorization: Bearer multi word secret' URL", "multi word secret", "[redacted-header]"},
		{"curl --header=\"Cookie: session=multi word secret\" URL", "multi word secret", "[redacted-header]"},
		{"curl -H 'Proxy-Authorization: Basic multi word secret' URL", "multi word secret", "[redacted-header]"},
		{"curl --header 'X-Api-Key: multi word secret' URL", "multi word secret", "[redacted-header]"},
		{"curl -H Cookie: session=secret next-leaks-too", "next-leaks-too", "[redacted-header] [redacted]"},
		{"fetch 'https://user:multi-word-pass@example.test/path'", "user:multi-word-pass", "https://[redacted]@example.test/path"},
		{"TOKEN='unterminated secret", "unterminated secret", "[redacted]"},
		{"TOKEN=trailing-secret\\", "trailing-secret", "[redacted]"},
	}
	for _, tt := range tests {
		got := approvalSummary(tt.command)
		if strings.Contains(got, tt.secret) || !strings.Contains(got, tt.want) {
			t.Fatalf("summary=%q secret=%q want=%q", got, tt.secret, tt.want)
		}
	}
}

func TestApprovalSummaryBoundedAfterQuotedSanitization(t *testing.T) {
	got := approvalSummary("run '" + strings.Repeat("safe ", 100) + "'")
	if len(got) > maxApprovalSummaryBytes || !utf8.ValidString(got) {
		t.Fatalf("unbounded or invalid summary: len=%d %q", len(got), got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
