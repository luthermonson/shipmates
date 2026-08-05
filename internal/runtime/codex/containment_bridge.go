package codex

import (
	"context"
	"os/exec"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// Contain binds a shipmates containment watcher and its per-launch limits into
// the supervisor the codex app-server process is spawned through:
//
//	opts.Supervisor = codex.Contain(watchdog.New(), limits)
//
// This file is the ONLY place internal/runtime/codex touches
// internal/runtime/containment, and the only bridge between the transport's
// stdlib-only seam (internal/codexapp/supervise.go) and the shipmates
// containment package. It is deliberately the same shape as
// internal/runtime/claude/containment_bridge.go: codex and claude now get
// their process bounds from one portable Go implementation — RSS/CPU sampling
// plus native tree teardown, identical on Linux, macOS and Windows — instead
// of a watchdog for one runtime and a Linux-only cgroup launcher for the
// other.
//
// A nil watcher yields nil, which codexapp reads as "start the child
// directly": an unsupervised transport that honestly reports
// Caps.Containment == false rather than a supervisor that pretends.
func Contain(w containment.Watcher, limits containment.Limits) codexapp.Supervisor {
	if w == nil {
		return nil
	}
	return supervisor{watcher: w, limits: limits}
}

type supervisor struct {
	watcher containment.Watcher
	limits  containment.Limits
}

// Bounded reports whether this supervisor actually contains anything, which is
// what Caps.Containment is derived from.
//
// The "none" watcher is deliberately excluded. It reaps the child and nothing
// more: no process group, no job object, no limits — its own package doc says
// a killed child may leave descendants behind. Reporting containment for it
// would be the precise kind of over-claim the capability contract exists to
// prevent, and it is worse than reporting none at all because a caller that
// trusts a false-positive silently loses the protection it thinks it has.
func (s supervisor) Bounded() bool { return s.watcher.Kind() != "none" }

func (s supervisor) Start(cmd *exec.Cmd) (codexapp.Handle, error) {
	h, err := s.watcher.Start(cmd, s.limits)
	if err != nil {
		return nil, err
	}
	// Translate the watcher's terminal event into the transport's. Both
	// channels carry at most one value and are then closed, which is what
	// keeps this safe: containment.Handle.Close waits on its own exit signal
	// rather than consuming Done(), so this goroutine is the single reader.
	//
	// A source that closes WITHOUT delivering (an already-consumed event) must
	// close out without sending too: the adapter distinguishes "ended with a
	// reason" from "ended, reason unavailable" by the receive's ok flag, and
	// inventing a zero Terminal here would report a bogus clean exit.
	out := make(chan codexapp.Terminal, 1)
	go func() {
		defer close(out)
		ev, ok := <-h.Done()
		if !ok {
			return
		}
		out <- codexapp.Terminal{
			Reason:   string(ev.Reason),
			Detail:   ev.Detail,
			ExitCode: ev.ExitCode,
			At:       ev.At,
		}
	}()
	return handle{inner: h, done: out}, nil
}

type handle struct {
	inner containment.Handle
	done  <-chan codexapp.Terminal
}

func (h handle) Pid() int                        { return h.inner.Pid() }
func (h handle) Done() <-chan codexapp.Terminal  { return h.done }
func (h handle) Close(ctx context.Context) error { return h.inner.Close(ctx) }
