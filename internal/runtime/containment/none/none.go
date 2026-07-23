// Package none provides a containment.Watcher that applies no bounds.
// Kept as a first-class implementation so config-driven selection can
// explicitly opt out of containment without leaking a "nil watcher"
// special case into every caller.
package none

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// New returns a no-op Watcher.
func New() containment.Watcher { return watcher{} }

type watcher struct{}

func (watcher) Kind() string { return "none" }

func (watcher) Start(cmd *exec.Cmd, _ containment.Limits) (containment.Handle, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("containment/none: start: %w", err)
	}
	h := &handle{cmd: cmd, done: make(chan containment.Event, 1)}
	go h.wait()
	return h, nil
}

type handle struct {
	cmd  *exec.Cmd
	done chan containment.Event
	once sync.Once
}

func (h *handle) Pid() int                       { return h.cmd.Process.Pid }
func (h *handle) Done() <-chan containment.Event { return h.done }

func (h *handle) Close(ctx context.Context) error {
	_ = h.cmd.Process.Kill()
	select {
	case <-h.done:
	case <-ctx.Done():
	}
	return nil
}

func (h *handle) wait() {
	err := h.cmd.Wait()
	ev := containment.Event{Reason: containment.ReasonExited, ExitCode: h.cmd.ProcessState.ExitCode(), At: time.Now()}
	if err != nil {
		ev.Detail = err.Error()
	}
	h.once.Do(func() { h.done <- ev; close(h.done) })
}
