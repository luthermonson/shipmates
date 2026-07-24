package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestMain doubles as a fake `claude` binary: when SHIPMATES_CLAUDE_FAKE=1
// the test executable pretends to be Claude Code — it drains stdin, emits a
// small stream-json transcript, lingers briefly (so interrupt/concurrency
// tests have a live process to hit), and exits 0.
func TestMain(m *testing.M) {
	if os.Getenv("SHIPMATES_CLAUDE_FAKE") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"fake hello"}]}}`)
		// Echo a requested environment variable back as a text frame so
		// tests can prove SessionSpec.Environment reached the child.
		if name := os.Getenv("SHIPMATES_CLAUDE_FAKE_ECHO_ENV"); name != "" {
			fmt.Printf("{\"type\":\"text\",\"text\":\"env %s=%s\"}\n", name, os.Getenv(name))
		}
		fmt.Println(`{"type":"result","subtype":"success"}`)
		if ms := os.Getenv("SHIPMATES_CLAUDE_FAKE_LINGER_MS"); ms != "" {
			var n int
			_, _ = fmt.Sscanf(ms, "%d", &n)
			time.Sleep(time.Duration(n) * time.Millisecond)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeClaudeRuntime returns a Runtime whose binary is this test executable
// in fake-claude mode.
func fakeClaudeRuntime(t *testing.T, lingerMS int) *Runtime {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIPMATES_CLAUDE_FAKE", "1")
	t.Setenv("SHIPMATES_CLAUDE_FAKE_LINGER_MS", fmt.Sprintf("%d", lingerMS))
	return New(Config{Binary: exe})
}

func startSession(t *testing.T, rt *Runtime) runtime.Session {
	t.Helper()
	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{
		Persona:    "tester",
		ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSendTurn_RefusesConcurrentTurn verifies a second SendTurn on a session
// with a live turn errors instead of silently orphaning the prior process.
func TestSendTurn_RefusesConcurrentTurn(t *testing.T) {
	rt := fakeClaudeRuntime(t, 3000)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("first SendTurn: %v", err)
	}
	_, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"})
	if err == nil {
		t.Fatal("second SendTurn succeeded; expected active-turn refusal")
	}
	if !strings.Contains(err.Error(), "already has an active turn") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSendTurn_AllowsNextTurnAfterCompletion verifies the turn slot is
// released once a turn's fanout finishes.
func TestSendTurn_AllowsNextTurnAfterCompletion(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("first SendTurn: %v", err)
	}
	// Drain until the first turn's terminal event arrives.
	waitTurnDone(t, rt)

	deadline := time.After(10 * time.Second)
	for {
		_, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "already has an active turn") {
			t.Fatalf("second SendTurn: %v", err)
		}
		// The slot is released just after the terminal event is emitted;
		// retry briefly.
		select {
		case <-deadline:
			t.Fatal("turn slot never released after completion")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitTurnDone(t *testing.T, rt *Runtime) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("event stream closed before turn completion")
			}
			if ev.Kind == runtime.KindTurnDone || ev.Kind == runtime.KindError {
				return
			}
		case <-timeout:
			t.Fatal("no terminal event within 15s")
		}
	}
}

// TestConcurrentSendTurnAndInterrupt exercises the locking under the race
// detector: many goroutines hammer SendTurn and InterruptTurn on the same
// session while a consumer drains events.
func TestConcurrentSendTurnAndInterrupt(t *testing.T) {
	rt := fakeClaudeRuntime(t, 500)
	s := startSession(t, rt)

	done := make(chan struct{})
	go func() { // drain events so fanouts never block
		for range rt.Events() {
		}
		close(done)
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "spin"})
				time.Sleep(10 * time.Millisecond)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = rt.InterruptTurn(context.Background(), s.ID(), "")
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream not closed after Close — consumers would hang")
	}
}

// TestClose_ClosesEventStream verifies Close ends a `for range Events()`
// loop even when a turn is mid-flight.
func TestClose_ClosesEventStream(t *testing.T) {
	rt := fakeClaudeRuntime(t, 3000)
	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	terminated := make(chan struct{})
	go func() {
		for range rt.Events() {
		}
		close(terminated)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("Events() still open after Close")
	}
	// Idempotent.
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestSendTurn_AppliesSessionEnvironment verifies SessionSpec.Environment
// (and the SHIPMATES_PERSONA export) reach the spawned process.
func TestSendTurn_AppliesSessionEnvironment(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ENV", "SHIPMATES_TEST_MARKER")

	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{
		Persona:     "tester",
		ProjectDir:  t.TempDir(),
		Environment: map[string]string{"SHIPMATES_TEST_MARKER": "spec-env-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "go"}); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(15 * time.Second)
	var sawEnv bool
	for !sawEnv {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before env echo")
			}
			if ev.Kind == runtime.KindText {
				if s, _ := ev.Payload.(string); strings.Contains(s, "SHIPMATES_TEST_MARKER=spec-env-value") {
					sawEnv = true
				}
			}
			if ev.Kind == runtime.KindTurnDone || ev.Kind == runtime.KindError {
				if !sawEnv {
					t.Fatalf("turn ended without env echo (last event %v)", ev.Kind)
				}
			}
		case <-timeout:
			t.Fatal("no env echo within 15s")
		}
	}
}

// TestSendTurn_AfterCloseFails verifies a turn cannot slip past Close.
func TestSendTurn_AfterCloseFails(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	s := startSession(t, rt)
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "late"}); err == nil {
		t.Fatal("SendTurn after Close succeeded")
	}
}
