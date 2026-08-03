package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeShip stands in for the ship's coordination server: the same routes
// internal/server registers, with the same status codes and the same
// single-writer semantics, so the whole client path is exercised without
// spawning claude under a real PTY.
type fakeShip struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	roster    []MateStatus
	pendings  []Pending
	ptys      map[string]*fakePTY
	lockedBy  map[string]string // persona -> client id holding the writer lock
	inputs    []inputCall
	resizes   []resizeCall
	resolves  []resolveCall
	takeovers []string
	releases  []string
	starts    []string
	statusErr int // when non-zero, /status.json answers this code
}

type fakePTY struct {
	ring   []byte
	subs   map[int]chan []byte
	nextID int
	closed bool
}

type inputCall struct {
	Persona string
	Client  string
	Data    string
}

type resizeCall struct {
	Persona    string
	Cols, Rows int
}

type resolveCall struct {
	ID       string
	Behavior string
	Duration string
}

func newFakeShip(t *testing.T) *fakeShip {
	t.Helper()
	f := &fakeShip{
		t:        t,
		ptys:     map[string]*fakePTY{},
		lockedBy: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /status.json", f.handleStatus)
	mux.HandleFunc("GET /pending.json", f.handlePending)
	mux.HandleFunc("POST /resolve/{id}", f.handleResolve)
	mux.HandleFunc("POST /pty/{persona}/start", f.handleStart)
	mux.HandleFunc("GET /pty/{persona}/snapshot", f.handleSnapshot)
	mux.HandleFunc("GET /pty/{persona}/stream", f.handleStream)
	mux.HandleFunc("POST /pty/{persona}/input", f.handleInput)
	mux.HandleFunc("POST /pty/{persona}/resize", f.handleResize)
	mux.HandleFunc("POST /pty/{persona}/takeover", f.handleTakeover)
	mux.HandleFunc("POST /pty/{persona}/release", f.handleRelease)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeShip) base() string { return f.srv.URL }

// --- test-side controls -----------------------------------------------------

func (f *fakeShip) setRoster(list ...MateStatus) {
	f.mu.Lock()
	f.roster = list
	f.mu.Unlock()
}

func (f *fakeShip) setPending(list ...Pending) {
	f.mu.Lock()
	f.pendings = list
	f.mu.Unlock()
}

// spawnPTY pre-creates a PTY mate with initial ring content, the state the
// server is in when a browser tab (or another bridge) already attached.
func (f *fakeShip) spawnPTY(persona, ring string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ptys[persona] = &fakePTY{ring: []byte(ring), subs: map[int]chan []byte{}}
}

// push broadcasts screen bytes to every subscriber, like pumpPTY does.
func (f *fakeShip) push(persona, data string) {
	f.mu.Lock()
	p := f.ptys[persona]
	if p == nil {
		f.mu.Unlock()
		f.t.Fatalf("push to unknown pty %q", persona)
	}
	p.ring = append(p.ring, data...)
	chans := make([]chan []byte, 0, len(p.subs))
	for _, ch := range p.subs {
		chans = append(chans, ch)
	}
	f.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- []byte(data):
		case <-time.After(time.Second):
			f.t.Errorf("subscriber for %q did not accept a frame", persona)
		}
	}
}

// exitPTY ends the mate: subscribers get an exit event and the PTY disappears,
// so later requests 404 exactly as the real server's do.
func (f *fakeShip) exitPTY(persona string) {
	f.mu.Lock()
	p := f.ptys[persona]
	if p == nil {
		f.mu.Unlock()
		return
	}
	p.closed = true
	for id, ch := range p.subs {
		close(ch)
		delete(p.subs, id)
	}
	delete(f.ptys, persona)
	f.mu.Unlock()
}

// lock makes the persona's writer lock held by someone else, so our input 409s.
func (f *fakeShip) lock(persona, client string) {
	f.mu.Lock()
	f.lockedBy[persona] = client
	f.mu.Unlock()
}

// unlock drops the writer lock, whoever holds it.
func (f *fakeShip) unlock(persona string) {
	f.mu.Lock()
	delete(f.lockedBy, persona)
	f.mu.Unlock()
}

func (f *fakeShip) snapshot() (inputs []inputCall, resizes []resizeCall, resolves []resolveCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inputCall(nil), f.inputs...),
		append([]resizeCall(nil), f.resizes...),
		append([]resolveCall(nil), f.resolves...)
}

func (f *fakeShip) inputText(persona string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ""
	for _, c := range f.inputs {
		if c.Persona == persona {
			out += c.Data
		}
	}
	return out
}

func (f *fakeShip) counts() (starts, takeovers, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts), len(f.takeovers), len(f.releases)
}

// --- handlers ---------------------------------------------------------------

func (f *fakeShip) handleStatus(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	code, out := f.statusErr, append([]MateStatus(nil), f.roster...)
	f.mu.Unlock()
	if code != 0 {
		http.Error(w, "boom", code)
		return
	}
	writeJSON(w, out)
}

func (f *fakeShip) handlePending(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	out := append([]Pending(nil), f.pendings...)
	f.mu.Unlock()
	writeJSON(w, out)
}

func (f *fakeShip) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Behavior string `json:"behavior"`
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		http.Error(w, "behavior must be allow|deny", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	f.mu.Lock()
	f.resolves = append(f.resolves, resolveCall{ID: id, Behavior: body.Behavior, Duration: body.Duration})
	kept := f.pendings[:0:0]
	found := false
	for _, p := range f.pendings {
		if p.ID == id {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	f.pendings = kept
	f.mu.Unlock()
	if !found {
		http.Error(w, "no such pending request", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeShip) handleStart(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	f.mu.Lock()
	f.starts = append(f.starts, persona)
	if f.ptys[persona] == nil {
		f.ptys[persona] = &fakePTY{
			ring: []byte("\x1b[?1049h\x1b[?2004h" + persona + " ready\r\n"),
			subs: map[int]chan []byte{},
		}
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

// handleSnapshot mirrors the real server: raw ring bytes and, notably, NO
// latched-mode prefix. The bridge relies on that difference.
func (f *fakeShip) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	p := f.ptys[r.PathValue("persona")]
	var ring []byte
	if p != nil {
		ring = append([]byte(nil), p.ring...)
	}
	f.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(ring)
}

func (f *fakeShip) handleStream(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	f.mu.Lock()
	p := f.ptys[persona]
	if p == nil {
		f.mu.Unlock()
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	// The stream's snapshot event carries the mode prefix ahead of the ring —
	// the whole reason the bridge re-primes from it.
	snapshot := append([]byte("\x1b[?2004h"), p.ring...)
	ch := make(chan []byte, 64)
	id := p.nextID
	p.nextID++
	p.subs[id] = ch
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		if pp := f.ptys[persona]; pp != nil {
			if c, ok := pp.subs[id]; ok {
				delete(pp.subs, id)
				close(c)
			}
		}
		f.mu.Unlock()
	}()

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	emit := func(event string, b []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, base64.StdEncoding.EncodeToString(b)); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !emit("snapshot", snapshot) {
		return
	}
	for {
		select {
		case chunk, open := <-ch:
			if !open {
				_, _ = fmt.Fprint(w, "event: exit\ndata: \n\n")
				fl.Flush()
				return
			}
			if !emit("data", chunk) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeShip) handleInput(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	client := r.URL.Query().Get("client")
	f.mu.Lock()
	p := f.ptys[persona]
	holder := f.lockedBy[persona]
	f.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	if holder != "" && client != "" && holder != client {
		http.Error(w, "another viewer holds the keyboard", http.StatusConflict)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || len(body) == 0 {
		http.Error(w, "empty input", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	if holder == "" && client != "" {
		f.lockedBy[persona] = client // typing claims the lock, as the server does
	}
	f.inputs = append(f.inputs, inputCall{Persona: persona, Client: client, Data: string(body)})
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeShip) handleResize(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	var body struct{ Cols, Rows int }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
		body.Cols <= 0 || body.Rows <= 0 {
		http.Error(w, "want {cols, rows} > 0", http.StatusBadRequest)
		return
	}
	client := r.URL.Query().Get("client")
	f.mu.Lock()
	p := f.ptys[persona]
	holder := f.lockedBy[persona]
	if p != nil && !(holder != "" && client != "" && holder != client) {
		f.resizes = append(f.resizes, resizeCall{Persona: persona, Cols: body.Cols, Rows: body.Rows})
	}
	f.mu.Unlock()
	switch {
	case p == nil:
		http.Error(w, "no pty mate", http.StatusNotFound)
	case holder != "" && client != "" && holder != client:
		http.Error(w, "another viewer holds the keyboard", http.StatusConflict)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (f *fakeShip) handleTakeover(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	client := r.URL.Query().Get("client")
	if client == "" {
		http.Error(w, "want ?client=<id>", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	p := f.ptys[persona]
	if p != nil {
		f.lockedBy[persona] = client // unconditional steal, matching the server
		f.takeovers = append(f.takeovers, persona)
	}
	f.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (f *fakeShip) handleRelease(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	client := r.URL.Query().Get("client")
	f.mu.Lock()
	p := f.ptys[persona]
	if p != nil {
		if client != "" && f.lockedBy[persona] == client {
			delete(f.lockedBy, persona)
		}
		f.releases = append(f.releases, persona)
	}
	f.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
