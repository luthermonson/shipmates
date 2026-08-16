package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// --- M2: one bead-id guard, not two -----------------------------------------

// TestBeadIDGuardRejectsParentHop is the divergence the issue names: the
// ship-side copy of beadIDOK accepted ".." long after the fleet-side copy had
// been taught to reject it. Both now call internal/beadid.
func TestBeadIDGuardRejectsParentHop(t *testing.T) {
	for _, id := range []string{"..", "...", "a..b", "proj-c03..", "..proj-c03"} {
		if beadIDOK(id) {
			t.Errorf("beadIDOK(%q) = true, want false", id)
		}
	}
	// The ids bd actually mints must keep working — including dotted epics,
	// which is why "." stays in the alphabet and only the ".." sequence goes.
	for _, id := range []string{"proj-c03", "proj-a3f8.1", "proj-a3f8.1.2", "a_b", "A1", "0"} {
		if !beadIDOK(id) {
			t.Errorf("beadIDOK(%q) = false, want true", id)
		}
	}
}

// TestBeadRoutesRejectParentHop: the guard is wired into every bead route that
// takes an id, and refuses before the exec.
func TestBeadRoutesRejectParentHop(t *testing.T) {
	_, h := newTestServer(t)
	enableBeads(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/bead/%2e%2e", ""},
		{"POST", "/bead/%2e%2e/close", `{}`},
		{"POST", "/bead/%2e%2e/update", `{"priority":"1"}`},
	}
	for _, tc := range cases {
		w := do(t, h, tc.method, tc.path, tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", tc.method, tc.path, w.Code)
		}
	}
}

// --- M8: bounded request bodies ---------------------------------------------

// TestJSONBodiesAreBounded: every JSON-decoding handler caps the body it will
// read. Before this, a client (hostile or merely stuck) could stream unbounded
// bytes into json.Decoder — /attach was the only endpoint that said no.
func TestJSONBodiesAreBounded(t *testing.T) {
	// No `claude` on PATH: if the cap ever regresses, /tell must fail on the
	// spawn rather than starting a real agent in a temp directory.
	t.Setenv("PATH", t.TempDir())
	_, h := newTestServer(t)
	enableBeads(t)
	huge := `{"message":"` + strings.Repeat("A", int(maxJSONBody)+1024) + `"}`
	cases := []struct{ name, method, path string }{
		{"tell", "POST", "/tell/backend"},
		{"events", "POST", "/events"},
		{"resolve", "POST", "/resolve/abcd1234"},
		{"bead create", "POST", "/bead"},
		{"bead update", "POST", "/bead/proj-c03/update"},
		{"pty resize", "POST", "/pty/backend/resize"},
	}
	for _, tc := range cases {
		w := do(t, h, tc.method, tc.path, huge)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: %s %s with an oversized body = %d, want 413",
				tc.name, tc.method, tc.path, w.Code)
		}
	}
}

// TestPTYInputIsBounded: keystrokes are raw bytes, not JSON, and used to be
// read with io.LimitReader — which silently truncates. A half-delivered paste
// is a half-typed command in a live agent's terminal, so it is refused.
func TestPTYInputIsBounded(t *testing.T) {
	_, h := newTestServer(t)
	w := do(t, h, "POST", "/pty/backend/input", strings.Repeat("k", int(maxPTYInputBody)+1))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized pty input = %d, want 413", w.Code)
	}
}

// TestHookBodyIsBounded: the hook handler deliberately ignores a decode error
// (a malformed hook is still recorded rather than dropped), so the observable
// effect of the cap is that an over-cap payload does not decode — the bytes
// were never all read — while the request still answers 200.
func TestHookBodyIsBounded(t *testing.T) {
	s, h := newTestServer(t)
	huge := `{"tool_name":"Read","tool_input":{"file_path":"` +
		strings.Repeat("A", int(maxHookBody)+1024) + `"}}`
	w := do(t, h, "POST", "/hook/backend/PostToolUse", huge)
	if w.Code != http.StatusOK {
		t.Fatalf("oversized hook = %d, want 200 (hooks fail soft)", w.Code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(s.events))
	}
	if s.events[0].Text != "(no tool_name)" {
		t.Fatalf("an over-cap hook body was decoded anyway: %+v", s.events[0])
	}
}

// --- M8: bounded event log --------------------------------------------------

// TestEventLogIsCapped: s.events used to grow for the life of the process, and
// a fleeted captain never exits on idle. The log now keeps the newest
// eventLogCap entries and drops the oldest, the same rule ring.go applies to
// PTY bytes.
func TestEventLogIsCapped(t *testing.T) {
	s, _ := newTestServer(t)
	const extra = 10
	for i := 0; i < eventLogCap+extra; i++ {
		s.addEvent(Event{Persona: "backend", Type: "note", Text: strconv.Itoa(i)})
	}
	s.mu.Lock()
	got := len(s.events)
	first, last := s.events[0].Text, s.events[len(s.events)-1].Text
	capacity := cap(s.events)
	s.mu.Unlock()

	if got != eventLogCap {
		t.Fatalf("event log holds %d, want %d", got, eventLogCap)
	}
	// Newest kept, oldest dropped.
	if first != strconv.Itoa(extra) {
		t.Errorf("oldest kept event = %q, want %q", first, strconv.Itoa(extra))
	}
	if last != strconv.Itoa(eventLogCap+extra-1) {
		t.Errorf("newest event = %q, want %q", last, strconv.Itoa(eventLogCap+extra-1))
	}
	// The backing array is bounded too — a slice header walking forward
	// through an ever-growing allocation would pass the length check while
	// still leaking memory.
	if capacity > 4*eventLogCap {
		t.Errorf("backing array cap = %d, unbounded growth", capacity)
	}
}

// TestEventsEndpointServesTheCappedLog: what the fleet polls is the capped
// feed, so the mirror sees a bounded response rather than a body that grows
// without limit for the life of the ship.
func TestEventsEndpointServesTheCappedLog(t *testing.T) {
	s, h := newTestServer(t)
	for i := 0; i < eventLogCap+5; i++ {
		s.addEvent(Event{Persona: "backend", Type: "note", Text: strconv.Itoa(i)})
	}
	w := do(t, h, "GET", "/events", "")
	var out []Event
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != eventLogCap {
		t.Fatalf("GET /events returned %d events, want %d", len(out), eventLogCap)
	}
	if out[len(out)-1].Text != strconv.Itoa(eventLogCap+4) {
		t.Fatalf("newest event missing from /events: %q", out[len(out)-1].Text)
	}
}

// --- L5: bounded remote strings into the beads graph ------------------------

// The characters L5 is about, named and built from code points rather than
// pasted in — a test file that carries a literal bidi override is exactly the
// hazard the rule exists to stop.
var (
	esc      = string(rune(0x1B))   // starts an ANSI control sequence
	rtlOver  = string(rune(0x202E)) // reverses the visual order of what follows
	zeroWide = string(rune(0x200B)) // invisible, but changes what a string is
	lineSep  = string(rune(0x2028)) // a line break to a JS consumer, not to Go
)

// TestSafeTextBounds is the unit-level rule. These strings become bd flag
// values and then land in the shared graph, the fleet UI, and other agents'
// prompts, so the concern is rendering and volume, not shell quoting — there
// is no shell on this path and every value rides flag=value form.
func TestSafeTextBounds(t *testing.T) {
	if _, ok := safeText(strings.Repeat("a", 11), 10, false); ok {
		t.Error("over-long value accepted")
	}
	if got, ok := safeText("  spaced  ", 10, false); !ok || got != "spaced" {
		t.Errorf("safeText trimmed to %q ok=%v", got, ok)
	}
	rejected := []struct{ v, why string }{
		{"a\rb", "CR rewrites the line above it in a terminal"},
		{"a\nb", "newline in a single-line field"},
		{"a\tb", "tab in a single-line field"},
		{"a" + esc + "[2Jb", "ANSI escape"},
		{"a\x00b", "NUL"},
		{"a" + rtlOver + "b", "bidi override"},
		{"a" + zeroWide + "b", "zero-width space"},
		{"a" + lineSep + "b", "JS line separator"},
	}
	for _, tc := range rejected {
		if _, ok := safeText(tc.v, 100, false); ok {
			t.Errorf("safeText(%q) accepted — %s", tc.v, tc.why)
		}
	}
	// A description is markdown: newlines and tabs are content there. Escapes
	// and bidi overrides still are not.
	if got, ok := safeText("line one\n\tindented", 100, true); !ok || got != "line one\n\tindented" {
		t.Errorf("multiline value mangled: %q ok=%v", got, ok)
	}
	if _, ok := safeText("desc\n"+esc+"[2J", 100, true); ok {
		t.Error("multiline mode accepted an ANSI escape")
	}
	// Everything a control character is not stays untouched.
	ordinary := "fix: don't panic — see §4 (émoji \U0001F6A2 ok)"
	if got, ok := safeText(ordinary, 100, false); !ok || got != ordinary {
		t.Errorf("ordinary text rejected: %q ok=%v", got, ok)
	}
}

// TestBeadFieldsAreBounded: the rule is wired into every bd flag value the
// handlers build from a remote string, and refuses before the exec. The
// hostile characters ride as JSON \u escapes, which is exactly how they would
// arrive from the fleet.
func TestBeadFieldsAreBounded(t *testing.T) {
	_, h := newTestServer(t)
	enableBeads(t)
	cases := []struct{ name, method, path, body string }{
		{"create title too long", "POST", "/bead",
			`{"title":"` + strings.Repeat("a", beadTitleMax+1) + `"}`},
		{"create title with CR", "POST", "/bead", `{"title":"a\rb"}`},
		{"create title with an ANSI escape", "POST", "/bead", `{"title":"a\u001b[2Jb"}`},
		{"create description too long", "POST", "/bead",
			`{"title":"ok","description":"` + strings.Repeat("d", beadDescMax+1) + `"}`},
		{"create external_ref with newline", "POST", "/bead", `{"title":"ok","external_ref":"a\nb"}`},
		{"update assignee too long", "POST", "/bead/proj-c03/update",
			`{"assignee":"` + strings.Repeat("x", beadAssigneeMax+1) + `"}`},
		{"update assignee with an ANSI escape", "POST", "/bead/proj-c03/update",
			`{"assignee":"a\u001b[2Jb"}`},
		{"update title with a bidi override", "POST", "/bead/proj-c03/update",
			`{"title":"a\u202eb"}`},
		{"close reason too long", "POST", "/bead/proj-c03/close",
			`{"reason":"` + strings.Repeat("r", beadReasonMax+1) + `"}`},
	}
	for _, tc := range cases {
		w := do(t, h, tc.method, tc.path, tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %s %s = %d, want 400", tc.name, tc.method, tc.path, w.Code)
		}
	}
}
