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

// eventLogCap bounds the in-memory activity feed. A captain wired to a fleet
// never exits on idle (see idleTimeoutFleeted), so "grows for the life of the
// process" meant "grows forever": every hook callback, every assistant chunk,
// every auto-allow appended a struct that was then copied wholesale on every
// GET /events — which the fleet polls every 2 seconds per ship. 5000 events is
// several hours of a busy crew and a few megabytes at worst.
//
// What capping means for consumers: /events is a *recent* feed, not an
// archive. The fleet's mirrorLoop already persists what it reads into SQLite
// and advances a timestamp watermark, so history survives on the fleet side
// and a cap only matters if a ship emits more than eventLogCap events between
// two 2-second polls — which would take a sustained ~2500 events/second.
// Consumers that page backwards through the ship's own /events (the fleet's
// conversation transcript reader) now see the newest eventLogCap entries
// rather than all of them; that is the intended trade, and the alternative
// was an unbounded allocation an anonymous caller could drive.
const eventLogCap = 5000

// eventLog is the activity feed's fixed-capacity counterpart to ring: same
// drop-oldest rule, Event elements instead of bytes. It stays a slice type so
// the feed can still be ranged and indexed directly.
//
// Not goroutine-safe — callers hold the owning Server's mutex.
type eventLog []Event

// Append adds one event, evicting the oldest when the log is at capacity. The
// eviction copies down in place rather than reslicing, so the backing array
// stays bounded instead of the slice header merely walking forward through an
// ever-growing allocation.
func (l *eventLog) Append(e Event) {
	*l = append(*l, e)
	if over := len(*l) - eventLogCap; over > 0 {
		*l = append((*l)[:0], (*l)[over:]...)
	}
}
