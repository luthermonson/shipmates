package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/project"
)

// raw issues a request against the guarded handler WITHOUT the credential do()
// attaches, so the guard itself is what the assertion is about.
func raw(t *testing.T, h http.Handler, method, path, body string) *http.Request {
	t.Helper()
	if body == "" {
		return httptest.NewRequest(method, path, nil)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestUnauthenticatedStateChangeRejected is the finding in one test: before
// this, any page the operator visited could POST a prompt into a live agent
// on loopback. The status code matters, but the side effect matters more —
// an endpoint that 401s after already telling the mate would be no fix at all.
func TestUnauthenticatedStateChangeRejected(t *testing.T) {
	s, h := newTestServer(t)
	stdin := &nopWriteCloser{}
	s.mu.Lock()
	s.live["backend"] = &liveProc{persona: "backend", stdin: stdin}
	s.mu.Unlock()

	w := serve(h, raw(t, h, "POST", "/tell/backend", `{"message":"pwned"}`))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /tell = %d, want 401", w.Code)
	}
	if got := stdin.buf.String(); got != "" {
		t.Fatalf("unauthenticated tell reached the mate's stdin: %q", got)
	}
	s.mu.Lock()
	events := len(s.events)
	s.mu.Unlock()
	if events != 0 {
		t.Fatalf("unauthenticated tell recorded %d events, want 0", events)
	}
}

// TestUnauthenticatedShutdownRejected: /shutdown is the cheapest denial of
// service on the box — one cross-site POST and every mate dies.
func TestUnauthenticatedShutdownRejected(t *testing.T) {
	s, h := newTestServer(t)
	if w := serve(h, raw(t, h, "POST", "/shutdown", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated POST /shutdown = %d, want 401", w.Code)
	}
	select {
	case <-s.stopCh:
		t.Fatal("unauthenticated /shutdown stopped the server")
	default:
	}
}

// TestUnauthenticatedEndpointsAcrossTheSurface walks the state-changing and
// state-reading routes: the guard is meant to be blanket, not per-handler
// opt-in, so a route added later inherits it.
func TestUnauthenticatedEndpointsAcrossTheSurface(t *testing.T) {
	_, h := newTestServer(t)
	cases := []struct{ method, path, body string }{
		{"POST", "/tell/backend", `{"message":"x"}`},
		{"POST", "/pty/backend/start", ""},
		{"POST", "/pty/backend/input", "ls\n"},
		{"POST", "/pty/backend/takeover", ""},
		{"GET", "/pty/backend/snapshot", ""},
		{"POST", "/hook/backend/PreToolUse", `{"tool_name":"Bash"}`},
		{"POST", "/events", `{"persona":"backend","type":"note","text":"x"}`},
		{"GET", "/events", ""},
		{"GET", "/feed", ""},
		{"GET", "/status.json", ""},
		{"GET", "/pending.json", ""},
		{"POST", "/resolve/abc", `{"behavior":"allow"}`},
		{"POST", "/bead", `{"title":"x"}`},
		{"GET", "/bead/abc", ""},
		{"POST", "/register", ""},
		{"POST", "/shutdown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := serve(h, raw(t, h, tc.method, tc.path, tc.body))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, w.Code)
			}
			if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
			}
		})
	}
}

// TestHealthStaysOpen: the one exemption. client.EnsureRunning polls it while
// the server is still coming up — before the token file exists — so gating it
// would turn "not running yet" into a credential error.
func TestHealthStaysOpen(t *testing.T) {
	_, h := newTestServer(t)
	w := serve(h, raw(t, h, "GET", "/health", ""))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("unauthenticated GET /health = %d %q, want 200 \"ok\"", w.Code, w.Body.String())
	}
}

// TestCorrectTokenWorks is the other half: the guard must not be a wall.
func TestCorrectTokenWorks(t *testing.T) {
	s, h := newTestServer(t)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/status.json", "", http.StatusOK},
		{"GET", "/pending.json", "", http.StatusOK},
		{"GET", "/events", "", http.StatusOK},
		{"GET", "/feed", "", http.StatusOK},
		{"POST", "/events", `{"persona":"backend","type":"note","text":"hello"}`, http.StatusNoContent},
		{"POST", "/register", "", http.StatusNoContent},
		{"POST", "/deregister", "", http.StatusNoContent},
		// No PTY mate exists in the sandbox: 404 is the handler answering,
		// which is what this asserts — the guard let it through.
		{"GET", "/pty/backend/snapshot", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := raw(t, h, tc.method, tc.path, tc.body)
			req.Header.Set("Authorization", "Bearer "+s.token)
			if w := serve(h, req); w.Code != tc.want {
				t.Fatalf("%s %s with a valid token = %d, want %d", tc.method, tc.path, w.Code, tc.want)
			}
		})
	}
}

// TestBadTokensRejected covers the near-miss shapes, including a truncated
// prefix of the real token — the case a naive strings.HasPrefix or a
// short-circuiting compare would wave through.
func TestBadTokensRejected(t *testing.T) {
	s, h := newTestServer(t)
	cases := []struct{ name, header string }{
		{"absent", ""},
		{"empty bearer", "Bearer "},
		{"wrong token", "Bearer " + strings.Repeat("f", len(s.token))},
		{"truncated token", "Bearer " + s.token[:len(s.token)-1]},
		{"token with trailing junk", "Bearer " + s.token + "x"},
		{"bare token, no scheme", s.token},
		{"wrong scheme", "Basic " + s.token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := raw(t, h, "POST", "/shutdown", "")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if w := serve(h, req); w.Code != http.StatusUnauthorized {
				t.Fatalf("Authorization %q = %d, want 401", tc.header, w.Code)
			}
		})
	}
}

// TestCrossSiteRejectedEvenWithToken: defence in depth. A browser cannot read
// the token file, but if one ever did (or an operator pasted it into a
// console), the request still has to be refused — nothing in shipmates talks
// to the captain from a page.
func TestCrossSiteRejectedEvenWithToken(t *testing.T) {
	s, h := newTestServer(t)
	cases := []struct{ name, header, value string }{
		{"origin", "Origin", "https://evil.example"},
		{"origin null", "Origin", "null"},
		{"origin loopback page", "Origin", "http://127.0.0.1:9999"},
		{"sec-fetch-site cross-site", "Sec-Fetch-Site", "cross-site"},
		{"sec-fetch-site same-site", "Sec-Fetch-Site", "same-site"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := raw(t, h, "POST", "/tell/backend", `{"message":"x"}`)
			req.Header.Set("Authorization", "Bearer "+s.token)
			req.Header.Set(tc.header, tc.value)
			if w := serve(h, req); w.Code != http.StatusForbidden {
				t.Fatalf("%s: %s = %d, want 403", tc.header, tc.value, w.Code)
			}
		})
	}
}

// TestJSONEndpointNeedsJSONContentType: text/plain and form-encoded bodies are
// exactly the shapes a page can POST cross-origin without a preflight.
func TestJSONEndpointNeedsJSONContentType(t *testing.T) {
	s, h := newTestServer(t)
	for _, ct := range []string{"", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		t.Run(ct, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/tell/backend", strings.NewReader(`{"message":"x"}`))
			req.Header.Set("Authorization", "Bearer "+s.token)
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			if w := serve(h, req); w.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q = %d, want 415", ct, w.Code)
			}
		})
	}

	// The parameterised form of the right type still passes.
	req := httptest.NewRequest("POST", "/events", strings.NewReader(`{"persona":"backend","type":"note","text":"x"}`))
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if w := serve(h, req); w.Code != http.StatusNoContent {
		t.Fatalf("application/json; charset=utf-8 = %d, want 204", w.Code)
	}
}

// TestNonJSONBodyEndpointsExempt: /pty/{persona}/input carries raw keystrokes
// and /attach carries multipart. Requiring JSON there would break them; they
// are covered by the token and the cross-site checks instead. 404/400 here is
// the handler answering — the guard did not reject the media type.
func TestNonJSONBodyEndpointsExempt(t *testing.T) {
	s, h := newTestServer(t)
	req := httptest.NewRequest("POST", "/pty/backend/input", strings.NewReader("ls\n"))
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	if w := serve(h, req); w.Code == http.StatusUnsupportedMediaType {
		t.Fatal("/pty/{persona}/input must accept a non-JSON body")
	}
}

// TestHookTokenInQueryAccepted pins the compatibility fallback: hookSettings
// puts the token in both the Authorization header and the URL, because a
// Claude Code build that ignores the header field would otherwise leave the
// permission gate erroring on every tool call. The fallback is scoped to
// /hook/ and nowhere else.
func TestHookTokenInQueryAccepted(t *testing.T) {
	s, h := newTestServer(t)

	req := httptest.NewRequest("POST", "/hook/backend/PostToolUse?token="+s.token,
		strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`))
	req.Header.Set("Content-Type", "application/json")
	if w := serve(h, req); w.Code == http.StatusUnauthorized {
		t.Fatal("hook with ?token= was rejected; the gate would stop reporting")
	}

	// Same trick anywhere else is not a credential.
	other := raw(t, h, "POST", "/shutdown?token="+s.token, "")
	if w := serve(h, other); w.Code != http.StatusUnauthorized {
		t.Fatalf("?token= on /shutdown = %d, want 401", w.Code)
	}
}

// TestGuardFailsClosedWithoutAToken: a server that could not mint a
// credential must serve nothing, not everything.
func TestGuardFailsClosedWithoutAToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = ""
	h := s.guard(s.routes())
	if w := serve(h, raw(t, h, "POST", "/shutdown", "")); w.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless server answered %d, want 401", w.Code)
	}
	// Even an empty presented credential must not match the empty token.
	req := raw(t, h, "POST", "/shutdown", "")
	req.Header.Set("Authorization", "Bearer ")
	if w := serve(h, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("empty bearer against tokenless server = %d, want 401", w.Code)
	}
}

// TestNewMintsDistinctTokens: the credential is per run, not per install, and
// it comes from crypto/rand — two servers must never share one.
func TestNewMintsDistinctTokens(t *testing.T) {
	a, _ := newTestServer(t)
	b, _ := newTestServer(t)
	if a.token == "" || len(a.token) < 32 {
		t.Fatalf("token = %q, want a long random string", a.token)
	}
	if a.token == b.token {
		t.Fatal("two servers minted the same token")
	}
}

// TestSessionFilesArePrivate covers the file modes: the port file used to be
// 0644, readable by every account on the machine, while being treated as the
// capability. The token file must never be.
func TestSessionFilesArePrivate(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.writeSessionFiles(54321); err != nil {
		t.Fatalf("writeSessionFiles: %v", err)
	}
	got, err := project.ReadAPIToken()
	if err != nil {
		t.Fatalf("read back token: %v", err)
	}
	if got != s.token {
		t.Fatalf("token file holds %q, want %q", got, s.token)
	}
	for _, p := range []string{project.TokenFile(), project.PortFile(), project.PidFile()} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if runtime.GOOS == "windows" {
			// Go synthesizes 0666 for any writable file on Windows; the mode
			// argument buys nothing there and access is decided by the
			// inherited DACL. Asserting 0600 would be asserting a fiction —
			// the same position internal/recovery takes for its journal. All
			// that is checkable here is that the file exists and is not a
			// directory.
			if info.IsDir() {
				t.Fatalf("%s is a directory", p)
			}
			continue
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", p, perm)
		}
	}
}

// TestWriteSessionFilesRewritesLoosePermissions: os.WriteFile does not
// re-apply the mode to an existing file, so a 0644 port file left behind by a
// crashed pre-fix server would otherwise survive the upgrade.
func TestWriteSessionFilesRewritesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows; see TestSessionFilesArePrivate")
	}
	s, _ := newTestServer(t)
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.PortFile(), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeSessionFiles(54321); err != nil {
		t.Fatalf("writeSessionFiles: %v", err)
	}
	info, err := os.Stat(project.PortFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("stale 0644 port file kept mode %04o, want 0600", perm)
	}
}

// TestRunRefusesToStartWithoutAWritableTokenFile is the fail-closed rule: no
// credential on disk means no legitimate client can authenticate, so the
// server must refuse rather than start (and must certainly not decide to
// serve unauthenticated instead).
func TestRunRefusesToStartWithoutAWritableTokenFile(t *testing.T) {
	s, _ := newTestServer(t)
	// A non-empty DIRECTORY where the token file belongs: neither the remove
	// nor the write can succeed, on any platform.
	if err := os.MkdirAll(project.TokenFile(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.TokenFile(), "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Run started with no way to publish its token")
	}
	if !strings.Contains(err.Error(), "api token") {
		t.Fatalf("Run error = %v, want it to name the token", err)
	}
	if _, statErr := os.Stat(project.PortFile()); statErr == nil {
		t.Error("Run published a port file for a server that refused to start")
	}
}

// TestRunRefusesToStartWithNoToken: the other fail-closed path, where
// crypto/rand itself failed at construction.
func TestRunRefusesToStartWithNoToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.token = ""
	err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "API token") {
		t.Fatalf("Run with no token = %v, want a refusal naming the token", err)
	}
}

// TestClientRoundTripsAgainstTheRealServer wires the actual CLI client to the
// actual guarded route table. Both halves of this change have to agree about
// the header, the media type and the token file's location, and unit tests on
// either side alone would not catch them drifting apart.
func TestClientRoundTripsAgainstTheRealServer(t *testing.T) {
	s, h := newTestServer(t)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.WritePrivateFile(project.PortFile(), []byte(port)); err != nil {
		t.Fatal(err)
	}
	if err := project.WriteAPIToken(s.token); err != nil {
		t.Fatal(err)
	}

	if !client.Healthy() {
		t.Fatal("client.Healthy() = false against a live server")
	}
	if _, err := client.Post("/events", map[string]string{
		"persona": "backend", "type": "note", "text": "round trip",
	}); err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	body, err := client.Get("/feed")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	if !strings.Contains(string(body), "round trip") {
		t.Fatalf("feed = %q, want the event the client just posted", body)
	}

	// And the same round trip with the wrong credential is refused.
	if err := project.WriteAPIToken(strings.Repeat("0", len(s.token))); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Post("/events", map[string]string{"persona": "backend", "type": "note"}); err == nil {
		t.Fatal("client.Post with a stale token succeeded")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("client.Post with a stale token = %v, want a 401", err)
	}
}
