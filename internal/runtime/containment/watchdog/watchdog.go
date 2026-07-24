// Package watchdog is the shipmates default containment: a cross-platform
// Go process wrapper that periodically samples RSS + CPU time and kills
// the process tree when limits are exceeded.
//
// Trade-off vs cgroups: not kernel-instant on rapid memory spikes (limited
// by PollInterval, default 500ms) but zero privileges, zero install step,
// works on Linux/macOS/Windows uniformly. The right default for shipmates
// homelab and developer use; cgroups remains available as an opt-in for
// enterprise Linux deployments.
package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// New returns a watchdog Watcher.
func New() containment.Watcher { return watcher{} }

type watcher struct{}

func (watcher) Kind() string { return "watchdog" }

func (watcher) Start(cmd *exec.Cmd, limits containment.Limits) (containment.Handle, error) {
	// Attach the platform-specific containment (Unix process group, Windows
	// Job Object) BEFORE Cmd.Start runs. Limits flow through so kernel-
	// enforced caps (Windows Job Object memory / active process limits) can
	// be programmed at the same time.
	if err := prepare(cmd, limits); err != nil {
		return nil, fmt.Errorf("watchdog: prepare: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("watchdog: start: %w", err)
	}
	// Post-start setup (e.g. assign to Windows Job Object). No-op on Unix.
	if err := attach(cmd, limits); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("watchdog: attach: %w", err)
	}

	h := &handle{
		cmd:    cmd,
		limits: limits,
		done:   make(chan containment.Event, 1),
		stop:   make(chan struct{}),
	}

	go h.waitLoop()
	if limits.MaxRSSBytes > 0 || limits.MaxCPUSeconds > 0 {
		go h.pollLoop()
	}

	return h, nil
}

type handle struct {
	cmd    *exec.Cmd
	limits containment.Limits
	done   chan containment.Event
	stop   chan struct{}
	once   sync.Once
}

func (h *handle) Pid() int                       { return h.cmd.Process.Pid }
func (h *handle) Done() <-chan containment.Event { return h.done }

func (h *handle) Close(ctx context.Context) error {
	deadline := h.limits.EffectiveGracefulTimeout()
	if ctx != nil {
		if d, ok := ctx.Deadline(); ok {
			deadline = time.Until(d)
		}
	}
	// Signal termination cooperatively, escalate on deadline.
	_ = killTree(h.cmd, false)
	select {
	case <-h.done:
		h.emit(containment.Event{Reason: containment.ReasonRequested, At: time.Now()})
		return nil
	case <-time.After(deadline):
		_ = killTree(h.cmd, true)
	}
	select {
	case <-h.done:
	case <-ctx.Done():
	}
	h.emit(containment.Event{Reason: containment.ReasonRequested, At: time.Now()})
	return nil
}

// waitLoop reaps the child and produces the terminal event.
func (h *handle) waitLoop() {
	err := h.cmd.Wait()
	close(h.stop) // notify pollLoop
	code := -1
	if h.cmd.ProcessState != nil {
		code = h.cmd.ProcessState.ExitCode()
	}
	ev := containment.Event{
		Reason:   containment.ReasonExited,
		ExitCode: code,
		At:       time.Now(),
	}
	if err != nil {
		ev.Detail = err.Error()
	}
	h.emit(ev)
}

// pollLoop samples RSS/CPU on the configured cadence and kills on breach.
// A failed sample (process racing exit, malformed ps output, permission)
// is a skipped tick, never a 0.0 reading — it's logged at debug so a
// persistently failing sampler is diagnosable instead of silently
// disabling the limit.
func (h *handle) pollLoop() {
	tick := time.NewTicker(h.limits.EffectivePollInterval())
	defer tick.Stop()
	var lastCPU float64
	for {
		select {
		case <-h.stop:
			return
		case <-tick.C:
			if h.limits.MaxRSSBytes > 0 {
				rss, err := sampleRSS(h.cmd.Process.Pid)
				if err != nil {
					slog.Debug("watchdog: rss sample failed", "pid", h.cmd.Process.Pid, "err", err)
				} else if rss > h.limits.MaxRSSBytes {
					_ = killTree(h.cmd, true)
					h.emit(containment.Event{
						Reason:   containment.ReasonMemoryLimit,
						Detail:   fmt.Sprintf("RSS %d bytes exceeded limit %d", rss, h.limits.MaxRSSBytes),
						At:       time.Now(),
						ExitCode: -1,
					})
					return
				}
			}
			if h.limits.MaxCPUSeconds > 0 {
				cpu, err := sampleCPUSeconds(h.cmd.Process.Pid)
				if err != nil {
					slog.Debug("watchdog: cpu sample failed", "pid", h.cmd.Process.Pid, "err", err)
				} else {
					lastCPU = cpu
					if cpu > h.limits.MaxCPUSeconds {
						_ = killTree(h.cmd, true)
						h.emit(containment.Event{
							Reason:   containment.ReasonCPULimit,
							Detail:   fmt.Sprintf("CPU %.2fs exceeded limit %.2fs", cpu, h.limits.MaxCPUSeconds),
							At:       time.Now(),
							ExitCode: -1,
						})
						return
					}
				}
			}
			_ = lastCPU
		}
	}
}

// emit posts an event on Done exactly once.
func (h *handle) emit(ev containment.Event) {
	h.once.Do(func() {
		h.done <- ev
		close(h.done)
	})
}
