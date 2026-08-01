package bridge

import "sync"

// maxInputQueue bounds bytes waiting to be POSTed to one mate. A wedged or very
// slow server must not let held keys grow memory without limit. 256 KiB is far
// past anything a human or a paste produces in the time one POST takes.
const maxInputQueue = 256 << 10

// inputPipe serializes keystroke delivery for one persona.
//
// This exists because of how Bubble Tea schedules work: every tea.Cmd returned
// from Update runs in its OWN goroutine, concurrently. Returning one command per
// keystroke therefore gives no ordering guarantee at all — typing "hi" really can
// arrive at the PTY as "ih", which is exactly what an earlier version of this code
// did (see TestKeystrokesReachTheFocusedMateOnly). For a terminal multiplexer
// byte order is not cosmetic, it is the entire contract.
//
// The fix is to establish order where order exists. Update is called from a single
// goroutine, so appends made *inside* Update are totally ordered; enqueue happens
// there. Exactly one drain command is ever in flight per persona, so the POSTs
// leave in append order no matter how the scheduler interleaves anything else.
// Coalescing falls out for free: a burst of keys drains as one request.
type inputPipe struct {
	mu       sync.Mutex
	buf      []byte
	draining bool
}

// enqueue appends data to the queue. It reports start=true when the caller must
// return a drain command, and dropped=true when the queue was full and data was
// discarded.
//
// Overflow drops the NEW bytes rather than the old ones: truncating the tail of
// what the operator typed is bad, but reordering the front of it — which is what
// dropping from the head would do — is worse.
func (p *inputPipe) enqueue(data []byte) (start, dropped bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf)+len(data) > maxInputQueue {
		return false, true
	}
	p.buf = append(p.buf, data...)
	if p.draining {
		// A drain is already running; it will pick these bytes up in order.
		return false, false
	}
	p.draining = true
	return true, false
}

// take removes and returns up to n bytes. A nil return means the queue is empty
// and the drain is over; the draining flag is cleared under the same lock that
// enqueue takes, so a keystroke arriving at that instant either joins this drain
// or starts the next one — it can never be silently stranded.
func (p *inputPipe) take(n int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) == 0 {
		p.draining = false
		return nil
	}
	if n > len(p.buf) {
		n = len(p.buf)
	}
	out := make([]byte, n)
	copy(out, p.buf[:n])
	p.buf = p.buf[:copy(p.buf, p.buf[n:])]
	return out
}

// abort discards queued bytes and ends the drain. Used when a write fails — the
// rest of the burst would fail the same way — and when a tab is detached, so
// stale keystrokes cannot land on a mate the operator has walked away from.
func (p *inputPipe) abort() {
	p.mu.Lock()
	p.buf = nil
	p.draining = false
	p.mu.Unlock()
}

func (p *inputPipe) queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}
