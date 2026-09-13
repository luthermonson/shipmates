package server

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain doubles as a fake `claude -p` stream-json child so the spawn/pipe
// lifecycle in spawnCrewLive can be exercised without the real binary. When
// SHIPMATES_CLAUDE_FAKE=1 the test executable re-execs into runFakeClaude and
// never runs the suite; otherwise it runs the tests normally.
func TestMain(m *testing.M) {
	if os.Getenv("SHIPMATES_CLAUDE_FAKE") == "1" {
		runFakeClaude()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeClaude mimics the crew process the server spawns: it emits one
// assistant text frame (carrying the model) and then a terminal `result` frame
// (carrying cost/duration) on stdout, and exits immediately afterward. Exiting
// the instant the terminal frame is written is exactly the teardown the fix
// guards: cmd.Wait must not close the stdout pipe before pump has decoded that
// final frame.
//
// Knobs (env vars):
//
//	SHIPMATES_FAKE_STARTUP_MS  sleep this long before emitting anything, so the
//	                           process outlives spawnCrewLive's resume-path
//	                           startup window and pump is reading stdout when the
//	                           terminal frame lands (the original #41 race).
func runFakeClaude() {
	if ms := os.Getenv("SHIPMATES_FAKE_STARTUP_MS"); ms != "" {
		var n int
		_, _ = fmt.Sscanf(ms, "%d", &n)
		time.Sleep(time.Duration(n) * time.Millisecond)
	}
	emit := func(v any) {
		buf, _ := json.Marshal(v)
		fmt.Println(string(buf))
	}
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":   "claude-fake-1",
			"content": []map[string]any{{"type": "text", "text": "fake hello"}},
		},
	})
	emit(map[string]any{
		"type":           "result",
		"subtype":        "success",
		"is_error":       false,
		"total_cost_usd": 0.0042,
		"duration_ms":    1234,
	})
}

// armFakeClaude points spawnCrewLive at this test executable in fake-claude
// mode. The env var is inherited by the spawned child, which re-execs into
// runFakeClaude via TestMain.
func armFakeClaude(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := claudeBinary
	claudeBinary = exe
	t.Cleanup(func() { claudeBinary = prev })
	t.Setenv("SHIPMATES_CLAUDE_FAKE", "1")
}

// spawnLive runs spawnCrewLive under the caller-holds-s.mu contract and returns
// the live proc.
func spawnLive(t *testing.T, s *Server, persona string, fresh bool) *liveProc {
	t.Helper()
	s.mu.Lock()
	lp, err := s.spawnCrewLive(persona, fresh)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("spawnCrewLive(%q, fresh=%v): %v", persona, fresh, err)
	}
	if lp == nil {
		t.Fatal("spawnCrewLive returned a nil liveProc without an error")
	}
	return lp
}

// waitClosed blocks until c is closed or the deadline expires, failing the test
// on timeout with the given label.
func waitClosed(t *testing.T, c <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(d):
		t.Fatalf("timed out after %s waiting for %s", d, what)
	}
}

// resultEvent returns the first "result" event for persona in the log, or a
// zero Event and false.
func resultEvent(s *Server, persona string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Persona == persona && e.Type == "result" {
			return e, true
		}
	}
	return Event{}, false
}

// TestSpawnCrewLiveDeliversFinalResultFrame is the twin of issue #41 in the
// server: when the crew process writes its terminal `result` frame and exits
// immediately, cmd.Wait races pump's decode. With os/exec owning the stdout
// pipe (the old code), Wait closed it the instant the child was reaped and the
// final frame — the one carrying cost and model — was dropped. The fix owns the
// pipe and drains before closing, so the frame must always arrive.
func TestSpawnCrewLiveDeliversFinalResultFrame(t *testing.T) {
	s, _ := newTestServer(t)
	armFakeClaude(t)
	writePersona(t, "tester", "name: tester")

	lp := spawnLive(t, s, "tester", true)

	// pump has drained stdout once stdoutDone closes.
	waitClosed(t, lp.stdoutDone, 20*time.Second, "pump to finish draining stdout")

	ev, ok := resultEvent(s, "tester")
	if !ok {
		t.Fatalf("final result frame was dropped: no result event delivered")
	}
	if ev.CostUSD != 0.0042 {
		t.Errorf("result event lost its cost: got %v, want 0.0042", ev.CostUSD)
	}
	if ev.DurationMS != 1234 {
		t.Errorf("result event lost its duration: got %v, want 1234", ev.DurationMS)
	}
	if ev.Model != "claude-fake-1" {
		t.Errorf("result event lost its model: got %q, want claude-fake-1", ev.Model)
	}
}

// TestSpawnCrewLiveReapsFreshChild pins the second defect: the old code called
// cmd.Wait only on the resume path, so every fresh mate spawn was never reaped
// — a zombie on Unix / a leaked handle on Windows. reapLive now Waits on both
// paths; ProcessState is non-nil only after a successful Wait.
func TestSpawnCrewLiveReapsFreshChild(t *testing.T) {
	s, _ := newTestServer(t)
	armFakeClaude(t)
	writePersona(t, "tester", "name: tester")

	lp := spawnLive(t, s, "tester", true)

	waitClosed(t, lp.procDone, 20*time.Second, "the fresh child to be reaped")
	if lp.cmd.ProcessState == nil {
		t.Fatal("fresh spawn was never reaped: cmd.ProcessState is nil (zombie/leaked handle)")
	}
	if !lp.cmd.ProcessState.Exited() {
		t.Errorf("reaped process did not exit cleanly: %v", lp.cmd.ProcessState)
	}
}

// TestSpawnCrewLiveResumePathDeliversFinalFrame drives the exact path the bug
// lived on: fresh=false, where pump starts only after the startup window and is
// mid-stream when the child emits its terminal frame and exits. The fake sleeps
// past a shortened startup window, then emits and exits, so Wait and pump race
// on a live resume session.
func TestSpawnCrewLiveResumePathDeliversFinalFrame(t *testing.T) {
	s, _ := newTestServer(t)
	armFakeClaude(t)
	writePersona(t, "tester", "name: tester")

	// Survive a shrunken startup window, then race Wait against pump.
	t.Setenv("SHIPMATES_FAKE_STARTUP_MS", "400")
	prev := liveStartupWindow
	liveStartupWindow = 150 * time.Millisecond
	t.Cleanup(func() { liveStartupWindow = prev })

	lp := spawnLive(t, s, "tester", false)

	waitClosed(t, lp.stdoutDone, 20*time.Second, "pump to finish draining stdout")
	if _, ok := resultEvent(s, "tester"); !ok {
		t.Fatal("resume-path final result frame was dropped")
	}
	waitClosed(t, lp.procDone, 20*time.Second, "the resumed child to be reaped")
	if lp.cmd.ProcessState == nil {
		t.Fatal("resumed child was never reaped")
	}
}
