package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aymanbagabas/go-pty"
)

// fakePty implements pty.Pty over an in-process pipe so the PTY HTTP surface
// can be exercised without ConPTY, without a `claude` binary on PATH, and
// without a child process to reap. Writes (keystrokes) are captured; reads
// deliver whatever the test feeds in.
type fakePty struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu       sync.Mutex
	written  bytes.Buffer
	cols     int
	rows     int
	resizes  int
	closed   bool
	writeErr error
	sizeErr  error
}

var _ pty.Pty = (*fakePty)(nil)

func newFakePty() *fakePty {
	pr, pw := io.Pipe()
	return &fakePty{pr: pr, pw: pw}
}

func (f *fakePty) Read(p []byte) (int, error) { return f.pr.Read(p) }

func (f *fakePty) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.written.Write(p)
}

func (f *fakePty) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	_ = f.pw.Close()
	return f.pr.Close()
}

func (f *fakePty) Name() string { return "fake-pty" }

func (f *fakePty) Command(string, ...string) *pty.Cmd { return nil }

func (f *fakePty) CommandContext(context.Context, string, ...string) *pty.Cmd { return nil }

func (f *fakePty) Resize(w, h int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sizeErr != nil {
		return f.sizeErr
	}
	f.cols, f.rows, f.resizes = w, h, f.resizes+1
	return nil
}

func (f *fakePty) Fd() uintptr { return 0 }

// feed pushes screen bytes to whatever is reading the PTY.
func (f *fakePty) feed(t *testing.T, s string) {
	t.Helper()
	if _, err := f.pw.Write([]byte(s)); err != nil {
		t.Fatalf("feed: %v", err)
	}
}

func (f *fakePty) keystrokes() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.String()
}

// attachFakePTY registers a ptyProc backed by a fakePty for the persona, as if
// ensurePTY had spawned one, and returns both.
func attachFakePTY(t *testing.T, s *Server, persona string) (*ptyProc, *fakePty) {
	t.Helper()
	f := newFakePty()
	p := &ptyProc{
		persona: persona,
		pt:      f,
		ring:    newRing(ptyRingCap),
		subs:    map[int]chan []byte{},
		modes:   map[int]bool{},
	}
	s.mu.Lock()
	s.ptys[persona] = p
	s.mu.Unlock()
	t.Cleanup(func() { _ = f.Close() })
	return p, f
}

// TestPTYEndpointsRequireALiveMate: every PTY route must 404 rather than
// panic when no mate is attached. Nil-map/nil-pointer dereferences here would
// take the whole captain down, not just the request.
func TestPTYEndpointsRequireALiveMate(t *testing.T) {
	_, h := newTestServer(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/pty/ghost/snapshot", ""},
		{"GET", "/pty/ghost/stream", ""},
		{"POST", "/pty/ghost/input?client=a", "hi"},
		{"POST", "/pty/ghost/resize?client=a", `{"cols":80,"rows":24}`},
		{"POST", "/pty/ghost/takeover?client=a", ""},
		{"POST", "/pty/ghost/release?client=a", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			if w := do(t, h, tc.method, tc.path, tc.body); w.Code != http.StatusNotFound {
				t.Fatalf("= %d, want 404", w.Code)
			}
		})
	}
}

func TestPTYSnapshotServesBackscroll(t *testing.T) {
	s, h := newTestServer(t)
	p, _ := attachFakePTY(t, s, "backend")
	p.mu.Lock()
	p.ring.Write([]byte("hello world"))
	p.mu.Unlock()

	w := do(t, h, "GET", "/pty/backend/snapshot", "")
	if w.Code != http.StatusOK || w.Body.String() != "hello world" {
		t.Fatalf("= %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestPTYInputSingleWriterLock is the multi-viewer safety contract: the first
// client to type owns the keyboard until it releases or someone takes over.
// Without it, two browser tabs interleave keystrokes into one agent session.
func TestPTYInputSingleWriterLock(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")

	if w := do(t, h, "POST", "/pty/backend/input?client=alice", "ls\r"); w.Code != http.StatusAccepted {
		t.Fatalf("alice's first keystroke = %d, want 202", w.Code)
	}
	if w := do(t, h, "POST", "/pty/backend/input?client=bob", "rm -rf /\r"); w.Code != http.StatusConflict {
		t.Fatalf("bob = %d, want 409", w.Code)
	}
	if got := f.keystrokes(); got != "ls\r" {
		t.Fatalf("bob's keystrokes leaked into the PTY: %q", got)
	}

	// Takeover transfers the lock; alice is now the one locked out.
	if w := do(t, h, "POST", "/pty/backend/takeover?client=bob", ""); w.Code != http.StatusAccepted {
		t.Fatalf("takeover = %d", w.Code)
	}
	if w := do(t, h, "POST", "/pty/backend/input?client=alice", "x"); w.Code != http.StatusConflict {
		t.Fatalf("alice after takeover = %d, want 409", w.Code)
	}
	if w := do(t, h, "POST", "/pty/backend/input?client=bob", "y"); w.Code != http.StatusAccepted {
		t.Fatalf("bob after takeover = %d, want 202", w.Code)
	}

	// Release hands the keyboard back to whoever types next.
	if w := do(t, h, "POST", "/pty/backend/release?client=bob", ""); w.Code != http.StatusAccepted {
		t.Fatalf("release = %d", w.Code)
	}
	if w := do(t, h, "POST", "/pty/backend/input?client=alice", "z"); w.Code != http.StatusAccepted {
		t.Fatalf("alice after release = %d, want 202", w.Code)
	}
}

func TestPTYTakeoverRequiresClientID(t *testing.T) {
	// Without this, an anonymous takeover would set writer="" — silently
	// unlocking the keyboard for everyone instead of claiming it.
	s, h := newTestServer(t)
	attachFakePTY(t, s, "backend")
	if w := do(t, h, "POST", "/pty/backend/takeover", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("= %d, want 400", w.Code)
	}
}

func TestPTYReleaseByNonHolderIsANoOp(t *testing.T) {
	s, h := newTestServer(t)
	p, _ := attachFakePTY(t, s, "backend")
	do(t, h, "POST", "/pty/backend/input?client=alice", "a")

	// bob's tab closing must not free alice's keyboard.
	do(t, h, "POST", "/pty/backend/release?client=bob", "")
	p.mu.Lock()
	holder := p.writer
	p.mu.Unlock()
	if holder != "alice" {
		t.Fatalf("writer = %q after bob's release, want alice", holder)
	}
}

func TestPTYInputBypassAndValidation(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")

	// An empty body is rejected before it reaches the PTY.
	if w := do(t, h, "POST", "/pty/backend/input", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("empty input = %d, want 400", w.Code)
	}
	// Server-internal writes carry no client id and always pass, even while a
	// viewer holds the lock — that is how `shipmates tell` types into a PTY.
	do(t, h, "POST", "/pty/backend/input?client=alice", "a")
	if w := do(t, h, "POST", "/pty/backend/input", "internal"); w.Code != http.StatusAccepted {
		t.Fatalf("internal write = %d, want 202", w.Code)
	}
	if !strings.Contains(f.keystrokes(), "internal") {
		t.Fatalf("internal write did not reach the pty: %q", f.keystrokes())
	}
}

func TestPTYInputWriteFailureIs500(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")
	f.mu.Lock()
	f.writeErr = errors.New("pty gone")
	f.mu.Unlock()
	if w := do(t, h, "POST", "/pty/backend/input?client=alice", "ls"); w.Code != http.StatusInternalServerError {
		t.Fatalf("= %d, want 500", w.Code)
	}
}

// TestPTYResizeOwnerWins: a read-only tab auto-fits on attach, and that fit
// must not reflow the typist's terminal out from under them. But resizing must
// never CLAIM the lock either — a look is not a claim.
func TestPTYResizeOwnerWins(t *testing.T) {
	s, h := newTestServer(t)
	p, f := attachFakePTY(t, s, "backend")

	// Nobody holds the keyboard yet: any viewer may fit.
	if w := do(t, h, "POST", "/pty/backend/resize?client=bob", `{"cols":100,"rows":40}`); w.Code != http.StatusAccepted {
		t.Fatalf("unclaimed resize = %d, want 202", w.Code)
	}
	p.mu.Lock()
	holder := p.writer
	p.mu.Unlock()
	if holder != "" {
		t.Fatalf("resize claimed the keyboard for %q; it must not", holder)
	}

	// alice types, claiming the lock. bob's fit is now rejected.
	do(t, h, "POST", "/pty/backend/input?client=alice", "a")
	if w := do(t, h, "POST", "/pty/backend/resize?client=bob", `{"cols":40,"rows":10}`); w.Code != http.StatusConflict {
		t.Fatalf("bob's resize under alice's lock = %d, want 409", w.Code)
	}
	// alice may still resize her own terminal.
	if w := do(t, h, "POST", "/pty/backend/resize?client=alice", `{"cols":120,"rows":50}`); w.Code != http.StatusAccepted {
		t.Fatalf("alice's own resize = %d, want 202", w.Code)
	}
	f.mu.Lock()
	cols, rows := f.cols, f.rows
	f.mu.Unlock()
	if cols != 120 || rows != 50 {
		t.Fatalf("pty geometry = %dx%d, want 120x50", cols, rows)
	}

	// Once alice's lease lapses, bob may fit again — a dead tab must not hold
	// the geometry hostage forever.
	p.mu.Lock()
	p.writerAt = time.Now().Add(-writerLease - time.Second)
	p.mu.Unlock()
	if w := do(t, h, "POST", "/pty/backend/resize?client=bob", `{"cols":80,"rows":24}`); w.Code != http.StatusAccepted {
		t.Fatalf("bob's resize after lease expiry = %d, want 202", w.Code)
	}
}

func TestPTYResizeRejectsNonsenseGeometry(t *testing.T) {
	s, h := newTestServer(t)
	attachFakePTY(t, s, "backend")
	for _, body := range []string{"", "{", `{"cols":0,"rows":24}`, `{"cols":80,"rows":0}`, `{"cols":-5,"rows":-5}`, `{}`} {
		if w := do(t, h, "POST", "/pty/backend/resize?client=a", body); w.Code != http.StatusBadRequest {
			t.Errorf("resize %q = %d, want 400", body, w.Code)
		}
	}
}

func TestPTYResizeFailureIs500(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")
	f.mu.Lock()
	f.sizeErr = errors.New("conpty closed")
	f.mu.Unlock()
	if w := do(t, h, "POST", "/pty/backend/resize?client=a", `{"cols":80,"rows":24}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("= %d, want 500", w.Code)
	}
}

// TestTellIntoAttachedPTY covers the "single attachment per shipmate" rule:
// when a terminal holds the persona, a tell is typed INTO that terminal with
// bracketed-paste framing instead of spawning a second claude against the same
// session. The framing matters — without it a multi-line tell executes line by
// line as the user types.
func TestTellIntoAttachedPTY(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")

	w := do(t, h, "POST", "/tell/backend", `{"message":"ship it\nplease"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("tell = %d, want 202", w.Code)
	}
	want := "\x1b[200~ship it\nplease\x1b[201~\r"
	if got := f.keystrokes(); got != want {
		t.Fatalf("pty received %q, want %q", got, want)
	}
	if !hasEventType(s, "tell") {
		t.Fatalf("tell must land in the feed, got %v", eventTypes(s))
	}
	// No headless process may have been spawned for this persona.
	s.mu.Lock()
	n := len(s.live)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("tell to an attached PTY spawned %d live procs", n)
	}
}

func TestTellIntoDeadPTYIs500(t *testing.T) {
	s, h := newTestServer(t)
	_, f := attachFakePTY(t, s, "backend")
	f.mu.Lock()
	f.writeErr = errors.New("pty gone")
	f.mu.Unlock()
	if w := do(t, h, "POST", "/tell/backend", `{"message":"hi"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("= %d, want 500", w.Code)
	}
}

// TestPTYSubscribeReplaysModesThenBacksroll: a viewer attaching after the TUI's
// startup sequence has scrolled out of the ring would otherwise land in default
// terminal mode (dead scroll wheel, wrong paste behavior).
func TestPTYSubscribeReplaysModesThenBackscroll(t *testing.T) {
	p := &ptyProc{subs: map[int]chan []byte{}, modes: map[int]bool{}, ring: newRing(64)}
	p.trackModes([]byte("\x1b[?1049h\x1b[?2004h"))
	p.ring.Write([]byte("prompt> "))

	snap, ch, cancel := p.subscribe()
	defer cancel()
	if !bytes.HasSuffix(snap, []byte("prompt> ")) {
		t.Fatalf("snapshot must end with the backscroll, got %q", snap)
	}
	if !bytes.Contains(snap, []byte("\x1b[?1049h")) || !bytes.Contains(snap, []byte("\x1b[?2004h")) {
		t.Fatalf("snapshot must be prefixed with latched modes, got %q", snap)
	}
	if idx := bytes.Index(snap, []byte("prompt>")); idx == 0 {
		t.Fatal("modes must come BEFORE the backscroll")
	}

	// Cancelling twice must not panic on a double close.
	cancel()
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel should be closed after cancel")
		}
	default:
		t.Fatal("cancelled subscriber's channel should be closed")
	}
}

func TestPTYSubscribeAfterExitClosesImmediately(t *testing.T) {
	p := &ptyProc{subs: map[int]chan []byte{}, modes: map[int]bool{}, ring: newRing(64), closed: true}
	_, ch, cancel := p.subscribe()
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("want a closed channel for a dead mate")
		}
	case <-time.After(time.Second):
		t.Fatal("subscribing to a dead mate must not hang the viewer")
	}
	p.mu.Lock()
	n := len(p.subs)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("a dead mate must not register subscribers, got %d", n)
	}
}

// TestPumpPTYDropsOldestForSlowSubscribers is the backpressure contract: a
// browser tab that stops reading must never stall the agent's PTY. If the
// fan-out ever blocks, this test hangs — which is exactly the failure being
// guarded against, so it is bounded by the outer test timeout.
func TestPumpPTYDropsOldestForSlowSubscribers(t *testing.T) {
	s, _ := newTestServer(t)
	p, f := attachFakePTY(t, s, "backend")
	s.mu.Lock()
	s.refs = 1
	s.mu.Unlock()

	_, ch, _ := p.subscribe() // never read from

	pumped := make(chan struct{})
	go func() { defer close(pumped); s.pumpPTY(p) }()

	// Far more chunks than the subscriber buffer holds.
	for i := 0; i < subBufChunks+64; i++ {
		f.feed(t, "x")
	}
	_ = f.pw.Close() // EOF retires the mate

	select {
	case <-pumped:
	case <-time.After(20 * time.Second):
		t.Fatal("pumpPTY blocked on a slow subscriber — the agent would stall behind a browser tab")
	}

	if len(ch) > subBufChunks {
		t.Fatalf("subscriber buffer grew to %d, past its %d bound", len(ch), subBufChunks)
	}

	// Retirement: the mate leaves s.ptys, flips to done, and gives back its ref.
	s.mu.Lock()
	_, stillListed := s.ptys["backend"]
	exited := s.exited["backend"]
	refs := s.refs
	s.mu.Unlock()
	if stillListed {
		t.Error("an exited PTY mate must be removed from s.ptys")
	}
	if !exited {
		t.Error("an exited PTY mate must be marked done for /status.json")
	}
	if refs != 0 {
		t.Errorf("refs = %d after the mate exited, want 0", refs)
	}
	if st := s.computeStatus(time.Now()); len(st) != 1 || st[0].Status != "done" {
		t.Errorf("status after exit = %+v, want done", st)
	}
}

// TestPumpPTYFansOutAndFillsRing checks the happy path: screen bytes reach
// both the backscroll ring and every live subscriber.
func TestPumpPTYFansOutAndFillsRing(t *testing.T) {
	s, _ := newTestServer(t)
	p, f := attachFakePTY(t, s, "backend")
	_, ch, cancel := p.subscribe()
	defer cancel()

	pumped := make(chan struct{})
	go func() { defer close(pumped); s.pumpPTY(p) }()

	f.feed(t, "\x1b[?1049hhello")
	select {
	case chunk := <-ch:
		if !bytes.Contains(chunk, []byte("hello")) {
			t.Fatalf("subscriber got %q", chunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber never received the chunk")
	}

	_ = f.pw.Close()
	<-pumped

	p.mu.Lock()
	snap := p.ring.Snapshot()
	latched := p.modes[1049]
	p.mu.Unlock()
	if !bytes.Contains(snap, []byte("hello")) {
		t.Fatalf("ring = %q", snap)
	}
	if !latched {
		t.Fatal("pump must latch DEC private modes as they stream by")
	}
}

// TestPTYStreamSSEFraming pins the wire format the browser terminal parses:
// a base64 `snapshot` event, then base64 `data` events, then `exit` on mate
// death. A framing change silently blanks every attached terminal.
func TestPTYStreamSSEFraming(t *testing.T) {
	s, h := newTestServer(t)
	p, f := attachFakePTY(t, s, "backend")
	p.mu.Lock()
	p.ring.Write([]byte("backscroll"))
	p.mu.Unlock()

	pumped := make(chan struct{})
	go func() { defer close(pumped); s.pumpPTY(p) }()

	streamed := make(chan string, 1)
	go func() {
		w := do(t, h, "GET", "/pty/backend/stream", "")
		streamed <- w.Body.String()
	}()

	// Give the stream handler time to subscribe, then push a chunk and exit.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.subs)
		p.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.feed(t, "live bytes")
	time.Sleep(50 * time.Millisecond)
	_ = f.pw.Close()
	<-pumped

	var body string
	select {
	case body = <-streamed:
	case <-time.After(10 * time.Second):
		t.Fatal("SSE handler did not return after the mate exited")
	}

	if !strings.Contains(body, "event: snapshot\ndata: "+base64.StdEncoding.EncodeToString([]byte("backscroll"))) {
		t.Fatalf("missing base64 snapshot event: %q", body)
	}
	if !strings.Contains(body, "event: data\ndata: "+base64.StdEncoding.EncodeToString([]byte("live bytes"))) {
		t.Fatalf("missing base64 data event: %q", body)
	}
	if !strings.HasSuffix(body, "event: exit\ndata: \n\n") {
		t.Fatalf("stream must end with an exit event: %q", body)
	}
}

func TestClosePTYsClosesEveryMate(t *testing.T) {
	s, _ := newTestServer(t)
	_, f1 := attachFakePTY(t, s, "backend")
	_, f2 := attachFakePTY(t, s, "frontend")

	s.closePTYs()

	for name, f := range map[string]*fakePty{"backend": f1, "frontend": f2} {
		f.mu.Lock()
		closed := f.closed
		f.mu.Unlock()
		if !closed {
			t.Errorf("%s's pty was not closed on shutdown", name)
		}
	}
}

// TestComputeStatusCountsPTYMates: a PTY-hosted mate has no entry in s.live,
// so if computeStatus only consulted s.live an attached terminal would show
// as "off" in the fleet.
func TestComputeStatusCountsPTYMates(t *testing.T) {
	s, _ := newTestServer(t)
	attachFakePTY(t, s, "backend")
	now := time.Now()
	s.mu.Lock()
	s.lastSeen["backend"] = now.Add(-2 * time.Second)
	s.mu.Unlock()

	out := s.computeStatus(now)
	if len(out) != 1 || out[0].Status != "working" {
		t.Fatalf("= %+v, want backend working", out)
	}

	s.mu.Lock()
	s.lastSeen["backend"] = now.Add(-workingWindow - time.Second)
	s.mu.Unlock()
	if out := s.computeStatus(now); out[0].Status != "idle" {
		t.Fatalf("= %+v, want idle past the working window", out)
	}
}
