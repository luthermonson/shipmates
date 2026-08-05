package none

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// As in the watchdog tests, child processes are this test binary re-executed
// in helper mode, so the same code runs on Linux, macOS and Windows.

const helperEnv = "SHIPMATES_NONE_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "quick":
		os.Exit(0)
	case "fail":
		os.Exit(7)
	case "long":
		time.Sleep(60 * time.Second)
		os.Exit(0)
	default:
		os.Exit(3)
	}
}

func helperCmd(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	return cmd
}

func TestKind(t *testing.T) {
	if got := New().Kind(); got != "none" {
		t.Errorf("Kind() = %q, want none", got)
	}
}

func TestStart_NaturalExit(t *testing.T) {
	h, err := New().Start(helperCmd("quick"), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.Pid() <= 0 {
		t.Errorf("pid = %d, want > 0", h.Pid())
	}
	ev := awaitDone(t, h)
	if ev.Reason != containment.ReasonExited || ev.ExitCode != 0 {
		t.Errorf("event = %+v, want a clean exit", ev)
	}
}

func TestStart_ReportsExitCode(t *testing.T) {
	h, err := New().Start(helperCmd("fail"), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ev := awaitDone(t, h)
	if ev.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", ev.ExitCode)
	}
	if ev.Detail == "" {
		t.Error("a non-zero exit should carry the wait error as detail")
	}
}

// Limits are ignored by design; an operator who selected mode "none" asked
// for an unbounded process, and pretending otherwise would be the dishonest
// answer.
func TestStart_IgnoresLimits(t *testing.T) {
	h, err := New().Start(helperCmd("quick"), containment.Limits{MaxRSSBytes: 1, MaxCPUSeconds: 0.0001})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ev := awaitDone(t, h)
	if ev.Reason != containment.ReasonExited {
		t.Errorf("reason = %q, want exited — mode none must not enforce limits", ev.Reason)
	}
}

func TestClose_KillsAndReportsRequested(t *testing.T) {
	h, err := New().Start(helperCmd("long"), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Errorf("close: %v", err)
	}
	if ev := awaitDone(t, h); ev.Reason != containment.ReasonRequested {
		t.Errorf("reason = %q, want requested", ev.Reason)
	}
}

func TestClose_Idempotent(t *testing.T) {
	h, err := New().Start(helperCmd("long"), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range 3 {
		if err := h.Close(ctx); err != nil {
			t.Errorf("close #%d: %v", i+1, err)
		}
	}
}

func TestStart_UnrunnableCommand(t *testing.T) {
	cmd := exec.Command("shipmates-no-such-binary-9f3a")
	if _, err := New().Start(cmd, containment.Limits{}); err == nil {
		t.Fatal("expected an error starting a nonexistent binary")
	}
}

func awaitDone(t *testing.T, h containment.Handle) containment.Event {
	t.Helper()
	select {
	case ev := <-h.Done():
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("no terminal event within 30s")
		return containment.Event{}
	}
}
