// Package none provides a containment.Watcher that applies no bounds.
//
// It is a first-class implementation rather than a nil Watcher so that
// "containment off" is an explicit choice an operator makes in config, and so
// no caller has to carry a nil-check branch. It still reaps the child and
// reports a terminal event, because callers depend on Done() regardless of
// posture.
package none

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// New returns a Watcher that imposes no limits.
func New() containment.Watcher { return watcher{} }

type watcher struct{}

func (watcher) Kind() string { return "none" }

// Start runs cmd as-is. Limits are ignored — that is the whole point of this
// watcher — and no process group or job object is created, so a killed child
// may leave descendants behind.
func (watcher) Start(cmd *exec.Cmd, _ containment.Limits) (containment.Handle, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("containment/none: start: %w", err)
	}
	h := &handle{
		cmd:    cmd,
		done:   make(chan containment.Event, 1),
		exited: make(chan struct{}),
	}
	go h.wait()
	return h, nil
}

type handle struct {
	cmd     *exec.Cmd
	done    chan containment.Event
	exited  chan struct{}
	closing atomic.Bool
	once    sync.Once
}

func (h *handle) Pid() int                       { return h.cmd.Process.Pid }
func (h *handle) Done() <-chan containment.Event { return h.done }

// Close kills the process and waits for the reaper. Without a process group
// there is nothing to escalate to, so there is no graceful phase: an operator
// who turned containment off asked for exactly this.
func (h *handle) Close(ctx context.Context) error {
	h.closing.Store(true)
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.exited:
		return nil
	default:
	}
	_ = h.cmd.Process.Kill()
	select {
	case <-h.exited:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("containment/none: pid %d did not exit before the deadline: %w", h.Pid(), ctx.Err())
	}
}

func (h *handle) wait() {
	err := h.cmd.Wait()
	close(h.exited)
	code := -1
	if h.cmd.ProcessState != nil {
		code = h.cmd.ProcessState.ExitCode()
	}
	ev := containment.Event{Reason: containment.ReasonExited, ExitCode: code, At: time.Now()}
	if err != nil {
		ev.Detail = err.Error()
	}
	if h.closing.Load() {
		ev.Reason = containment.ReasonRequested
	}
	h.once.Do(func() {
		h.done <- ev
		close(h.done)
	})
}
