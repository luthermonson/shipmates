package server

// ring is a fixed-capacity byte ring buffer holding the most recent PTY
// output. A newly-attaching viewer gets Snapshot() as backscroll so the pane
// isn't blank; the PTY pump keeps writing regardless of subscriber count.
// Not goroutine-safe — callers hold the owning ptyProc's mutex.
type ring struct {
	buf   []byte
	start int // index of oldest byte
	size  int // bytes currently held
}

func newRing(capacity int) *ring {
	return &ring{buf: make([]byte, capacity)}
}

func (r *ring) Write(p []byte) {
	if len(p) >= len(r.buf) {
		// chunk alone overflows capacity: keep only its tail
		copy(r.buf, p[len(p)-len(r.buf):])
		r.start = 0
		r.size = len(r.buf)
		return
	}
	// write p at the logical end, wrapping
	end := (r.start + r.size) % len(r.buf)
	n := copy(r.buf[end:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.size += len(p)
	if r.size > len(r.buf) {
		// overwrote oldest bytes; advance start
		r.start = (r.start + (r.size - len(r.buf))) % len(r.buf)
		r.size = len(r.buf)
	}
}

// Snapshot returns the held bytes oldest-first as a fresh slice.
func (r *ring) Snapshot() []byte {
	out := make([]byte, r.size)
	n := copy(out, r.buf[r.start:min(r.start+r.size, len(r.buf))])
	if n < r.size {
		copy(out[n:], r.buf[:r.size-n])
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
