package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// tlsDummy stands in for a terminated-here TLS connection (r.TLS != nil is the
// only thing the cookie logic reads).
var tlsDummy = tls.ConnectionState{}

// safeBuffer is an io.Writer for slog: Run logs from goroutines, so the test's
// read of the captured output has to be synchronized with them.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog redirects the default slog logger for the duration of a test.
// Whether a message is a warning is part of the fix for M4, so the test has to
// read the level, not just the text.
func captureSlog(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// Regression tests for the MEDIUM/LOW findings in issue #42 that live on the
// fleet side: M4 (auth silently off), M5 (cookie scope), M8 (unbounded
// bodies), L4 (missing Content-Type) and L6 (dial port from a ship header).
// The M3 (XSS) tests live in ui_test.go and in sanitizeRepoURL below.

// ---------------------------------------------------------------------------
// M4 — a token-less fleet must not bind anything but loopback
// ---------------------------------------------------------------------------

func TestRequireTokenForAddr(t *testing.T) {
	cases := []struct {
		addr    string
		token   string
		wantErr bool
	}{
		// Token set: any address is the operator's business.
		{"0.0.0.0:8443", "s3cret", false},
		{":8443", "s3cret", false},
		{"127.0.0.1:8443", "s3cret", false},
		{"", "s3cret", false},
		// No token: loopback only.
		{"127.0.0.1:8443", "", false},
		{"127.7.7.7:8443", "", false},
		{"localhost:8443", "", false},
		{"LOCALHOST:8443", "", false},
		{"[::1]:8443", "", false},
		{"127.0.0.1", "", false},
		// No token, reachable from the network: refuse.
		{"0.0.0.0:8443", "", true},
		{":8443", "", true},
		{"", "", true},
		{"[::]:8443", "", true},
		{"192.168.1.10:8443", "", true},
		{"fleet.example.com:8443", "", true},
		// A whitespace-only token is not a token.
		{"0.0.0.0:8443", "   ", true},
	}
	for _, tc := range cases {
		err := requireTokenForAddr(tc.addr, tc.token)
		if (err != nil) != tc.wantErr {
			t.Errorf("requireTokenForAddr(%q, %q) err=%v, wantErr=%v", tc.addr, tc.token, err, tc.wantErr)
		}
	}
}

// The refusal must be a startup failure, not a log line: `--addr 0.0.0.0:8443`
// with no $SHIPMATES_FLEET_TOKEN used to publish tells, PTY input and
// permission resolves to the network with nothing but `auth=false` at INFO.
func TestRun_RefusesNonLoopbackBindWithoutToken(t *testing.T) {
	b := newTestFleet(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run must return the refusal immediately. It is deliberately NOT called
	// inline: without the guard Run reaches ListenAndServe and blocks forever,
	// and a test that hangs pins a CI runner instead of reporting a failure.
	errc := make(chan error, 1)
	go func() { errc <- b.Run(ctx, "0.0.0.0:0") }()

	var err error
	select {
	case err = <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("Run bound a non-loopback address with no token instead of refusing")
	}
	if err == nil {
		t.Fatal("a token-less fleet must refuse to bind a non-loopback address")
	}
	for _, want := range []string{"0.0.0.0:0", "shared secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q, got: %v", want, err)
		}
	}
}

func TestNew_RefusesNonLoopbackBindWithoutToken(t *testing.T) {
	t.Setenv(trustProxyEnv, "")
	if _, err := New(Options{Addr: "0.0.0.0:8443", PolicyPath: t.TempDir() + "/none.yaml"}); err == nil {
		t.Fatal("New must refuse a public bind with no token")
	}
	// The same address WITH a token is fine, and loopback without one is fine.
	for _, opts := range []Options{
		{Addr: "0.0.0.0:8443", Token: "s3cret"},
		{Addr: "127.0.0.1:8443"},
	} {
		opts.PolicyPath = t.TempDir() + "/none.yaml"
		s, err := New(opts)
		if err != nil {
			t.Fatalf("New(%+v): %v", opts, err)
		}
		_ = s.Close()
	}
}

// Loopback with no token still starts — it is the documented dev mode — but it
// must say so at WARN, not bury it in an INFO field.
func TestRun_LoopbackWithoutTokenWarnsAndServes(t *testing.T) {
	b := newTestFleet(t, "")
	logs := captureSlog(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, "127.0.0.1:0") }()
	// Give Run enough time to reach ListenAndServe, then shut it down.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loopback dev mode must still serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	got := logs.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("token-less start must log at WARN, got:\n%s", got)
	}
	if !strings.Contains(got, "WITHOUT a shared secret") {
		t.Errorf("the warning must say what is wrong, got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// M5 — cookie Secure attribute and derived session id
// ---------------------------------------------------------------------------

func loginCookie(t *testing.T, b *Server, mutate func(*http.Request)) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"token": {"s3cret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	b.handleLogin(rec, req)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want one cookie, got %d (status %d)", len(cookies), rec.Code)
	}
	return cookies[0]
}

func TestSessionCookie_SecureHonoursTrustedProxyOnly(t *testing.T) {
	xfp := func(v string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", v) }
	}

	t.Run("untrusted proxy header is ignored", func(t *testing.T) {
		b := newAuthFleet("s3cret")
		if loginCookie(t, b, xfp("https")).Secure {
			t.Error("a client-supplied X-Forwarded-Proto must not set Secure when proxy trust is off")
		}
	})

	t.Run("trusted proxy header sets Secure", func(t *testing.T) {
		b := newAuthFleet("s3cret")
		b.trustProxy = true
		c := loginCookie(t, b, xfp("https"))
		if !c.Secure {
			t.Error("behind a trusted TLS-terminating proxy the session cookie must be Secure")
		}
		// The other hardening survives the change.
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Errorf("HttpOnly/SameSite regressed: %+v", c)
		}
	})

	t.Run("proxy chain uses the leftmost value", func(t *testing.T) {
		b := newAuthFleet("s3cret")
		b.trustProxy = true
		if !loginCookie(t, b, xfp("https, http")).Secure {
			t.Error("the browser-facing hop is the first entry, not the last")
		}
		if loginCookie(t, b, xfp("http, https")).Secure {
			t.Error("a client that appends https must not be able to flip the attribute")
		}
	})

	t.Run("trusted proxy on plain http stays non-Secure", func(t *testing.T) {
		b := newAuthFleet("s3cret")
		b.trustProxy = true
		if loginCookie(t, b, xfp("http")).Secure {
			t.Error("a plaintext hop must not claim Secure — the browser would stop sending the cookie")
		}
	})

	t.Run("direct TLS still sets Secure", func(t *testing.T) {
		b := newAuthFleet("s3cret")
		c := loginCookie(t, b, func(r *http.Request) { r.TLS = &tlsDummy })
		if !c.Secure {
			t.Error("direct TLS must still mark the cookie Secure")
		}
	})
}

func TestSessionID_IsDerivedNotTheSecret(t *testing.T) {
	sid := sessionID("s3cret")
	if sid == "" || sid == "s3cret" {
		t.Fatalf("session id must be derived, got %q", sid)
	}
	if !strings.Contains(sid, "") || len(sid) < 32 {
		t.Errorf("session id looks too short to be an HMAC: %q", sid)
	}
	if strings.Contains(sid, "s3cret") {
		t.Errorf("session id embeds the secret: %q", sid)
	}
	// Deterministic, so a fleet restart doesn't sign every operator out.
	if sessionID("s3cret") != sid {
		t.Error("session id must be stable across calls")
	}
	if sessionID("other") == sid {
		t.Error("different secrets must derive different session ids")
	}
	if sessionID("") != "" {
		t.Error("a token-less fleet has no session id to hand out")
	}
}

func TestEnvTrustProxy(t *testing.T) {
	for env, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false, "off": false, "  OFF ": false,
		"1": true, "true": true, "yes": true,
	} {
		t.Setenv(trustProxyEnv, env)
		if got := envTrustProxy(); got != want {
			t.Errorf("%s=%q -> %v, want %v", trustProxyEnv, env, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// M3 (server half) — the ship's repo URL is not stored raw
// ---------------------------------------------------------------------------

func TestSanitizeRepoURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/luthermonson/shipmates", "https://github.com/luthermonson/shipmates"},
		{"http://gitea.internal/luther/ship", "http://gitea.internal/luther/ship"},
		{"  https://github.com/x/y/  ", "https://github.com/x/y"},
		// The XSS seeds from the finding.
		{"javascript:alert(1)", ""},
		{`" onload=x`, ""},
		// Re-serialization percent-encodes the quote and the angle brackets, so
		// even a consumer that forgets to escape cannot be broken out of.
		{`https://ok.example/"><script>alert(1)</script>`, "https://ok.example/%22%3E%3Cscript%3Ealert%281%29%3C/script%3E"},
		{"data:text/html,<script>alert(1)</script>", ""},
		{"vbscript:msgbox(1)", ""},
		// Real git remotes that are not browsable URLs.
		{"git@github.com:luthermonson/shipmates.git", ""},
		{"ssh://git@github.com/x/y.git", ""},
		{"/relative/path", ""},
		{"", ""},
		{"https://", ""},
		{"https://host/\r\nX-Injected: 1", ""},
		{strings.Repeat("https://a.example/", 100), ""},
	}
	for _, tc := range cases {
		if got := sanitizeRepoURL(tc.in); got != tc.want {
			t.Errorf("sanitizeRepoURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The header is the actual source, so pin the behaviour end-to-end: a hostile
// repo URL never reaches the Captain record the UI renders.
func TestAuthorize_DropsUnusableRepoURL(t *testing.T) {
	b := newTestFleet(t, "")
	r := authorizeReq("homelab:captain", "")
	r.Header.Set("X-Shipmates-Repo-URL", `javascript:alert(document.cookie)`)
	if _, authed, err := b.authorize(r); err != nil || !authed {
		t.Fatalf("authed=%v err=%v", authed, err)
	}
	if got := b.captains["homelab:captain"].RepoURL; got != "" {
		t.Errorf("a javascript: repo URL must not be stored, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// L6 — the dial port comes off the ship's own header
// ---------------------------------------------------------------------------

func TestParseShipPort(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"8443", 8443},
		{" 8443 ", 8443},
		{"1", 1},
		{"65535", 65535},
		{"", 0},
		{"0", 0},
		{"-1", 0},
		{"65536", 0},
		{"99999999999999999999", 0},
		{"not-a-number", 0},
		// Sscanf's old behaviour: "8443junk" parsed as 8443 and " 22" as 22.
		{"8443junk", 0},
		{"0x1f90", 0},
		{"+8443", 0},
	}
	for _, tc := range cases {
		if got := parseShipPort(tc.in, "homelab:captain"); got != tc.want {
			t.Errorf("parseShipPort(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A ship that reports an implausible port is registered (its identity and feed
// are still useful) but is never dialed: proxy() reports it instead of
// building "127.0.0.1:0".
func TestProxy_RefusesToDialAnImplausiblePort(t *testing.T) {
	b := newTestFleet(t, "")
	r := authorizeReq("homelab:captain", "")
	r.Header.Set("X-Shipmates-Port", "0")
	if _, authed, err := b.authorize(r); err != nil || !authed {
		t.Fatalf("authed=%v err=%v", authed, err)
	}
	if got := b.captains["homelab:captain"].Port; got != 0 {
		t.Fatalf("want port 0, got %d", got)
	}
	_, status, err := b.proxy(context.Background(), "homelab:captain", "GET", "/events", nil)
	if err == nil {
		t.Fatal("dialing a captain with no usable port must fail")
	}
	if status != http.StatusBadGateway {
		t.Errorf("want 502, got %d", status)
	}
	if !strings.Contains(err.Error(), "no usable port") {
		t.Errorf("error should explain itself, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// L4 — every proxied response carries an explicit Content-Type
// ---------------------------------------------------------------------------

func TestWriteProxied_SetsExplicitContentType(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"json object", `{"ok":true}`, "application/json"},
		{"json array", `[1,2,3]`, "application/json"},
		{"leading whitespace json", "\n  [1]", "application/json"},
		{"plain feed text", "12:00 picard/result: done", "text/plain; charset=utf-8"},
		{"empty", "", "text/plain; charset=utf-8"},
		// The finding: a crafted bead/feed body must never be sniffed as HTML
		// on the fleet origin, which is the origin holding the session cookie.
		{"html-looking body", `<html><script>alert(document.cookie)</script>`, "text/plain; charset=utf-8"},
		{"doctype body", "<!DOCTYPE html><body onload=alert(1)>", "text/plain; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeProxied(rec, http.StatusOK, []byte(tc.body), nil)
			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
			if strings.Contains(rec.Header().Get("Content-Type"), "html") {
				t.Errorf("a proxied body must never be served as HTML")
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestWriteProxied_ErrorPathAlsoTyped(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProxied(rec, http.StatusBadGateway, nil, context.DeadlineExceeded)
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("error Content-Type = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// ---------------------------------------------------------------------------
// M8 (fleet half) — request bodies are bounded
// ---------------------------------------------------------------------------

// oversizedBody is comfortably past every limit in the package.
func oversizedBody(n int64) *strings.Reader {
	return strings.NewReader(strings.Repeat("A", int(n)))
}

func TestHandlers_RejectOversizedBodies(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	cases := []struct {
		name    string
		method  string
		target  string
		vars    map[string]string
		handler func(*Server) http.HandlerFunc
		size    int64
	}{
		{
			name: "tell", method: "POST", target: "/api/captain/homelab:captain/tell/captain",
			vars:    map[string]string{"key": "homelab:captain", "persona": "captain"},
			handler: func(s *Server) http.HandlerFunc { return s.handleTell },
			size:    tellBodyLimit + 1024,
		},
		{
			name: "resolve", method: "POST", target: "/api/captain/homelab:captain/resolve/abc123",
			vars:    map[string]string{"key": "homelab:captain", "id": "abc123"},
			handler: func(s *Server) http.HandlerFunc { return s.handleResolve },
			size:    resolveBodyLimit + 1024,
		},
		{
			name: "bead assign", method: "POST", target: "/api/captain/homelab:captain/bead/abc123/assign",
			vars:    map[string]string{"key": "homelab:captain", "id": "abc123"},
			handler: func(s *Server) http.HandlerFunc { return s.handleBeadAssign },
			size:    beadBodyLimit + 1024,
		},
		{
			name: "beads nudge", method: "POST", target: "/api/beads/nudge",
			handler: func(s *Server) http.HandlerFunc { return s.handleBeadsNudge },
			size:    nudgeBodyLimit + 1024,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, oversizedBody(tc.size))
			for k, v := range tc.vars {
				req.SetPathValue(k, v)
			}
			rec := httptest.NewRecorder()
			tc.handler(b)(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("want 413 for a %d-byte body, got %d (%s)", tc.size, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "exceeds") {
				t.Errorf("the 413 should say what happened: %q", rec.Body.String())
			}
		})
	}

	// The oversized requests must never have reached the ship.
	for _, h := range ship.allHits() {
		t.Errorf("an oversized body was proxied to the ship: %s %s", h.method, h.path)
	}
}

// A normal-sized tell still goes through — the limit must not be so eager it
// breaks the product.
func TestHandleTell_NormalBodyStillProxied(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	req := httptest.NewRequest("POST", "/api/captain/homelab:captain/tell/captain",
		strings.NewReader(`{"message":"all hands"}`))
	req.SetPathValue("key", "homelab:captain")
	req.SetPathValue("persona", "captain")
	rec := httptest.NewRecorder()
	b.handleTell(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	hits := ship.hits("POST /tell/captain")
	if len(hits) != 1 || string(hits[0].body) != `{"message":"all hands"}` {
		t.Fatalf("tell did not reach the ship intact: %+v", hits)
	}
}

func TestReadLimitedBody(t *testing.T) {
	t.Run("under the limit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
		got, ok := readLimitedBody(rec, req, 64)
		if !ok || string(got) != "hello" {
			t.Fatalf("ok=%v body=%q", ok, got)
		}
	})
	t.Run("over the limit rejects rather than truncating", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", oversizedBody(200))
		got, ok := readLimitedBody(rec, req, 64)
		if ok {
			t.Fatalf("oversized body accepted as %q", got)
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("want 413, got %d", rec.Code)
		}
		if len(got) != 0 {
			t.Errorf("a rejected body must not be handed back truncated: %d bytes", len(got))
		}
	})
}
