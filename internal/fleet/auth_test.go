package fleet

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// okHandler records that the wrapped handler was reached at all — the whole
// point of authGate is deciding whether the request gets this far.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("through"))
	})
}

func newAuthFleet(token string) *Server {
	return &Server{token: token, captains: map[string]*Captain{}}
}

func TestAuthGate_NoTokenDisablesAuth(t *testing.T) {
	b := newAuthFleet("")
	var reached bool
	rec := httptest.NewRecorder()
	b.authGate(okHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/captains", nil))
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("token-less fleet must pass everything through: reached=%v code=%d", reached, rec.Code)
	}
}

func TestAuthGate_PublicSurface(t *testing.T) {
	b := newAuthFleet("s3cret")
	// Assets a signed-out browser must be able to fetch. Anything with a .js
	// extension especially: an edge cache that stores login HTML under a .js
	// URL bricks the UI for authenticated users.
	public := []string{
		"/health", "/login", "/logout", "/style.css", "/app.js",
		"/conversation.js", "/favicon.ico",
		"/vendor/xterm.min.js", "/vendor/addon-fit.min.js",
		"/connect",
	}
	for _, p := range public {
		var reached bool
		rec := httptest.NewRecorder()
		b.authGate(okHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if !reached {
			t.Errorf("%s must be reachable without auth (got %d)", p, rec.Code)
		}
	}

	// Everything else is gated, including the app shell and the other HTML
	// pages — root navigation while signed out belongs on /login.
	gated := []string{"/", "/index.html", "/conversation.html", "/api/captains", "/api/status"}
	for _, p := range gated {
		var reached bool
		rec := httptest.NewRecorder()
		b.authGate(okHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if reached {
			t.Errorf("%s must NOT be reachable without auth", p)
		}
	}
}

func TestAuthGate_APIGets401NotLoginHTML(t *testing.T) {
	b := newAuthFleet("s3cret")
	var reached bool
	rec := httptest.NewRecorder()
	b.authGate(okHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/pending", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for unauthenticated /api/*, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Errorf("API callers must not receive HTML: %q", rec.Body.String())
	}
}

func TestAuthGate_BrowserRedirectsToLogin(t *testing.T) {
	b := newAuthFleet("s3cret")
	var reached bool
	rec := httptest.NewRecorder()
	b.authGate(okHandler(&reached)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("want redirect to /login, got %q", loc)
	}
}

func TestAuthGate_AcceptsBearerAndCookie(t *testing.T) {
	b := newAuthFleet("s3cret")

	t.Run("bearer", func(t *testing.T) {
		var reached bool
		req := httptest.NewRequest("GET", "/api/captains", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		b.authGate(okHandler(&reached)).ServeHTTP(rec, req)
		if !reached {
			t.Fatalf("valid bearer rejected: %d", rec.Code)
		}
	})

	t.Run("cookie", func(t *testing.T) {
		var reached bool
		req := httptest.NewRequest("GET", "/api/captains", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionID("s3cret")})
		rec := httptest.NewRecorder()
		b.authGate(okHandler(&reached)).ServeHTTP(rec, req)
		if !reached {
			t.Fatalf("valid cookie rejected: %d", rec.Code)
		}
	})
}

func TestAuthenticated_RejectsNearMisses(t *testing.T) {
	b := newAuthFleet("s3cret")
	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"no credentials", func(*http.Request) {}},
		{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }},
		{"prefix of secret", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cre") }},
		{"secret plus suffix", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3crets") }},
		{"wrong cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: cookieName, Value: "nope"}) }},
		{"right value wrong cookie name", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "shipmates_bridge", Value: sessionID("s3cret")})
		}},
		// M5: the two credentials are not interchangeable in EITHER direction.
		// A pre-derivation cookie (the raw secret) is no longer a session, and
		// a session id is not a bearer token.
		{"raw secret in the cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: "s3cret"})
		}},
		{"session id as a bearer token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+sessionID("s3cret"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/captains", nil)
			tc.mutate(req)
			if b.authenticated(req) {
				t.Fatalf("authenticated() accepted %s", tc.name)
			}
		})
	}
}

// The "Bearer " scheme is optional: authenticated() TrimPrefix's it, so a bare
// secret in the Authorization header also passes. That matches authorize()'s
// handling of the captain's connect token, and CLI callers rely on it. Pinned
// here so nobody "fixes" one side without the other.
func TestAuthenticated_BearerSchemeIsOptional(t *testing.T) {
	b := newAuthFleet("s3cret")
	for _, hdr := range []string{"Bearer s3cret", "s3cret"} {
		req := httptest.NewRequest("GET", "/api/captains", nil)
		req.Header.Set("Authorization", hdr)
		if !b.authenticated(req) {
			t.Errorf("authenticated() rejected Authorization: %q", hdr)
		}
	}
}

func TestHandleLogin_GETServesPage(t *testing.T) {
	b := newAuthFleet("s3cret")
	rec := httptest.NewRecorder()
	b.handleLogin(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("want html content type, got %q", ct)
	}
	if strings.Contains(rec.Body.String(), `class="err"`) {
		t.Errorf("a plain GET must not render an error banner")
	}
}

func TestHandleLogin_POSTWrongTokenRendersError(t *testing.T) {
	b := newAuthFleet("s3cret")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"token": {"wrong"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	b.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid token") {
		t.Errorf("error banner missing from login page")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("a failed login must not set a session cookie")
	}
}

// A fleet started without a token has no password to check; the form must not
// become a way in with an empty string.
func TestHandleLogin_POSTRejectedWhenFleetHasNoToken(t *testing.T) {
	b := newAuthFleet("")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"token": {""}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	b.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 when the fleet has no token, got %d", rec.Code)
	}
}

func TestHandleLogin_POSTSetsHardenedCookie(t *testing.T) {
	b := newAuthFleet("s3cret")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"token": {"s3cret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	b.handleLogin(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302 after login, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want exactly one cookie, got %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != cookieName {
		t.Errorf("cookie name wrong: %s", c.Name)
	}
	// M5: the cookie carries a DERIVED session id. The shared secret is also
	// the bearer token for every /api/* call and for the ship-side tunnel
	// connect, so a cookie that leaks must not be the master key.
	if c.Value == "s3cret" {
		t.Errorf("session cookie carries the raw shared secret")
	}
	if c.Value != sessionID("s3cret") {
		t.Errorf("cookie value %q is not the derived session id", c.Value)
	}
	if c.Value == "" {
		t.Errorf("session cookie is empty")
	}
	if !c.HttpOnly {
		t.Errorf("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie must be SameSite=Strict, got %v", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("cookie path must be /, got %q", c.Path)
	}
	if c.MaxAge <= 0 {
		t.Errorf("cookie must have a positive MaxAge, got %d", c.MaxAge)
	}
}

func TestHandleLogout_ExpiresCookie(t *testing.T) {
	b := newAuthFleet("s3cret")
	rec := httptest.NewRecorder()
	b.handleLogout(rec, httptest.NewRequest("POST", "/logout", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName {
		t.Fatalf("logout must clear %s, got %+v", cookieName, cookies)
	}
	if cookies[0].MaxAge >= 0 || cookies[0].Value != "" {
		t.Errorf("logout cookie must be emptied and expired: %+v", cookies[0])
	}
	// The cleared cookie must no longer authenticate.
	req := httptest.NewRequest("GET", "/api/captains", nil)
	req.AddCookie(cookies[0])
	if b.authenticated(req) {
		t.Errorf("cleared cookie still authenticates")
	}
}

// End-to-end through the gate: log in, follow the redirect with the cookie the
// server handed back, and land on a gated endpoint.
func TestAuthGate_LoginRoundTrip(t *testing.T) {
	b := newAuthFleet("s3cret")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", b.handleLogin)
	mux.HandleFunc("POST /login", b.handleLogin)
	mux.HandleFunc("GET /api/captains", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	ts := httptest.NewServer(b.authGate(mux))
	defer ts.Close()

	jar := &recordingJar{}
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.PostForm(ts.URL+"/login", url.Values{"token": {"s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/captains", nil)
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("gated endpoint after login: %d %q", resp2.StatusCode, body)
	}
}

// recordingJar is a minimal cookie jar (net/http/cookiejar would work too, but
// this keeps the assertion surface small and host-rule-free).
type recordingJar struct{ cookies []*http.Cookie }

func (j *recordingJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.cookies = append(j.cookies, cs...)
}
func (j *recordingJar) Cookies(*url.URL) []*http.Cookie { return j.cookies }
