package livesession

import (
	"context"
	"testing"
)

// TestSteerExactTurnDuplicateIdentityRefuses covers the duplicate-identity
// branch of SteerExactTurn. That branch used to unlock m.mu a second time —
// the mutex is already released before the candidate scan — which is an
// unrecoverable runtime throw, not a recoverable panic.
func TestSteerExactTurnDuplicateIdentityRefuses(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{
		"a": {sessionID: "sid", threadID: "tid", turnID: "turn"},
		"b": {sessionID: "sid", threadID: "tid", turnID: "turn"},
	}}
	d := m.SteerExactTurn(context.Background(), "sid", "tid", "turn", "hello")
	if d.Outcome != RemoteSteerRefused || d.ReasonCode != "stale_target" {
		t.Fatalf("decision=%+v, want RemoteSteerRefused/stale_target", d)
	}
}
