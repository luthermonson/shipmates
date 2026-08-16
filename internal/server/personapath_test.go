package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// M1 — persona path traversal on the captain's own handlers.
//
// The captain takes {persona} from a URL wildcard. ServeMux percent-decodes
// PathValue before the handler sees it, and cleanPath — which would have
// collapsed a literal "/tell/../.." into "/tell" and redirected — operates on
// the ESCAPED path, so "%2e%2e" and "..%2f" sail straight through matching and
// arrive at the handler already decoded. From there the name reached
// project.AgentPath and project.MemoryDir (reads of arbitrary *.md / *.yaml),
// project.SessionMarker (a file WRITE, with JSON content the caller
// influences), the per-persona policy lookup, and berth.Dir → `git worktree
// add`. internal/fleet validated this at its proxy edge; the captain, which is
// the process that actually opens the file, did not.

// hostilePersonaSegments are request-target spellings, not decoded values: the
// point of the test is that the escaping survives mux matching. Each entry
// pairs the segment as it appears on the wire with what PathValue decodes it
// to.
var hostilePersonaSegments = []struct{ segment, decodes, why string }{
	{"%2e%2e", "..", "bare parent hop, encoded past cleanPath"},
	{"..%2fescape", "../escape", "hop plus separator"},
	{"..%2f..%2f..%2foutside%2fpwned", "../../../outside/pwned", "hop out of the checkout entirely"},
	{"nested%2fname", "nested/name", "path separator"},
	{"nested%5Cname", "nested\\name", "windows path separator"},
	{"-rf", "-rf", "leading dash reads as a flag"},
	{"--dangerously-skip-permissions", "--dangerously-skip-permissions", "leading dash reads as a flag"},
	{"%2Fabsolute", "/absolute", "absolute path"},
	{"Captain", "Captain", "uppercase is not the rule"},
	{"with%20space", "with space", "whitespace"},
	{"with%0anewline", "with\nnewline", "request-line framing"},
}

// personaRoutes is every route on the captain that takes a persona. Keeping it
// as a list — rather than testing the two the issue happened to name — is the
// point: a new persona-taking route that forgets the guard fails here.
var personaRoutes = []struct {
	name, method, format, body string
}{
	{"tell", "POST", "/tell/%s", `{"message":"hi"}`},
	{"hook", "POST", "/hook/%s/PostToolUse", `{"tool_name":"Read"}`},
	{"pty start", "POST", "/pty/%s/start", ""},
	{"pty stream", "GET", "/pty/%s/stream", ""},
	{"pty snapshot", "GET", "/pty/%s/snapshot", ""},
	{"pty input", "POST", "/pty/%s/input", "keys"},
	{"pty resize", "POST", "/pty/%s/resize", `{"cols":80,"rows":24}`},
	{"pty takeover", "POST", "/pty/%s/takeover?client=c1", ""},
	{"pty release", "POST", "/pty/%s/release?client=c1", ""},
}

// sandboxWithNeighbour chdirs into a sandbox that has a sibling directory an
// escaping path can actually land in, and returns a func that fails the test
// if anything anywhere under the shared root changed. Without the sibling, a
// traversal would fail on a missing parent for the wrong reason.
func sandboxWithNeighbour(t *testing.T) func() {
	t.Helper()
	root := t.TempDir()
	sandbox := filepath.Join(root, "ship")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{sandbox, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A persona file the traversal would otherwise resolve to and parse.
	if err := os.WriteFile(filepath.Join(outside, "pwned.md"), []byte("---\nmodel: leaked\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sandbox)

	before := treeOf(t, root)
	return func() {
		t.Helper()
		after := treeOf(t, root)
		if strings.Join(before, "\n") != strings.Join(after, "\n") {
			t.Fatalf("filesystem changed outside the sandbox\nbefore:\n%s\nafter:\n%s",
				strings.Join(before, "\n"), strings.Join(after, "\n"))
		}
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestPersonaHandlersRejectTraversal sweeps every persona-taking route with
// every hostile spelling and demands a 400 — and, separately, that nothing was
// created or removed anywhere, inside the checkout or out of it.
func TestPersonaHandlersRejectTraversal(t *testing.T) {
	// No `claude` on PATH: a route that gets past the guard must fail on the
	// spawn rather than starting a real agent in a temp directory. Keeps this
	// test safe to run against the pre-fix code, which is the only way to know
	// it fails before.
	t.Setenv("PATH", t.TempDir())

	for _, route := range personaRoutes {
		for _, hostile := range hostilePersonaSegments {
			t.Run(route.name+"/"+hostile.segment, func(t *testing.T) {
				checkContained := sandboxWithNeighbour(t)
				_, h := newTestServer(t)
				target := strings.Replace(route.format, "%s", hostile.segment, 1)
				w := do(t, h, route.method, target, route.body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s %s = %d (%s), want 400 — %s decodes to %q (%s)",
						route.method, target, w.Code, strings.TrimSpace(w.Body.String()),
						hostile.segment, hostile.decodes, hostile.why)
				}
				checkContained()
			})
		}
	}
}

// TestPathValueStillDecodesTheEscaping documents WHY the guard is needed at
// the handler and not left to the mux: the mux hands the handler the decoded
// traversal. If a future Go release starts rejecting these at the router, this
// test tells us the ground moved; the guard stays either way.
func TestPathValueStillDecodesTheEscaping(t *testing.T) {
	mux := http.NewServeMux()
	got := make(map[string]string)
	mux.HandleFunc("POST /tell/{persona}", func(w http.ResponseWriter, r *http.Request) {
		got["persona"] = r.PathValue("persona")
	})
	for _, hostile := range hostilePersonaSegments {
		delete(got, "persona")
		req := httptest.NewRequest("POST", "/tell/"+hostile.segment, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code == http.StatusMovedPermanently {
			continue // the mux cleaned it; nothing reached a handler
		}
		if got["persona"] != hostile.decodes {
			t.Errorf("PathValue for %q = %q, expected %q", hostile.segment, got["persona"], hostile.decodes)
		}
	}
}

// TestPersonaHandlersAcceptLegalNames is the other half: the guard must not
// break the routes it now stands in front of. A legal name reaches the handler
// and gets the handler's own answer (404 "no pty mate"), not a 400.
func TestPersonaHandlersAcceptLegalNames(t *testing.T) {
	_, h := newTestServer(t)
	for _, name := range []string{"captain", "first-mate", "backend_2", "a"} {
		for _, route := range []string{"/pty/%s/snapshot", "/pty/%s/input", "/pty/%s/release?client=c1"} {
			target := strings.Replace(route, "%s", name, 1)
			method, body := "GET", ""
			if !strings.HasSuffix(route, "snapshot") {
				method, body = "POST", "keys"
			}
			w := do(t, h, method, target, body)
			if w.Code == http.StatusBadRequest {
				t.Errorf("%s = 400 (%s), a legal persona must reach the handler",
					target, strings.TrimSpace(w.Body.String()))
			}
		}
	}
}

// TestEventPersonaFromBodyIsValidated: a persona also arrives in a JSON body,
// not only in a path wildcard. It keys s.lastSeen and shows up in
// /status.json, so an anonymous poster does not get to invent one — while
// ship-level events with no persona at all stay legal.
func TestEventPersonaFromBodyIsValidated(t *testing.T) {
	s, h := newTestServer(t)
	for _, hostile := range hostilePersonaSegments {
		body := `{"persona":` + quoteJSON(hostile.decodes) + `,"type":"note","text":"x"}`
		w := do(t, h, "POST", "/events", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /events persona=%q = %d, want 400 (%s)", hostile.decodes, w.Code, hostile.why)
		}
	}
	if w := do(t, h, "POST", "/events", `{"type":"attach:received","text":"x"}`); w.Code != http.StatusNoContent {
		t.Fatalf("ship-level event with no persona = %d, want 204", w.Code)
	}
	if w := do(t, h, "POST", "/events", `{"persona":"backend","type":"note","text":"x"}`); w.Code != http.StatusNoContent {
		t.Fatalf("legal persona = %d, want 204", w.Code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Persona != "" && e.Persona != "backend" {
			t.Fatalf("an invented persona reached the feed: %q", e.Persona)
		}
	}
}

// quoteJSON is enough JSON string quoting for the control characters these
// test values carry.
func quoteJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// TestHookEventSegmentIsBounded: {event} is not a path segment on our side but
// it is concatenated into an event Type the fleet mirrors and renders.
func TestHookEventSegmentIsBounded(t *testing.T) {
	_, h := newTestServer(t)
	for _, event := range []string{"with%20space", "with%0anewline", "%2e%2e", "a%2fb", strings.Repeat("a", 65)} {
		w := do(t, h, "POST", "/hook/backend/"+event, `{"tool_name":"Read"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("POST /hook/backend/%s = %d, want 400", event, w.Code)
		}
	}
	if w := do(t, h, "POST", "/hook/backend/PostToolUse", `{"tool_name":"Read"}`); w.Code != http.StatusOK {
		t.Fatalf("a real hook event = %d, want 200", w.Code)
	}
}
