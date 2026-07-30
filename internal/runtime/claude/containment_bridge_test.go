package claude

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// stubWatcher is a containment.Watcher that runs the command for real (so the
// session's stdio still works) and records the limits it was handed. It exists
// because internal/runtime/containment ships no no-op implementation this
// package could reuse.
type stubWatcher struct {
	mu     sync.Mutex
	limits containment.Limits
	starts int
}

func (w *stubWatcher) Kind() string { return "stub" }

func (w *stubWatcher) Start(cmd *exec.Cmd, limits containment.Limits) (containment.Handle, error) {
	w.mu.Lock()
	w.limits = limits
	w.starts++
	w.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &stubHandle{cmd: cmd, done: make(chan containment.Event, 1)}
	go func() {
		err := cmd.Wait()
		ev := containment.Event{Reason: containment.ReasonExited, ExitCode: -1, At: time.Now()}
		if cmd.ProcessState != nil {
			ev.ExitCode = cmd.ProcessState.ExitCode()
		}
		if err != nil {
			ev.Detail = err.Error()
		}
		h.once.Do(func() { h.done <- ev; close(h.done) })
	}()
	return h, nil
}

func (w *stubWatcher) seen() (containment.Limits, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.limits, w.starts
}

type stubHandle struct {
	cmd  *exec.Cmd
	done chan containment.Event
	once sync.Once
}

func (h *stubHandle) Pid() int                       { return h.cmd.Process.Pid }
func (h *stubHandle) Done() <-chan containment.Event { return h.done }

func (h *stubHandle) Close(ctx context.Context) error {
	_ = h.cmd.Process.Kill()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
	case <-ctx.Done():
	}
	return nil
}

// TestContain_DrivesASessionThroughAContainmentWatcher proves the bridge is
// wired end to end: the watcher receives the spawn along with its limits, the
// session's stdio still works through it, and the turn completes.
func TestContain_DrivesASessionThroughAContainmentWatcher(t *testing.T) {
	exe := fakeClaudeBinary(t, 0)
	w := &stubWatcher{}
	limits := containment.Limits{MaxRSSBytes: 512 << 20, PollInterval: 250 * time.Millisecond}
	rt := New(Config{Binary: exe, Supervisor: Contain(w, limits)})
	defer rt.Close(context.Background())

	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "hi"}); err != nil {
		t.Fatalf("SendTurn through the containment bridge: %v", err)
	}
	waitEventText(t, rt, "fake hello")
	waitTurnDone(t, rt)

	got, starts := w.seen()
	if starts != 1 {
		t.Errorf("watcher saw %d spawns, want 1", starts)
	}
	if got != limits {
		t.Errorf("watcher limits = %+v, want %+v", got, limits)
	}
}

// TestContain_TranslatesTheTerminalEvent proves the watcher's reason and exit
// code survive the hop into a runtime event, which is the whole point of the
// bridge: a session that dies has to say why.
func TestContain_TranslatesTheTerminalEvent(t *testing.T) {
	exe := fakeClaudeBinary(t, 0)
	t.Setenv("SHIPMATES_CLAUDE_FAKE_STDERR", "Invalid API key · Please run /login")
	rt := New(Config{Binary: exe, Supervisor: Contain(&stubWatcher{}, containment.Limits{})})
	defer rt.Close(context.Background())

	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "hi"}); err != nil {
		// The child exits on the first turn, so a broken stdin is a legitimate
		// outcome — but it still has to name the reason.
		if !strings.Contains(err.Error(), "Please run /login") {
			t.Fatalf("SendTurn error hides the child's stderr: %v", err)
		}
		return
	}
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before a terminal event")
			}
			if ev.Kind != runtime.KindError && ev.Kind != runtime.KindSessionClosed {
				continue
			}
			term, isTerm := ev.Payload.(Terminal)
			if !isTerm {
				t.Fatalf("terminal payload is %T (%v), want Terminal", ev.Payload, ev.Payload)
			}
			if term.Reason != string(containment.ReasonExited) {
				t.Errorf("Reason = %q, want %q", term.Reason, containment.ReasonExited)
			}
			if term.ExitCode != 1 {
				t.Errorf("ExitCode = %d, want 1", term.ExitCode)
			}
			if !strings.Contains(term.Detail, "Please run /login") {
				t.Errorf("Detail dropped the child's stderr: %q", term.Detail)
			}
			return
		case <-deadline:
			t.Fatal("no terminal event within 20s")
		}
	}
}

// TestContain_NilWatcherFallsBackToDirect keeps a factory that has not resolved
// a containment mode from producing a runtime that panics on first spawn.
func TestContain_NilWatcherFallsBackToDirect(t *testing.T) {
	sup := Contain(nil, containment.Limits{})
	if sup == nil {
		t.Fatal("Contain(nil) returned a nil Supervisor")
	}
	if _, err := sup.Start(exec.Command("definitely-not-a-real-binary-xyz")); err == nil {
		t.Fatal("expected a start error from the fallback supervisor")
	} else if !strings.Contains(err.Error(), "claude: start session process") {
		t.Errorf("fallback is not the direct supervisor: %v", err)
	}
}

// TestContain_PropagatesStartFailure proves a watcher that refuses the spawn
// surfaces its error rather than a nil handle.
func TestContain_PropagatesStartFailure(t *testing.T) {
	boom := errors.New("cgroup delegation unavailable")
	sup := Contain(refusingWatcher{err: boom}, containment.Limits{})
	h, err := sup.Start(exec.Command("irrelevant"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if h != nil {
		t.Errorf("handle = %v, want nil on start failure", h)
	}
}

type refusingWatcher struct{ err error }

func (refusingWatcher) Kind() string { return "refusing" }
func (w refusingWatcher) Start(*exec.Cmd, containment.Limits) (containment.Handle, error) {
	return nil, w.err
}
