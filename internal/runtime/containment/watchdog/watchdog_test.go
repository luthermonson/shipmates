package watchdog

import (
	"context"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

func TestKind(t *testing.T) {
	if got := New().Kind(); got != "watchdog" {
		t.Errorf("Kind() = %q, want watchdog", got)
	}
}

func TestStart_NaturalExit(t *testing.T) {
	h, err := New().Start(sleeper(200*time.Millisecond), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if h.Pid() <= 0 {
		t.Errorf("pid = %d, want > 0", h.Pid())
	}
	ev := awaitDone(t, h, 30*time.Second)
	if ev.Reason != containment.ReasonExited {
		t.Errorf("reason = %q, want exited (detail=%q)", ev.Reason, ev.Detail)
	}
	if ev.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0 (detail=%q)", ev.ExitCode, ev.Detail)
	}
	if ev.At.IsZero() {
		t.Error("event timestamp not set")
	}
}

// Close must terminate the child AND leave the terminal event on Done() for
// the caller to read. An earlier version of this watchdog consumed its own
// event inside Close, so a caller who closed and then read Done() got a zero
// Event off a closed channel and could not tell why the process ended.
func TestClose_KillsAndStillReportsOnDone(t *testing.T) {
	h, err := New().Start(sleeper(60*time.Second), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Errorf("close: %v", err)
	}
	ev := awaitDone(t, h, 10*time.Second)
	if ev.Reason != containment.ReasonRequested {
		t.Errorf("reason = %q, want requested — Close must classify the shutdown it caused", ev.Reason)
	}
}

func TestClose_Idempotent(t *testing.T) {
	h, err := New().Start(sleeper(60*time.Second), containment.Limits{})
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

func TestClose_AfterNaturalExit(t *testing.T) {
	h, err := New().Start(sleeper(50*time.Millisecond), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ev := awaitDone(t, h, 30*time.Second)
	if ev.Reason != containment.ReasonExited {
		t.Fatalf("reason = %q, want exited", ev.Reason)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Errorf("close after exit: %v", err)
	}
}

// The memory cap must actually stop a runaway allocator. Which mechanism
// fires is platform-specific — the Windows kernel fails the allocation
// through the Job Object cap, everything else notices on the next sample — so
// the assertion is on the outcome: the child died early, and not cleanly.
func TestMemoryLimit_StopsRunawayAllocator(t *testing.T) {
	limits := containment.Limits{
		MaxRSSBytes:     48 << 20, // 48 MiB — the hog wants 512 MiB
		PollInterval:    50 * time.Millisecond,
		GracefulTimeout: 500 * time.Millisecond,
	}
	h, err := New().Start(helperCmd("hog", ""), limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	ev := awaitDone(t, h, 25*time.Second)
	t.Logf("terminal event: reason=%s exit=%d detail=%q", ev.Reason, ev.ExitCode, ev.Detail)
	if ev.Reason == containment.ReasonMemoryLimit {
		return // sampler caught it, the reason names the limit
	}
	// Kernel-enforced path: the allocation failed inside the child, so the
	// watchdog sees an ordinary exit. It must at least be a failed one.
	if ev.ExitCode == 0 {
		t.Errorf("child exited cleanly despite a 48 MiB cap and a 512 MiB appetite; ev=%+v", ev)
	}
}

func TestCPULimit_StopsSpinner(t *testing.T) {
	limits := containment.Limits{
		MaxCPUSeconds:   0.25,
		PollInterval:    50 * time.Millisecond,
		GracefulTimeout: 500 * time.Millisecond,
	}
	h, err := New().Start(helperCmd("spin", ""), limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	ev := awaitDone(t, h, 30*time.Second)
	t.Logf("terminal event: reason=%s exit=%d detail=%q", ev.Reason, ev.ExitCode, ev.Detail)
	if ev.Reason != containment.ReasonCPULimit {
		t.Errorf("reason = %q, want cpu_limit", ev.Reason)
	}
}

// Done() carries exactly one event and is then closed. Callers multiplex on
// it, so a second event (or a second close) would panic the watchdog.
func TestDone_DeliversExactlyOneEventThenCloses(t *testing.T) {
	h, err := New().Start(sleeper(100*time.Millisecond), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ev := awaitDone(t, h, 30*time.Second); ev.Reason != containment.ReasonExited {
		t.Fatalf("reason = %q, want exited", ev.Reason)
	}
	select {
	case ev, ok := <-h.Done():
		if ok {
			t.Errorf("a second event arrived on Done(): %+v", ev)
		}
	case <-time.After(time.Second):
		t.Error("Done() was not closed after delivering its event")
	}
}

// An unbounded launch has nothing to sample for, so no breach can ever be
// recorded — a sampler that could never fire would just be a goroutine and a
// syscall per tick spent on nothing.
func TestUnboundedLaunch_RecordsNoBreach(t *testing.T) {
	cmd := sleeper(200 * time.Millisecond)
	h, err := New().Start(cmd, containment.Limits{PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	hh, ok := h.(*handle)
	if !ok {
		t.Fatalf("handle type = %T", h)
	}
	awaitDone(t, h, 30*time.Second)
	if b := hh.breached.Load(); b != nil {
		t.Errorf("an unbounded launch recorded a breach: %+v", b)
	}
}

func TestSampleRSS_ReturnsSomething(t *testing.T) {
	cmd := sleeper(30 * time.Second)
	h, err := New().Start(cmd, containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())
	time.Sleep(300 * time.Millisecond) // let it get resident

	rss, err := sampleRSS(h.Pid())
	if err != nil {
		t.Fatalf("sampleRSS: %v", err)
	}
	if rss <= 0 {
		t.Errorf("rss = %d, want > 0", rss)
	}
	cpu, err := sampleCPUSeconds(h.Pid())
	if err != nil {
		t.Fatalf("sampleCPUSeconds: %v", err)
	}
	if cpu < 0 {
		t.Errorf("cpu = %v, want >= 0", cpu)
	}
}

func awaitDone(t *testing.T, h containment.Handle, within time.Duration) containment.Event {
	t.Helper()
	select {
	case ev := <-h.Done():
		return ev
	case <-time.After(within):
		t.Fatalf("no terminal event within %v", within)
		return containment.Event{}
	}
}
