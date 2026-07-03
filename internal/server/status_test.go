package server

import (
	"testing"
	"time"
)

func TestComputeStatus(t *testing.T) {
	now := time.Now()
	s := New()

	// working: live proc + recent event
	s.live["frontend"] = &liveProc{persona: "frontend"}
	s.lastSeen["frontend"] = now.Add(-5 * time.Second)

	// idle: live proc + stale event
	s.live["backend"] = &liveProc{persona: "backend"}
	s.lastSeen["backend"] = now.Add(-2 * time.Minute)

	// blocked: pending beats live
	s.live["security"] = &liveProc{persona: "security"}
	s.lastSeen["security"] = now.Add(-1 * time.Second)
	s.pendings["ab12"] = &pending{id: "ab12", persona: "security", tool: "Bash", input: "rm -rf /tmp/x"}

	// done: exited, no live proc
	s.exited["tester"] = true
	s.lastSeen["tester"] = now.Add(-10 * time.Minute)

	got := map[string]MateStatus{}
	for _, st := range s.computeStatus(now) {
		got[st.Persona] = st
	}

	if len(got) != 4 {
		t.Fatalf("want 4 personas, got %d: %v", len(got), got)
	}
	if got["frontend"].Status != "working" {
		t.Errorf("frontend: want working, got %s", got["frontend"].Status)
	}
	if got["backend"].Status != "idle" {
		t.Errorf("backend: want idle, got %s", got["backend"].Status)
	}
	if st := got["security"]; st.Status != "blocked" || st.Tool != "Bash" || st.PendingID != "ab12" || st.Input == "" {
		t.Errorf("security: want blocked/Bash/ab12 with input, got %+v", st)
	}
	if got["tester"].Status != "done" {
		t.Errorf("tester: want done, got %s", got["tester"].Status)
	}
}

func TestComputeStatusPendingDeterminism(t *testing.T) {
	now := time.Now()
	s := New()
	s.pendings["zz99"] = &pending{id: "zz99", persona: "security", tool: "Bash"}
	s.pendings["aa11"] = &pending{id: "aa11", persona: "security", tool: "PowerShell"}

	for i := 0; i < 10; i++ {
		out := s.computeStatus(now)
		if len(out) != 1 || out[0].PendingID != "aa11" {
			t.Fatalf("iteration %d: want stable pick aa11, got %+v", i, out)
		}
	}
}

func TestComputeStatusRespawnClearsDone(t *testing.T) {
	now := time.Now()
	s := New()
	s.exited["frontend"] = true
	s.lastSeen["frontend"] = now.Add(-1 * time.Hour)

	if out := s.computeStatus(now); out[0].Status != "done" {
		t.Fatalf("want done before respawn, got %+v", out)
	}

	// simulate ensureLive's resurrect
	s.live["frontend"] = &liveProc{persona: "frontend"}
	delete(s.exited, "frontend")
	s.lastSeen["frontend"] = now

	if out := s.computeStatus(now); out[0].Status != "working" {
		t.Fatalf("want working after respawn, got %+v", out)
	}
}
