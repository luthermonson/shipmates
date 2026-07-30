package claude

import (
	"os/exec"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// Contain binds a shipmates containment watcher and its per-launch limits into
// the Supervisor that session processes are spawned through:
//
//	rt := claude.New(claude.Config{
//		Supervisor: claude.Contain(watchdog.New(), limits),
//	})
//
// This file is the ONLY place internal/runtime/claude touches
// internal/runtime/containment. Everything else speaks the stdlib-only seam in
// supervise.go, which is what lets the runtime build and test without a
// containment implementation present — delete this one file and the package is
// still complete, just without the shipmates-native binding.
//
// A nil watcher yields the direct supervisor rather than a panic, so a factory
// that has not resolved a containment mode yet still produces a working
// runtime.
func Contain(w containment.Watcher, limits containment.Limits) Supervisor {
	if w == nil {
		return directSupervisor{}
	}
	return SupervisorFunc(func(cmd *exec.Cmd) (Handle, error) {
		h, err := w.Start(cmd, limits)
		if err != nil {
			return nil, err
		}
		// Translate the watcher's terminal event into ours. Both channels carry
		// at most one value and are then closed, which is what keeps this safe:
		// containment.Handle.Close also reads Done(), so either that read or
		// this goroutine takes the value — and because the source closes after
		// delivering, whichever one loses still completes instead of blocking.
		//
		// A source that closes WITHOUT delivering (already-consumed event) must
		// close out without sending too: readLoop tells "died with a reason"
		// from "died, reason unavailable" by the receive's ok flag, and
		// inventing a zero Terminal here would report a bogus clean exit.
		out := make(chan Terminal, 1)
		go func() {
			defer close(out)
			ev, ok := <-h.Done()
			if !ok {
				return
			}
			out <- Terminal{
				Reason:   string(ev.Reason),
				Detail:   ev.Detail,
				ExitCode: ev.ExitCode,
				At:       ev.At,
			}
		}()
		return NewHandle(h.Pid(), out, h.Close), nil
	})
}
