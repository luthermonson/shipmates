package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a Server sandboxed in a fresh temp directory and hands
// back the REAL route table from routes(). Chdir'ing first matters: nearly
// every handler resolves cwd-relative state (persona frontmatter under
// .claude/agents, shipmates.yaml, .beads/), so without the sandbox these tests
// would read the developer's actual checkout and pass or fail by accident.
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	t.Chdir(t.TempDir())
	s := New()
	return s, s.routes()
}

// do issues one request against a handler and returns the recorder.
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHealthEndpoint(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, "GET", "/health", "")
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("GET /health = %d %q, want 200 \"ok\"", w.Code, w.Body.String())
	}
}

// TestRouteMethodConstraints pins the method half of every route pattern. The
// mux matches on "METHOD /path", so a route registered POST-only must 405 a
// GET — that is the only thing stopping a browser preflight or a stray link
// from firing /shutdown or /resolve.
func TestRouteMethodConstraints(t *testing.T) {
	_, h := newTestServer(t)
	cases := []struct {
		method, path string
	}{
		{"POST", "/health"},
		{"GET", "/shutdown"},
		{"GET", "/resolve/abc"},
		{"GET", "/tell/backend"},
		{"GET", "/attach"},
		{"GET", "/register"},
		{"GET", "/deregister"},
		{"GET", "/bead"},
		{"POST", "/pending"},
		{"POST", "/pending.json"},
		{"POST", "/status.json"},
		{"POST", "/feed"},
		{"GET", "/pty/backend/input"},
		{"POST", "/pty/backend/snapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := do(t, h, tc.method, tc.path, "")
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 405", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	_, h := newTestServer(t)
	for _, p := range []string{"/", "/nope", "/pty/backend", "/bead/x/delete"} {
		if w := do(t, h, "GET", p, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, w.Code)
		}
	}
}

func TestPostEventsRecordsAndStamps(t *testing.T) {
	s, h := newTestServer(t)

	// The server stamps Time itself and must ignore whatever the caller sent,
	// otherwise a hook could backdate its own events out of the feed window.
	w := do(t, h, "POST", "/events", `{"persona":"backend","type":"note","text":"hi","time":"1999-01-01T00:00:00Z"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST /events = %d, want 204", w.Code)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(s.events))
	}
	e := s.events[0]
	if e.Persona != "backend" || e.Type != "note" || e.Text != "hi" {
		t.Fatalf("event round-trip lost fields: %+v", e)
	}
	if strings.HasPrefix(e.Time, "1999") {
		t.Fatalf("caller-supplied Time must be overwritten, got %q", e.Time)
	}
	if _, err := time.Parse(time.RFC3339, e.Time); err != nil {
		t.Fatalf("Time %q is not RFC3339: %v", e.Time, err)
	}
	if s.lastSeen["backend"].IsZero() {
		t.Fatal("posting an event must refresh the persona's lastSeen")
	}
	if s.lastActivity.IsZero() {
		t.Fatal("posting an event must refresh lastActivity (idle watchdog input)")
	}
}

func TestPostEventsBadBody(t *testing.T) {
	_, h := newTestServer(t)
	for _, body := range []string{"", "not json", "[1,2,3]"} {
		w := do(t, h, "POST", "/events", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /events %q = %d, want 400", body, w.Code)
		}
	}
}

// TestEmptyPersonaEventDoesNotCreateLastSeenKey guards /status.json: an
// event with no persona (an attach:received, say) must not conjure a
// nameless mate into the roster.
func TestEmptyPersonaEventDoesNotCreateLastSeenKey(t *testing.T) {
	s, h := newTestServer(t)
	do(t, h, "POST", "/events", `{"type":"attach:received","text":"x"}`)
	s.mu.Lock()
	_, ok := s.lastSeen[""]
	s.mu.Unlock()
	if ok {
		t.Fatal(`an event with no persona must not create a "" lastSeen entry`)
	}
	if got := s.computeStatus(time.Now()); len(got) != 0 {
		t.Fatalf("roster should stay empty, got %+v", got)
	}
}

func TestGetEventsJSONSnapshotIsACopy(t *testing.T) {
	s, h := newTestServer(t)
	s.addEvent(Event{Persona: "a", Type: "assistant", Text: "one"})
	s.addEvent(Event{Persona: "b", Type: "result", Text: "two", CostUSD: 0.5, DurationMS: 1200, Model: "opus"})

	w := do(t, h, "GET", "/events", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /events = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var out []Event
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	if len(out) != 2 || out[0].Text != "one" || out[1].Model != "opus" || out[1].CostUSD != 0.5 {
		t.Fatalf("bad payload: %+v", out)
	}

	// Mutating the server's slice afterwards must not be visible in the
	// already-serialized response — the handler copies under the lock.
	s.addEvent(Event{Persona: "c", Type: "assistant", Text: "three"})
	var again []Event
	_ = json.Unmarshal(w.Body.Bytes(), &again)
	if len(again) != 2 {
		t.Fatalf("earlier response mutated to %d events", len(again))
	}
}

func TestGetEventsJSONEmptyIsArrayNotNull(t *testing.T) {
	// The fleet ingests this straight into a JS array; `null` would break it.
	_, h := newTestServer(t)
	w := do(t, h, "GET", "/events", "")
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Fatalf("empty feed should serialize as [], got %q", got)
	}
}

func TestFeedRendersOneLinePerEvent(t *testing.T) {
	s, h := newTestServer(t)
	s.addEvent(Event{Persona: "backend", Type: "assistant", Text: "shipping it"})
	s.addEvent(Event{Persona: "security", Type: "permission?", Text: "wants Bash"})

	w := do(t, h, "GET", "/feed", "")
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), w.Body.String())
	}
	if !strings.Contains(lines[0], "backend/assistant: shipping it") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "security/permission?: wants Bash") {
		t.Errorf("line 1 = %q", lines[1])
	}
}

// TestRegisterDeregisterRefcount covers the lifecycle counter the idle
// watchdog and `shipmates` client sessions depend on. The floor at zero is the
// important part: a duplicate deregister (a client that crashed and restarted)
// must not push refs negative and make the captain immortal.
func TestRegisterDeregisterRefcount(t *testing.T) {
	s, h := newTestServer(t)

	for i := 0; i < 3; i++ {
		if w := do(t, h, "POST", "/register", ""); w.Code != http.StatusNoContent {
			t.Fatalf("register %d = %d, want 204", i, w.Code)
		}
	}
	s.mu.Lock()
	refs := s.refs
	s.mu.Unlock()
	if refs != 3 {
		t.Fatalf("refs = %d after 3 registers, want 3", refs)
	}

	for i := 0; i < 5; i++ {
		if w := do(t, h, "POST", "/deregister", ""); w.Code != http.StatusNoContent {
			t.Fatalf("deregister %d = %d, want 204", i, w.Code)
		}
	}
	s.mu.Lock()
	refs = s.refs
	s.mu.Unlock()
	if refs != 0 {
		t.Fatalf("refs = %d after over-deregistering, want a floor of 0", refs)
	}
}

func TestRegisterRefreshesLastActivity(t *testing.T) {
	s, h := newTestServer(t)
	s.mu.Lock()
	s.lastActivity = time.Now().Add(-time.Hour)
	s.mu.Unlock()

	do(t, h, "POST", "/register", "")

	s.mu.Lock()
	idle := time.Since(s.lastActivity)
	s.mu.Unlock()
	if idle > time.Minute {
		t.Fatalf("register left lastActivity %s stale; the idle watchdog would reap a live session", idle)
	}
}

// TestRegisterConcurrency hammers the ref-count from many goroutines. The
// counter is the captain's liveness signal — a lost increment eventually
// shows up as a captain that shuts down under a live client.
func TestRegisterConcurrency(t *testing.T) {
	s, h := newTestServer(t)
	const n = 200
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			do(t, h, "POST", "/register", "")
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	s.mu.Lock()
	refs := s.refs
	s.mu.Unlock()
	if refs != n {
		t.Fatalf("refs = %d after %d concurrent registers, want %d", refs, n, n)
	}
}

// TestShutdownIsIdempotent is the regression guard on stopOnce: /shutdown is
// fired by the CLI, by the fleet, and by the idle watchdog, so a second close
// of stopCh is entirely plausible — and would panic the process.
func TestShutdownIsIdempotent(t *testing.T) {
	s, h := newTestServer(t)
	for i := 0; i < 3; i++ {
		w := do(t, h, "POST", "/shutdown", "")
		if w.Code != http.StatusAccepted {
			t.Fatalf("shutdown %d = %d, want 202", i, w.Code)
		}
	}
	select {
	case <-s.stopCh:
	default:
		t.Fatal("stopCh should be closed after /shutdown")
	}
}

func TestPendingEndpointsEmpty(t *testing.T) {
	_, h := newTestServer(t)
	if got := strings.TrimSpace(do(t, h, "GET", "/pending", "").Body.String()); got != "(none)" {
		t.Errorf("GET /pending with nothing waiting = %q, want (none)", got)
	}
	// The JSON form must be [] rather than null so the fleet can range over it.
	if got := strings.TrimSpace(do(t, h, "GET", "/pending.json", "").Body.String()); got != "[]" {
		t.Errorf("GET /pending.json empty = %q, want []", got)
	}
}

func TestPendingEndpointsPopulated(t *testing.T) {
	s, h := newTestServer(t)
	s.mu.Lock()
	s.pendings["ab12"] = &pending{id: "ab12", persona: "security", tool: "Bash", input: "rm -rf /", ch: make(chan string, 1)}
	s.mu.Unlock()

	text := do(t, h, "GET", "/pending", "").Body.String()
	if !strings.Contains(text, "ab12") || !strings.Contains(text, "security wants Bash") {
		t.Errorf("GET /pending = %q", text)
	}

	var out []struct {
		ID      string `json:"id"`
		Persona string `json:"persona"`
		Tool    string `json:"tool"`
		Input   string `json:"input"`
	}
	body := do(t, h, "GET", "/pending.json", "").Body.Bytes()
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out) != 1 || out[0].ID != "ab12" || out[0].Input != "rm -rf /" {
		t.Fatalf("bad pending.json: %+v", out)
	}
}

func TestStatusJSONEndToEnd(t *testing.T) {
	s, h := newTestServer(t)
	s.mu.Lock()
	s.live["backend"] = &liveProc{persona: "backend"}
	s.lastSeen["backend"] = time.Now()
	s.exited["tester"] = true
	s.mu.Unlock()

	w := do(t, h, "GET", "/status.json", "")
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var out []MateStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	got := map[string]string{}
	for _, st := range out {
		got[st.Persona] = st.Status
	}
	if got["backend"] != "working" || got["tester"] != "done" {
		t.Fatalf("status = %v", got)
	}
}

func TestTellRejectsEmptyOrMalformedMessage(t *testing.T) {
	// A tell with no message must never reach ensureLive — spawning a claude
	// process to say nothing is the expensive failure mode here.
	_, h := newTestServer(t)
	for _, body := range []string{"", "{}", `{"message":""}`, "garbage"} {
		w := do(t, h, "POST", "/tell/backend", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /tell with %q = %d, want 400", body, w.Code)
		}
	}
}
