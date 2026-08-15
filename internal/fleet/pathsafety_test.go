package fleet

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/personaname"
)

// These tests cover H1/H2 of the security review: the fleet builds the
// captain-side request line by hand, and every identifier it interpolates
// arrives percent-DECODED (r.PathValue) or straight out of LLM tool-call JSON.
// A segment carrying CR/LF or a space rewrites the request the captain
// receives, promoting the caller from the curated /api/* subset to any
// endpoint on the captain's (unauthenticated) local server — /shutdown,
// /pty/{persona}/start, /attach.
//
// The fix is two-layered and so are these tests: identifiers are validated at
// each entry point (400, or a tool error, and never reaching proxy), every
// segment is url.PathEscape'd at construction so the escape holds if a future
// caller forgets to validate, and proxy itself refuses a framing path as a
// backstop.

// routableHostile are decoded identifiers that ServeMux will still route as a
// single path segment — no "/", and nothing that trips ServeMux's path
// cleaning — so the handler itself is what must say no.
var routableHostile = []struct{ name, val, why string }{
	{"CRLF header injection", "data\r\nX-Injected: 1", "CR/LF ends the request line"},
	{"bare CR", "data\rx", "lenient parsers treat a bare CR as a line terminator"},
	{"bare LF", "data\nx", "LF ends the request line"},
	{"space", "data x", "a space splits the request target"},
	{"tab", "data\tx", "control character in the request line"},
	{"query separator", "data?x=1", "grafts a query onto the proxied path"},
	{"fragment separator", "data#x", "truncates the proxied path"},
	{"embedded traversal", "a..b", "traversal out of the interpolated segment"},
}

// hostileIdentifiers is routableHostile plus payloads that only reach a
// handler through a body or a tool call (they carry "/" or a leading "..",
// which ServeMux would rewrite before the handler ever ran).
var hostileIdentifiers = append(append([]struct{ name, val, why string }{},
	routableHostile...),
	struct{ name, val, why string }{"smuggled second request", "data\r\nPOST /shutdown HTTP/1.1\r\nHost: captain\r\n\r\n", "injects a whole extra request"},
	struct{ name, val, why string }{"slash escapes the segment", "data/../shutdown", "walks out of /tell/<persona>"},
	struct{ name, val, why string }{"parent directory hop", "..", "traversal"},
)

// wireHostile are the same attacks as they cross the wire: percent-encoded, so
// they survive as one segment and are decoded by ServeMux into the values
// above. This is the shape an attacker actually sends.
var wireHostile = []struct{ name, seg, why string }{
	{"encoded CRLF", "data%0d%0aX-Injected:%201", "CR/LF ends the request line"},
	{"encoded CRLF second request", "data%0d%0aPOST%20%2fshutdown%20HTTP%2f1.1%0d%0a%0d%0a", "injects a whole extra request"},
	{"encoded space", "data%20%2fshutdown", "a space splits the request target"},
	{"encoded slash", "data%2f..%2fshutdown", "walks out of the interpolated segment"},
	{"encoded traversal", "%2e%2e%2f%2e%2e%2fetc", "traversal"},
	{"encoded tab", "data%09x", "control character in the request line"},
}

// TestServeMuxDecodesPathValues is the premise of every test below: what an
// attacker types as %0d%0a reaches the handler as a real CRLF, so a handler
// that concatenates r.PathValue into a request line is writing attacker bytes
// into HTTP framing. If this ever stops being true the validation below is
// still correct, just less load-bearing.
func TestServeMuxDecodesPathValues(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", func(w http.ResponseWriter, r *http.Request) {
		got = r.PathValue("persona")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	if code := postRaw(t, ts, "/api/captain/homelab:captain/tell/data%0d%0aPOST%20%2fshutdown%20HTTP%2f1.1", ""); code != http.StatusOK {
		t.Fatalf("mux did not route the encoded segment: %d", code)
	}
	if !strings.Contains(got, "\r\n") || !strings.Contains(got, "/shutdown") {
		t.Fatalf("expected a decoded CRLF payload, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// backstop: proxy itself
// ---------------------------------------------------------------------------

func TestCheckProxyPath(t *testing.T) {
	good := []string{
		"/tell/data",
		"/beads.json?all=1&limit=5",
		"/pty/data/start?client=w1",
		"/bead/abc123/update",
		"/tell/data%0d%0a", // still-encoded: harmless, it never leaves one segment
	}
	for _, p := range good {
		if err := checkProxyPath(p); err != nil {
			t.Errorf("checkProxyPath(%q) = %v, want nil", p, err)
		}
	}

	bad := []struct{ path, why string }{
		{"/tell/data\r\nGET /shutdown HTTP/1.1\r\n\r\n", "CRLF injects a second request"},
		{"/tell/data\r", "bare CR"},
		{"/tell/data\n", "bare LF"},
		{"/tell/my persona", "space splits the request target"},
		{"/tell/data\t", "tab"},
		{"/tell/data\x00", "NUL"},
		{"/tell/data\x7f", "DEL"},
		{"tell/data", "not rooted — reads as a continuation of the method"},
		{"", "empty"},
	}
	for _, tc := range bad {
		if err := checkProxyPath(tc.path); err == nil {
			t.Errorf("checkProxyPath(%q) = nil, want an error (%s)", tc.path, tc.why)
		}
	}
}

// A hostile path must fail at proxy() with an error — not be silently
// stripped, and above all not be written to the ship.
func TestProxy_RejectsFramingPathBeforeDialing(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	for _, tc := range []struct{ name, path string }{
		{"CRLF smuggled second request", "/tell/data\r\nPOST /shutdown HTTP/1.1\r\nHost: captain\r\n\r\n"},
		{"space re-targets the request", "/tell/data /shutdown"},
		{"bare LF", "/tell/data\nx"},
		{"unrooted", "tell/data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, status, err := b.proxy(context.Background(), "homelab:captain", "POST", tc.path, []byte(`{}`))
			if err == nil {
				t.Fatalf("proxy accepted a framing path: status %d body %q", status, body)
			}
			if status != http.StatusBadRequest {
				t.Errorf("want 400 for a malformed path, got %d", status)
			}
		})
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected path still reached the ship: %+v", hits)
	}
}

func TestProxyRaw_RejectsFramingPath(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	_, status, err := b.proxyRaw(context.Background(), "homelab:captain", "POST",
		"/attach\r\nPOST /shutdown HTTP/1.1\r\n\r\n", "multipart/form-data; boundary=x", []byte("x"))
	if err == nil || status != http.StatusBadRequest {
		t.Fatalf("want a 400 error, got %d %v", status, err)
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected path still reached the ship: %+v", hits)
	}
}

// ---------------------------------------------------------------------------
// entry points: HTTP handlers
// ---------------------------------------------------------------------------

// rawPathRequest builds a request whose URL path holds bytes url.Parse would
// refuse (raw CR/LF). No real client can send those directly — but ServeMux
// hands the handler exactly these bytes after decoding a %0d%0a URI, so this
// is the handler's real input.
func rawPathRequest(method, path, body string) *http.Request {
	return &http.Request{
		Method: method,
		Proto:  "HTTP/1.1",
		URL:    &url.URL{Path: path},
		Host:   "fleet.invalid",
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func TestHandleTell_RejectsHostilePersona(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", b.handleTell)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, tc := range routableHostile {
		t.Run("decoded/"+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, rawPathRequest("POST",
				"/api/captain/homelab:captain/tell/"+tc.val, `{"message":"hi"}`))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400 (%s), got %d (%s)", tc.why, rec.Code, rec.Body.String())
			}
		})
	}
	for _, tc := range wireHostile {
		t.Run("wire/"+tc.name, func(t *testing.T) {
			code := postRaw(t, ts, "/api/captain/homelab:captain/tell/"+tc.seg, `{"message":"hi"}`)
			if code != http.StatusBadRequest {
				t.Fatalf("want 400 (%s), got %d", tc.why, code)
			}
		})
	}

	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected persona still reached the ship: %+v", hits)
	}
}

func TestHandleResolve_RejectsHostileID(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/resolve/{id}", b.handleResolve)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, tc := range routableHostile {
		t.Run("decoded/"+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, rawPathRequest("POST",
				"/api/captain/homelab:captain/resolve/"+tc.val, `{"behavior":"allow"}`))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400 (%s), got %d (%s)", tc.why, rec.Code, rec.Body.String())
			}
		})
	}
	for _, tc := range wireHostile {
		t.Run("wire/"+tc.name, func(t *testing.T) {
			code := postRaw(t, ts, "/api/captain/homelab:captain/resolve/"+tc.seg, `{"behavior":"allow"}`)
			if code != http.StatusBadRequest {
				t.Fatalf("want 400 (%s), got %d", tc.why, code)
			}
		})
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected id still reached the ship: %+v", hits)
	}
}

// The generic interpolating helpers gate their parameter too, so a route added
// tomorrow inherits the check instead of re-opening the hole.
func TestProxyHelpers_RejectHostileSegments(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/start", b.proxyPTYPost("/pty/%s/start"))
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/snapshot", b.proxyGet2("/pty/%s/snapshot", "persona", personaname.Valid))
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	mux.HandleFunc("GET /api/captain/{key}/bead/{id}", b.proxyGet2("/bead/%s", "id", beadIDOK))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/update", b.proxyPost2("/bead/%s/update", "id", beadIDOK))

	routes := []struct{ name, method, before, after string }{
		{"pty start", "POST", "/api/captain/homelab:captain/pty/", "/start"},
		{"pty snapshot", "GET", "/api/captain/homelab:captain/pty/", "/snapshot"},
		{"pty stream", "GET", "/api/captain/homelab:captain/pty/", "/stream"},
		{"bead show", "GET", "/api/captain/homelab:captain/bead/", ""},
		{"bead update", "POST", "/api/captain/homelab:captain/bead/", "/update"},
	}
	for _, rt := range routes {
		for _, tc := range routableHostile {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, rawPathRequest(rt.method, rt.before+tc.val+rt.after, `{}`))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("want 400 (%s), got %d (%s)", tc.why, rec.Code, rec.Body.String())
				}
			})
		}
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected segment still reached the ship: %+v", hits)
	}
}

func TestHandleBeadAssign_RejectsHostilePersonaInBody(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	for _, tc := range hostileIdentifiers {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"ship":"homelab:captain","persona":` + jsonQuote(tc.val) + `}`
			rec := httptest.NewRecorder()
			assignMux(b).ServeHTTP(rec, httptest.NewRequest("POST",
				"/api/captain/homelab:captain/bead/abc123/assign", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400 (%s), got %d (%s)", tc.why, rec.Code, rec.Body.String())
			}
		})
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected persona still reached the ship: %+v", hits)
	}
}

// ---------------------------------------------------------------------------
// H2: the same identifiers arriving as LLM tool-call arguments
// ---------------------------------------------------------------------------

// The voice loop's model context is fed by ship feeds and GitHub-derived text,
// so a tool-call argument is hostile input even though no operator typed it.
func TestConversationTools_RejectHostileIdentifiers(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")
	ctx := context.Background()

	for _, tc := range hostileIdentifiers {
		t.Run("tell_captain/"+tc.name, func(t *testing.T) {
			assertToolError(t, b.toolTellCaptain(ctx, map[string]any{
				"captain_key": "homelab:captain", "persona": tc.val, "message": "hi",
			}), tc.why)
		})
		t.Run("resolve/"+tc.name, func(t *testing.T) {
			assertToolError(t, b.toolResolve(ctx, map[string]any{
				"captain_key": "homelab:captain", "id": tc.val, "behavior": "allow",
			}), tc.why)
		})
		t.Run("dispatch_bead/"+tc.name, func(t *testing.T) {
			assertToolError(t, b.toolDispatchBead(ctx, map[string]any{
				"captain_key": "homelab:captain", "bead_id": "abc123", "persona": tc.val,
			}), tc.why)
		})
	}
	if hits := ship.allHits(); len(hits) != 0 {
		t.Fatalf("a rejected tool-call identifier still reached the ship: %+v", hits)
	}
}

// ---------------------------------------------------------------------------
// no regression: legal identifiers still proxy through unchanged
// ---------------------------------------------------------------------------

func TestValidSegmentsStillProxy(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", b.handleTell)
	mux.HandleFunc("POST /api/captain/{key}/resolve/{id}", b.handleResolve)
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/start", b.proxyPTYPost("/pty/%s/start"))
	mux.HandleFunc("GET /api/captain/{key}/bead/{id}", b.proxyGet2("/bead/%s", "id", beadIDOK))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cases := []struct{ name, method, url, wantPath string }{
		{"tell a hyphenated persona", "POST", "/api/captain/homelab:captain/tell/data-2", "/tell/data-2"},
		{"tell an underscored persona", "POST", "/api/captain/homelab:captain/tell/first_officer", "/tell/first_officer"},
		{"resolve a uuid-prefix id", "POST", "/api/captain/homelab:captain/resolve/3f2a91b0", "/resolve/3f2a91b0"},
		{"pty start keeps the query string", "POST", "/api/captain/homelab:captain/pty/data/start?client=w1", "/pty/data/start"},
		{"bead show with a dotted id", "GET", "/api/captain/homelab:captain/bead/proj-c03.1", "/bead/proj-c03.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.url, strings.NewReader(`{"message":"hi","behavior":"allow"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				t.Fatalf("valid identifier rejected: %d", resp.StatusCode)
			}
			if n := len(ship.hits(tc.method + " " + tc.wantPath)); n != 1 {
				t.Fatalf("want 1 hit on %q, got %d (all: %+v)", tc.wantPath, n, ship.allHits())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// postRaw sends path with its percent-encoding intact — url.Parse keeps the
// escaped form in RawPath, and that is what goes out on the wire — then
// returns the status code.
func postRaw(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request for %q: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("send %q: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func assertToolError(t *testing.T, out, why string) {
	t.Helper()
	got := decodeJSON[map[string]string](t, []byte(out))
	if got["error"] == "" {
		t.Fatalf("want a tool error (%s), got %s", why, out)
	}
}

// jsonQuote renders s as a JSON string literal, CR/LF included.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
