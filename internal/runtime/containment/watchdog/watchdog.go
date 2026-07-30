// Package watchdog is the shipmates default containment: native process-tree
// teardown plus a Go sampler that periodically reads RSS and CPU time and
// kills the tree when a limit is exceeded.
//
// Trade-off versus cgroups: sampling is not kernel-instant on a rapid memory
// spike (bounded by Limits.PollInterval, default 500ms), but it needs zero
// privileges and no install step and behaves the same on Linux, macOS and
// Windows. Windows additionally gets kernel-enforced memory and
// active-process caps for free, because Job Objects express them.
package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// New returns a watchdog Watcher. The zero value is usable and stateless;
// each Start gets its own handle.
func New() containment.Watcher { return watcher{} }

type watcher struct{}

func (watcher) Kind() string { return "watchdog" }

func (watcher) Start(cmd *exec.Cmd, limits containment.Limits) (containment.Handle, error) {
	// Attach platform containment BEFORE Cmd.Start: on Unix that is the
	// process-group request, on Windows it is CREATE_SUSPENDED so the
	// process can be put in a Job Object before it runs any code. Limits
	// flow through so the kernel-enforced Windows caps are programmed at the
	// same moment.
	if err := prepare(cmd, limits); err != nil {
		return nil, fmt.Errorf("watchdog: prepare: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watchdog: start: %w", err)
	}
	// Post-start setup: assign to the Job Object and resume on Windows,
	// no-op on Unix. A failure here leaves a live uncontained process, so
	// kill it rather than hand back something unbounded.
	if err := attach(cmd, limits); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("watchdog: attach: %w", err)
	}

	h := &handle{
		cmd:    cmd,
		limits: limits,
		done:   make(chan containment.Event, 1),
		exited: make(chan struct{}),
		stop:   make(chan struct{}),
	}

	go h.waitLoop()
	if limits.Bounded() {
		go h.pollLoop()
	}
	return h, nil
}

// breach records why the sampler decided to kill, so the single terminal
// event can name the limit instead of reporting an ordinary exit.
type breach struct {
	reason containment.Reason
	detail string
}

type handle struct {
	cmd    *exec.Cmd
	limits containment.Limits

	// done carries exactly one terminal event, emitted by waitLoop — the
	// only emitter, so a sampler kill and a natural exit cannot race into
	// two different answers.
	done chan containment.Event
	// exited is closed once cmd.Wait has returned. Close waits on this
	// instead of consuming from done, so the caller's Done() read still
	// sees the event.
	exited chan struct{}
	// stop is closed to shut the sampler down.
	stop chan struct{}

	breached atomic.Pointer[breach]
	closing  atomic.Bool
	once     sync.Once
	stopOnce sync.Once
}

func (h *handle) Pid() int                       { return h.cmd.Process.Pid }
func (h *handle) Done() <-chan containment.Event { return h.done }

func (h *handle) Close(ctx context.Context) error {
	h.closing.Store(true)
	if ctx == nil {
		ctx = context.Background()
	}

	// Already gone? Nothing to do.
	select {
	case <-h.exited:
		return nil
	default:
	}

	grace := h.limits.EffectiveGracefulTimeout()
	if d, ok := ctx.Deadline(); ok {
		if until := time.Until(d); until < grace {
			grace = until
		}
	}
	if grace < 0 {
		grace = 0
	}

	// Ask nicely, then insist.
	_ = killTree(h.cmd, false)
	if grace > 0 {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-h.exited:
			return nil
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	_ = killTree(h.cmd, true)
	select {
	case <-h.exited:
	case <-ctx.Done():
		// The tree outlived a hard kill and the caller's patience. Say so;
		// silently returning nil would present an escaped process as a
		// clean shutdown.
		return fmt.Errorf("watchdog: pid %d did not exit before the deadline: %w", h.Pid(), ctx.Err())
	}
	return nil
}

// waitLoop reaps the child and produces the one terminal event, classifying
// it by what the sampler or Close recorded.
func (h *handle) waitLoop() {
	err := h.cmd.Wait()
	h.stopSampler()
	// Drop any platform resource tied to the tree (the Windows Job Object
	// handle) before anyone can observe the exit, so a long-lived shipmates
	// process does not leak one per turn.
	release(h.cmd)
	close(h.exited)

	code := -1
	if h.cmd.ProcessState != nil {
		code = h.cmd.ProcessState.ExitCode()
	}
	ev := containment.Event{Reason: containment.ReasonExited, ExitCode: code, At: time.Now()}
	if err != nil {
		ev.Detail = err.Error()
	}
	switch {
	case h.breached.Load() != nil:
		b := h.breached.Load()
		ev.Reason, ev.Detail = b.reason, b.detail
	case h.closing.Load():
		ev.Reason = containment.ReasonRequested
	}
	h.once.Do(func() {
		h.done <- ev
		close(h.done)
	})
}

// pollLoop samples RSS and CPU on the configured cadence and kills the tree
// on breach. It records the reason and lets waitLoop emit; it never emits
// itself.
//
// A failed sample — the process racing its own exit, a permission error,
// malformed ps output — is a skipped tick, never a 0.0 reading. Treating a
// failure as zero would quietly disable the limit forever; skipping and
// logging at debug leaves a persistently broken sampler diagnosable.
func (h *handle) pollLoop() {
	tick := time.NewTicker(h.limits.EffectivePollInterval())
	defer tick.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-tick.C:
			if b := h.sample(); b != nil {
				h.breached.CompareAndSwap(nil, b)
				_ = killTree(h.cmd, true)
				return
			}
		}
	}
}

// sample takes one reading of each configured limit and returns the breach
// it found, or nil.
func (h *handle) sample() *breach {
	pid := h.cmd.Process.Pid
	if h.limits.MaxRSSBytes > 0 {
		rss, err := sampleRSS(pid)
		switch {
		case err != nil:
			slog.Debug("watchdog: rss sample failed", "pid", pid, "err", err)
		case rss > h.limits.MaxRSSBytes:
			return &breach{
				reason: containment.ReasonMemoryLimit,
				detail: fmt.Sprintf("RSS %d bytes exceeded limit %d", rss, h.limits.MaxRSSBytes),
			}
		}
	}
	if h.limits.MaxCPUSeconds > 0 {
		cpu, err := sampleCPUSeconds(pid)
		switch {
		case err != nil:
			slog.Debug("watchdog: cpu sample failed", "pid", pid, "err", err)
		case cpu > h.limits.MaxCPUSeconds:
			return &breach{
				reason: containment.ReasonCPULimit,
				detail: fmt.Sprintf("CPU %.2fs exceeded limit %.2fs", cpu, h.limits.MaxCPUSeconds),
			}
		}
	}
	return nil
}

func (h *handle) stopSampler() { h.stopOnce.Do(func() { close(h.stop) }) }
