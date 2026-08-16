package fleet

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

// cookieName is the fleet's session cookie. It carries a value DERIVED from
// the shared secret, never the secret itself: the secret is also the bearer
// token for the whole /api/* surface and for the ship-side tunnel connect, so
// a cookie that leaks (an XSS on this origin, a proxy access log, a browser
// profile copied off a laptop) used to hand over the fleet's master key. The
// derived id authenticates a browser session and nothing else — it is not
// accepted as a Bearer token.
//
// Renamed from "shipmates_bridge" during the fleet rename — existing browser
// sessions are invalidated on upgrade and users must log in once.
const cookieName = "shipmates_fleet"

// sessionKeyContext domain-separates the session derivation so the same secret
// used elsewhere (bearer auth, tunnel connect) can never collide with it.
const sessionKeyContext = "shipmates-fleet-session-v1"

// sessionID derives the browser session value from the shared secret. It is
// deterministic on purpose: the fleet keeps no server-side session table, so a
// restart must not sign every operator out mid-watch. HMAC (not a bare hash)
// means the cookie cannot be reversed into the secret, and the derivation is
// one-way even to someone who reads the source.
func sessionID(token string) string {
	if token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(sessionKeyContext))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// publicPaths are reachable without auth. /connect runs its own bearer check
// inside remotedialer's Authorizer. /login serves the login page (GET) and
// processes the form submit (POST). /health is for liveness probes.
// publicPaths and publicPrefixes are reachable without auth. The login page
// needs its CSS/JS to render, so the unauth'd static assets live under a known
// allow-list. (index.html itself is NOT in the list — root navigation while
// signed out redirects to /login.)
var publicPaths = map[string]bool{
	"/health":     true,
	"/login":      true,
	"/logout":     true,
	"/style.css":  true,
	"/app.js":     true,
	// like app.js: a .js URL must NEVER answer with login HTML or an edge
	// cache will store it and serve it to authenticated browsers as script
	"/conversation.js": true,
	"/favicon.ico":     true,
}

// publicPrefixes are path prefixes reachable without auth — vendored static
// libraries (xterm.js et al). UI code is not secret (it ships in the public
// repo); what matters is that asset URLs NEVER answer with the login page —
// intermediary caches (Cloudflare caches by extension) will happily store an
// HTML body under a .js URL and serve it to authenticated browsers, which
// bricks the UI in ways that look like anything but what they are.
var publicPrefixes = []string{"/vendor/", "/connect"}

// authGate wraps the given mux so /api/* and the UI require either a valid
// Bearer header OR a session cookie carrying the derived session id.
// Unauthenticated browsers are redirected to /login; unauthenticated API
// callers get 401 JSON.
//
// When the fleet was started with no token, auth is disabled and all requests
// pass through. That is a LOOPBACK-ONLY development mode: Run refuses to bind
// a non-loopback address without a token (see requireTokenForAddr), so this
// branch can never expose the fleet to the network.
func (b *Server) authGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.token == "" {
			next.ServeHTTP(w, r)
			return
		}
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range publicPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if b.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			httpError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// authenticated reports whether the request carries the shared secret in the
// Authorization: Bearer header, or the derived session id in the session
// cookie. Compared with crypto/subtle to avoid leaking either value via
// timing.
//
// The two credentials are deliberately NOT interchangeable: the cookie value
// is useless as a bearer token, and the bearer token is not accepted in the
// cookie. A browser that already holds a pre-derivation cookie simply fails
// the check and is bounced to /login.
func (b *Server) authenticated(r *http.Request) bool {
	want := []byte(b.token)
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != "" {
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if sid := sessionID(b.token); sid != "" &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(sid)) == 1 {
			return true
		}
	}
	return false
}

// trustProxyWarnOnce keeps the "you are behind a proxy but haven't said so"
// hint to a single line per process — it fires on every request otherwise.
var trustProxyWarnOnce sync.Once

// requestIsHTTPS decides whether the operator's browser reached us over TLS,
// which is what gates the Secure attribute on the session cookie.
//
// Trust model: X-Forwarded-Proto is client-supplied unless something in front
// of us overwrites it, so we honour it ONLY when the operator has declared
// that a trusted terminator sits in front (Options.TrustProxy, or the
// SHIPMATES_FLEET_TRUST_PROXY env var). Blindly trusting it would let any
// client flip a security attribute on its own cookie; refusing to ever read it
// leaves the cookie carrying a session credential without Secure behind every
// TLS-terminating reverse proxy, which is the far more common deployment.
// Declaring the trust is a one-line operator decision and it is the only
// place either failure mode can be avoided.
func (b *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	fwd := forwardedProto(r)
	if !b.trustProxy {
		if fwd == "https" {
			trustProxyWarnOnce.Do(func() {
				slog.Warn("fleet: X-Forwarded-Proto: https seen but proxy trust is off — " +
					"session cookies will NOT be marked Secure; set SHIPMATES_FLEET_TRUST_PROXY=1 " +
					"(only when a trusted TLS terminator sits in front of the fleet)")
			})
		}
		return false
	}
	return fwd == "https"
}

// forwardedProto reads the FIRST value of X-Forwarded-Proto. A proxy chain
// appends, so the leftmost entry is the one closest to the browser — and
// taking the last would let a client append its own.
func forwardedProto(r *http.Request) string {
	raw := r.Header.Get("X-Forwarded-Proto")
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	return strings.ToLower(strings.TrimSpace(first))
}

// handleLogin GETs the login page, or POSTs the form-submitted token. On a
// matching token we set the session cookie (carrying the DERIVED id, not the
// token) and redirect to /. On mismatch we re-render the login page with an
// error so a browser POST shows the failure inline instead of dumping a plain
// 401 text page.
func (b *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// A login form is a handful of bytes; anything larger is not a login.
		r.Body = http.MaxBytesReader(w, r.Body, loginFormLimit)
		token := r.FormValue("token")
		if b.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(b.token)) != 1 {
			b.serveLoginPage(w, r, "invalid token")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    sessionID(b.token),
			Path:     "/",
			HttpOnly: true,
			Secure:   b.requestIsHTTPS(r),
			SameSite: http.SameSiteStrictMode,
			MaxAge:   24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	b.serveLoginPage(w, r, "")
}

// loginFormLimit bounds the login POST body.
const loginFormLimit = 16 << 10

func (b *Server) serveLoginPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	page, err := uiFS.ReadFile("ui/login.html")
	if err != nil {
		httpError(w, "login page missing", http.StatusInternalServerError)
		return
	}
	body := string(page)
	if errMsg != "" {
		body = strings.Replace(body, "<!--ERR-->", `<div class="err">`+errMsg+`</div>`, 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_, _ = w.Write([]byte(body))
}

// handleLogout clears the session cookie and bounces the user back to /login.
func (b *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   b.requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// requireTokenForAddr is the fail-closed startup guard for M4: a fleet with no
// shared secret passes EVERY request through authGate — tells, PTY input,
// permission resolves, the whole ship surface — so it may only ever listen on
// loopback. Previously this was a single `auth=false` field on an INFO line at
// startup, which is not a decision an operator makes; `--addr 0.0.0.0:8443`
// with an unset $SHIPMATES_FLEET_TOKEN silently published the fleet.
//
// Returns a non-nil error when the combination must not be allowed to bind.
// Anything we cannot positively identify as loopback counts as public: a
// hostname we don't resolve, a wildcard bind, an empty address.
func requireTokenForAddr(addr, token string) error {
	if strings.TrimSpace(token) != "" {
		return nil
	}
	if addrIsLoopback(addr) {
		return nil
	}
	return &insecureBindError{addr: addr}
}

// insecureBindError explains the refusal in operator terms — the message is
// the entire user experience of this guard.
type insecureBindError struct{ addr string }

func (e *insecureBindError) Error() string {
	return "refusing to listen on " + e.addr + " without a shared secret: a token-less fleet " +
		"authenticates nobody, so it may only bind loopback. Set $SHIPMATES_FLEET_TOKEN (or " +
		"--token-file) before exposing the fleet, or bind 127.0.0.1 for local development."
}

// addrIsLoopback reports whether a listen address is definitely loopback-only.
// It is deliberately conservative — every "I'm not sure" answer is false, so
// the guard fails closed.
func addrIsLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false // net/http reads "" as ":http" — every interface
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// No port at all ("127.0.0.1"): treat the whole string as the host.
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // ":8443" — wildcard bind on every interface
	}
	// [::1] arrives with the brackets already stripped by SplitHostPort, but a
	// bare "[::1]" without a port does not.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	// Hostnames: only the well-known loopback names count. Resolving arbitrary
	// names at startup would make the guard depend on DNS, and a name that
	// resolves to a public address must not slip through on a lookup failure.
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain", "ip6-localhost":
		return true
	}
	return false
}
