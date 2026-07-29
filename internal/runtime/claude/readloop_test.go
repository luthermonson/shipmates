package claude

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestReadLoopOversizedFrameSurfacesErrorAndDoesNotStrandClose covers the
// swallowed scanner.Err(). readLoop scanned with a bounded token size and never
// checked the error afterwards, so a frame larger than the buffer ended the
// loop while the child was still alive and still writing: truncation was
// indistinguishable from a clean exit, the goroutine parked on
// <-p.handle.Done() waiting for a death that had not happened, and Close blocked
// forever on fanWG.Wait().
func TestReadLoopOversizedFrameSurfacesErrorAndDoesNotStrandClose(t *testing.T) {
	// A turn long enough that the child is unambiguously still running when the
	// oversized frame breaks framing.
	rt := fakeClaudeRuntime(t, 60000)
	rt.maxFrameBytes = 4096
	t.Setenv("SHIPMATES_CLAUDE_FAKE_HUGE_FRAME", "32768")
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	var payload string
	deadline := time.After(20 * time.Second)
	for payload == "" {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("event stream closed before the framing failure was reported")
			}
			if ev.Kind != runtime.KindError {
				continue
			}
			if ev.TurnID != turn.ID() {
				t.Errorf("framing failure attributed to turn %q, want %q", ev.TurnID, turn.ID())
			}
			payload = fmt.Sprintf("%v", ev.Payload)
		case <-deadline:
			t.Fatal("no KindError for the oversized frame within 20s: truncation is still indistinguishable from a clean exit")
		}
	}
	if !strings.Contains(payload, "framing failed") || !strings.Contains(payload, "token too long") {
		t.Errorf("error payload does not name the cause: %q", payload)
	}

	// The turn slot must come back — a desynced stream can never produce the
	// turn's result frame, so nothing else will release it.
	closed := make(chan error, 1)
	go func() { closed <- rt.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close stranded on fanWG.Wait(): readLoop is still waiting for a terminal event")
	}
	// Close closes the stream exactly once; ranging over it must terminate.
	for range rt.Events() {
	}
}

// TestSpawnCapturesChildStderr covers the discarded cmd.Stderr. Claude Code
// reports auth failures ("Invalid API key · Please run /login"), a missing
// config and crash detail on stderr; unwired, all of them looked like a generic
// timeout with an exit code and no explanation.
func TestSpawnCapturesChildStderr(t *testing.T) {
	const boom = "Invalid API key · Please run /login"
	rt := fakeClaudeRuntime(t, 0)
	t.Setenv("SHIPMATES_CLAUDE_FAKE_STDERR", boom)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	// The child exits as soon as it reads the turn, so SendTurn may either
	// succeed (and the reason arrives on the terminal event) or fail on a broken
	// stdin (and the reason must be in the error). Both paths must name it.
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "hello"}); err != nil {
		if !strings.Contains(err.Error(), boom) {
			t.Fatalf("SendTurn error hides the child's stderr: %v", err)
		}
		return
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("event stream closed before a terminal event")
			}
			if ev.Kind != runtime.KindError && ev.Kind != runtime.KindSessionClosed {
				continue
			}
			if got := fmt.Sprintf("%v", ev.Payload); !strings.Contains(got, boom) {
				t.Fatalf("terminal event does not carry the child's stderr: %v", got)
			}
			return
		case <-deadline:
			t.Fatal("no terminal event within 20s")
		}
	}
}

// TestStderrTailIsBoundedAndKeepsTheEnd pins the ring buffer: a chatty child
// must not grow shipmates without bound, and the retained window has to be the
// END of the output, which is where the failure reason is.
func TestStderrTailIsBoundedAndKeepsTheEnd(t *testing.T) {
	tail := newStderrTail(16)
	for i := 0; i < 500; i++ {
		if _, err := tail.Write([]byte("noise noise noise ")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tail.Write([]byte("Please run /login")); err != nil {
		t.Fatal(err)
	}
	got := tail.String()
	if len(got) > 16+len("...") {
		t.Fatalf("tail grew to %d bytes past its 16-byte bound: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "run /login") {
		t.Fatalf("tail dropped the most recent output: %q", got)
	}
	if !strings.HasPrefix(got, "...") {
		t.Fatalf("truncation not marked: %q", got)
	}

	// A single write larger than the whole window keeps its own tail rather
	// than overflowing.
	one := newStderrTail(8)
	if _, err := one.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := one.String(); got != "...89abcdef" {
		t.Fatalf("oversized single write = %q", got)
	}

	// Nothing written means nothing appended to a diagnostic.
	if got := withStderr("exit status 1", newStderrTail(8).String()); got != "exit status 1" {
		t.Fatalf("empty tail decorated the detail: %q", got)
	}
}
