package watchdog

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// sleeper returns an exec.Cmd that sleeps for the given seconds using each
// platform's native sleep-like command.
func sleeper(seconds int) *exec.Cmd {
	switch runtime.GOOS {
	case "windows":
		// timeout blocks in interactive shells; ping /n loopback is the
		// portable "wait N seconds" idiom on Windows.
		return exec.Command("cmd", "/C", "ping", "-n", itostr(seconds+1), "127.0.0.1", ">nul")
	default:
		return exec.Command("sleep", itostr(seconds))
	}
}

func itostr(n int) string {
	// avoid strconv import in a test helper
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func TestStart_NaturalExit(t *testing.T) {
	w := New()
	cmd := sleeper(1)
	h, err := w.Start(cmd, containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.Pid() <= 0 {
		t.Errorf("pid <= 0: %d", h.Pid())
	}
	select {
	case ev := <-h.Done():
		if ev.Reason != containment.ReasonExited {
			t.Errorf("reason=%q want exited", ev.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("process never exited")
	}
}

func TestStart_CloseKills(t *testing.T) {
	w := New()
	cmd := sleeper(60) // long enough that only Close() ends it
	h, err := w.Start(cmd, containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(6 * time.Second):
		t.Fatalf("process still alive after Close()")
	}
}

func TestSampleRSS_ReturnsSensible(t *testing.T) {
	// Sample our own process; any RSS > 0 is a win — verifies the platform
	// syscall path compiles and returns.
	cmd := sleeper(30)
	if err := prepare(cmd, containment.Limits{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	if err := attach(cmd, containment.Limits{}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Give the process a moment to allocate.
	time.Sleep(200 * time.Millisecond)

	rss, err := sampleRSS(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("sampleRSS: %v", err)
	}
	if rss <= 0 {
		t.Errorf("expected rss > 0, got %d", rss)
	}
}
