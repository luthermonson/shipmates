package claude

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
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

// nopWriteCloser stands in for a session process's stdin in the tests that
// drive readLoop directly, without a child.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// fakeProc builds a proc whose handle has already reported the given terminal
// event and whose stderr is already drained, so readLoop's post-scan teardown
// never blocks. There is no reaper behind it: nothing owns real pipes.
func fakeProc(ev Terminal) *proc {
	done := make(chan Terminal, 1)
	done <- ev
	close(done)
	p := newProc(NewHandle(0, done, nil), nopWriteCloser{}, nil, nil)
	close(p.stderrDone)
	return p
}

// exitedProc is the common case: a child that exited cleanly.
func exitedProc() *proc {
	return fakeProc(Terminal{Reason: ReasonExited, ExitCode: 0, At: time.Now()})
}

// TestReadLoopClampsScannerStartBufferToMax pins the buffer clamp on its own,
// without a subprocess.
//
// bufio.Scanner does not enforce the max it is handed: Scan only consults
// maxTokenSize once the buffer is FULL, so a start buffer larger than max
// silently raises the real ceiling to cap(buf). Handing bufio the usual 64 KiB
// start buffer alongside a 4 KiB max therefore made the 4 KiB "limit" mean
// 64 KiB, and the frame below would have been delivered as a perfectly ordinary
// token. Clamping start to max is what makes the bound a bound.
func TestReadLoopClampsScannerStartBufferToMax(t *testing.T) {
	// Comfortably over the 4 KiB limit and comfortably under bufio's 64 KiB
	// default start buffer — the only window in which the clamp is observable.
	line := `{"type":"text","text":"` + strings.Repeat("x", 32*1024) + `"}` + "\n"

	t.Run("over the limit is refused", func(t *testing.T) {
		rt := New(Config{})
		rt.maxFrameBytes = 4096
		s := rt.rememberSession("sess-clamp", runtime.SessionSpec{}, false)
		rt.readLoop(s, exitedProc(), strings.NewReader(line))

		select {
		case ev := <-rt.Events():
			if ev.Kind != runtime.KindError {
				t.Fatalf("Kind = %q, want error; the oversized frame was accepted, so maxFrameBytes is not the limit", ev.Kind)
			}
			got := fmt.Sprintf("%v", ev.Payload)
			if !strings.Contains(got, "framing failed") || !strings.Contains(got, "token too long") {
				t.Fatalf("error payload does not name the cause: %q", got)
			}
		default:
			t.Fatal("readLoop published nothing: bufio raised the token ceiling to cap(buf) and swallowed the oversized frame")
		}
		// One process death, one terminal event: the framing failure must not be
		// followed by a second terminal for the same exit.
		select {
		case ev := <-rt.Events():
			t.Fatalf("second terminal event for one death: %+v", ev)
		default:
		}
	})

	t.Run("under the limit still parses", func(t *testing.T) {
		// The negative control: with the default ceiling the very same frame is
		// ordinary output, so the assertion above is about the bound and not
		// about large frames being broken in general.
		rt := New(Config{})
		s := rt.rememberSession("sess-ok", runtime.SessionSpec{}, false)
		rt.readLoop(s, exitedProc(), strings.NewReader(line))

		ev := <-rt.Events()
		if ev.Kind != runtime.KindText {
			t.Fatalf("Kind = %q, want text", ev.Kind)
		}
		if got, _ := ev.Payload.(string); len(got) != 32*1024 {
			t.Fatalf("text payload len = %d, want %d", len(got), 32*1024)
		}
		if ev = <-rt.Events(); ev.Kind != runtime.KindSessionClosed {
			t.Fatalf("terminal Kind = %q, want session_closed", ev.Kind)
		}
	})
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

// slowDrainDelay is how long the stderr reader is held past the terminal event
// in the test below. It only has to exceed the 2s stderrDrainGrace this change
// removed: past that bound the old code published whatever was assembled, which
// was nothing.
const slowDrainDelay = 2500 * time.Millisecond

// TestReadLoopWaitsForTheStderrDrainBeforePublishing is issue #41's first
// symptom, made deterministic. The supervisor's terminal event and the stderr
// reader race: cmd.Wait reaps the child on its own goroutine while io.Copy is
// still moving the child's last words into the tail. The old code gave that
// drain a 2s budget and then published without it, so on a contended runner an
// auth failure reached the operator as a bare exit code — the exact regression
// TestSpawnCapturesChildStderr exists to prevent.
//
// The drain here is held open well past that budget rather than left to
// scheduling luck, so the assertion is about the ordering guarantee and not
// about how loaded the machine is.
func TestReadLoopWaitsForTheStderrDrainBeforePublishing(t *testing.T) {
	const boom = "Invalid API key · Please run /login"
	rt := New(Config{})
	s := rt.rememberSession("sess-slow-drain", runtime.SessionSpec{}, false)

	// The handle has already reported the death; the stderr reader has not
	// finished, exactly as when cmd.Wait wins the race.
	p := fakeProc(Terminal{Reason: ReasonExited, Detail: "exit status 1", ExitCode: 1, At: time.Now()})
	p.stderrDone = make(chan struct{})
	go func() {
		time.Sleep(slowDrainDelay)
		_, _ = p.stderr.Write([]byte(boom))
		close(p.stderrDone)
	}()

	rt.readLoop(s, p, strings.NewReader(""))

	ev := <-rt.Events()
	if ev.Kind != runtime.KindSessionClosed {
		t.Fatalf("Kind = %q, want session_closed", ev.Kind)
	}
	term, isTerm := ev.Payload.(Terminal)
	if !isTerm {
		t.Fatalf("terminal payload is %T, want Terminal", ev.Payload)
	}
	if !strings.Contains(term.Detail, boom) {
		t.Fatalf("terminal event published before the stderr drain finished: Detail = %q", term.Detail)
	}
}

// closedOnExitReader delivers a frame and then fails the way a child's stdout
// pipe does when the process is reaped: os.ErrClosed, not io.EOF. Before this
// package owned its pipes, os/exec's Wait produced this error verbatim ("read
// |0: file already closed") whenever it closed the descriptor while the scanner
// was mid-read.
type closedOnExitReader struct {
	frames []byte
	n      int
}

func (r *closedOnExitReader) Read(p []byte) (int, error) {
	if r.n < len(r.frames) {
		n := copy(p, r.frames[r.n:])
		r.n += n
		return n, nil
	}
	return 0, &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}
}

// TestReadLoopTeardownPipeCloseKeepsTheTerminalPayloadType is issue #41's second
// symptom. When the stdout reader lost the teardown race instead of the stderr
// one, scanner.Err() came back as os.ErrClosed and readLoop reported it through
// the framing-failure path — a KindError carrying a *string*. The reason still
// made it to the operator, but the terminal event's payload TYPE now depended on
// which reader finished first, so a consumer type-switching on it silently took
// a different branch. Nothing was malformed: the pipe simply went away because
// the child exited.
func TestReadLoopTeardownPipeCloseKeepsTheTerminalPayloadType(t *testing.T) {
	const boom = "Invalid API key · Please run /login"
	rt := New(Config{})
	s := rt.rememberSession("sess-pipe-closed", runtime.SessionSpec{}, false)
	p := fakeProc(Terminal{Reason: ReasonExited, Detail: "exit status 1", ExitCode: 1, At: time.Now()})
	if _, err := p.stderr.Write([]byte(boom)); err != nil {
		t.Fatal(err)
	}

	rt.readLoop(s, p, &closedOnExitReader{frames: []byte(`{"type":"text","text":"hi"}` + "\n")})

	if ev := <-rt.Events(); ev.Kind != runtime.KindText {
		t.Fatalf("first Kind = %q (%v), want text", ev.Kind, ev.Payload)
	}
	ev := <-rt.Events()
	if ev.Kind != runtime.KindSessionClosed {
		t.Fatalf("terminal Kind = %q, want session_closed", ev.Kind)
	}
	term, isTerm := ev.Payload.(Terminal)
	if !isTerm {
		t.Fatalf("terminal payload is %T (%v), want Terminal: a pipe closed by the child's exit was reported as a framing failure", ev.Payload, ev.Payload)
	}
	if term.ExitCode != 1 || term.Reason != ReasonExited {
		t.Errorf("terminal = %+v, want the supervisor's exited/1", term)
	}
	if !strings.Contains(term.Detail, boom) {
		t.Errorf("Detail dropped the child's stderr: %q", term.Detail)
	}
	// One death, one terminal event.
	select {
	case extra := <-rt.Events():
		t.Fatalf("second terminal event for one death: %+v", extra)
	default:
	}
}

// TestIsPipeClosedSeparatesTeardownFromMalformedStreams pins the classification
// on its own: only a vanished descriptor is teardown. A frame that genuinely
// broke framing must still be reported as one.
func TestIsPipeClosedSeparatesTeardownFromMalformedStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"os/exec closed the pipe on reap", &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}, true},
		{"io pipe closed", io.ErrClosedPipe, true},
		{"oversized frame", bufio.ErrTooLong, false},
		{"unexpected eof", io.ErrUnexpectedEOF, false},
	} {
		if got := isPipeClosed(tc.err); got != tc.want {
			t.Errorf("%s: isPipeClosed(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestStderrDiagnosisNamesAnIncompleteCollection keeps "the child said nothing"
// distinguishable from "we never finished collecting what it said".
func TestStderrDiagnosisNamesAnIncompleteCollection(t *testing.T) {
	if got := stderrDiagnosis("", true); got != "" {
		t.Errorf("a silent child decorated the detail: %q", got)
	}
	if got := stderrDiagnosis("boom", true); got != "boom" {
		t.Errorf("a drained tail was rewritten: %q", got)
	}
	if got := stderrDiagnosis("", false); !strings.Contains(got, "not collected") {
		t.Errorf("an abandoned drain reads as a silent child: %q", got)
	}
	if got := stderrDiagnosis("boom", false); !strings.Contains(got, "boom") || !strings.Contains(got, "incomplete") {
		t.Errorf("a partial tail is not marked partial: %q", got)
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
