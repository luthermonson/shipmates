package livesession

import (
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
)

func controllerTestManager() (*Manager, *Session) {
	m := New(nil, structAdapterOptions())
	m.newControllerID = func() (string, error) { return "controller-one", nil }
	s := &Session{persona: "backend", sessionID: "session", threadID: "thread", state: Idle, done: make(chan struct{}), nextSequence: 1, notify: make(chan struct{}, 1)}
	s.publishLocked("session.ready", "thread", "", nil)
	m.sessions["backend"] = s
	return m, s
}

func TestControllerLeaseExpiryUsesInjectedMonotonicTime(t *testing.T) {
	m, _ := controllerTestManager()
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	m.leaseDuration = 10 * time.Second
	a, err := m.AttachController("backend", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Second)
	if err = m.HeartbeatController("backend", "session", a.ControllerID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second)
	if err = m.HeartbeatController("backend", "session", a.ControllerID); ErrorCode(err) != StaleController {
		t.Fatalf("expired=%v", err)
	}
	m.newControllerID = func() (string, error) { return "replacement", nil }
	if _, err = m.AttachController("backend", "session", 0); err != nil {
		t.Fatal(err)
	}
}

func TestReclaimExpiredControllerIsSingleBoundedTransition(t *testing.T) {
	m, _ := controllerTestManager()
	now := time.Unix(100, 0)
	m.now = func() time.Time { return now }
	m.leaseDuration = 10 * time.Second
	m.newControllerID = func() (string, error) { return "expired-controller", nil }
	first, err := m.AttachController("backend", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	claimed, err := m.ReclaimExpiredController("backend", first.SessionID)
	if err != nil || !claimed {
		t.Fatalf("reclaim=%v err=%v", claimed, err)
	}
	claimed, err = m.ReclaimExpiredController("backend", first.SessionID)
	if err != nil || claimed {
		t.Fatalf("repeated reclaim=%v err=%v", claimed, err)
	}
	m.newControllerID = func() (string, error) { return "replacement-controller", nil }
	replacement, err := m.AttachController("backend", first.SessionID, 0)
	if err != nil || replacement.ControllerID != "replacement-controller" || replacement.ControllerLeaseGeneration != first.ControllerLeaseGeneration+1 {
		t.Fatalf("replacement=%+v err=%v", replacement, err)
	}
}

func TestReclaimLiveConcurrentAndStaleControllerBoundaries(t *testing.T) {
	m, _ := controllerTestManager()
	now := time.Unix(200, 0)
	m.now = func() time.Time { return now }
	m.leaseDuration = time.Hour
	first, err := m.AttachController("backend", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := m.ReclaimExpiredController("backend", first.SessionID); err != nil || reclaimed {
		t.Fatalf("live reclaim=%v err=%v", reclaimed, err)
	}
	if _, err := m.AttachController("backend", first.SessionID, 0); ErrorCode(err) != AlreadyOpen {
		t.Fatalf("live attach error=%v", err)
	}
	if reclaimed, err := m.ReclaimExpiredController("backend", "other-session"); ErrorCode(err) != StaleTarget || reclaimed {
		t.Fatalf("stale reclaim=%v err=%v", reclaimed, err)
	}
	// Concurrent rechecks cannot both reclaim or create a replacement.
	now = now.Add(time.Hour)
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() { reclaimed, _ := m.ReclaimExpiredController("backend", first.SessionID); results <- reclaimed }()
	}
	a, b := <-results, <-results
	if a == b {
		t.Fatalf("concurrent reclaim results=%v,%v, want exactly one", a, b)
	}
}

// Kept local to avoid coupling these authority tests to a subprocess fixture.
func structAdapterOptions() (z codexapp.StartOptions) { return }

func TestExclusiveControllerAndObserverIsolation(t *testing.T) {
	m, s := controllerTestManager()
	a, err := m.AttachController("backend", "session", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.ControllerID != "controller-one" || a.State != Idle || len(a.Events) != 1 {
		t.Fatalf("attach=%+v", a)
	}
	if _, err = m.AttachController("backend", "session", 0); ErrorCode(err) != AlreadyOpen {
		t.Fatalf("second=%v", err)
	}
	// A read-only observer neither needs nor changes the controller lease.
	if got := s.Feed(0); len(got.Events) != 1 {
		t.Fatalf("feed=%+v", got)
	}
	if s.controllerID != "controller-one" {
		t.Fatal("observer changed lease")
	}
}

func TestControllerMessageAllowsMultilineAdviceButRejectsControlBytes(t *testing.T) {
	for _, valid := range []string{"one line", "architect advice:\n- option one\n- option two", "tab\tvalue"} {
		if !validControllerMessage(valid) {
			t.Fatalf("valid controller message rejected: %q", valid)
		}
	}
	for _, invalid := range []string{"", "nul\x00byte", "carriage\rreturn", string([]byte{0xff})} {
		if validControllerMessage(invalid) {
			t.Fatalf("invalid controller message accepted: %q", invalid)
		}
	}
}
func TestReleaseIsExactAndReconnectGetsFreshAuthority(t *testing.T) {
	m, s := controllerTestManager()
	a, _ := m.AttachController("backend", "session", 0)
	if err := m.ReleaseController("backend", "session", "wrong"); ErrorCode(err) != StaleController {
		t.Fatalf("wrong=%v", err)
	}
	if s.controllerID != a.ControllerID {
		t.Fatal("stale release revoked lease")
	}
	if err := m.ReleaseController("backend", "session", a.ControllerID); err != nil {
		t.Fatal(err)
	}
	m.newControllerID = func() (string, error) { return "controller-two", nil }
	b, err := m.AttachController("backend", "session", a.NextSequence-1)
	if err != nil {
		t.Fatal(err)
	}
	if b.ControllerID == a.ControllerID {
		t.Fatal("controller credential reused")
	}
}
