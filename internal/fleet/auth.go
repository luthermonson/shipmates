package fleet

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// cookieName is the fleet's session cookie. We store the raw shared secret in
// it (not a derived session id) because the fleet already trusts whoever holds
// the secret — the cookie is just a way for a browser to present it on every
// request, including EventSource (which can't set Authorization headers).
// Renamed from "shipmates_bridge" during the fleet rename — existing browser
// sessions are invalidated on upgrade and users must log in once.
const cookieName = "shipmates_fleet"

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
// Bearer header OR a session cookie carrying the shared secret. Unauthenticated
// browsers are redirected to /login; unauthenticated API callers get 401 JSON.
// When the fleet was started with no token, auth is disabled and all requests
// pass through (dev only — fleet logs auth=false at startup).
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
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})
}

// authenticated reports whether the request carries the shared secret in either
// the Authorization: Bearer header or the session cookie. Compared with
// crypto/subtle to avoid leaking the secret via timing.
func (b *Server) authenticated(r *http.Request) bool {
	want := []byte(b.token)
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != "" {
		if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
			return true
		}
	}
	return false
}

// handleLogin GETs the login page, or POSTs the form-submitted token. On a
// matching token we set the session cookie and redirect to /. On mismatch we
// re-render the login page with an error so a browser POST shows the failure
// inline instead of dumping a plain 401 text page.
func (b *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		token := r.FormValue("token")
		if b.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(b.token)) != 1 {
			b.serveLoginPage(w, r, "invalid token")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	b.serveLoginPage(w, r, "")
}

func (b *Server) serveLoginPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	page, err := uiFS.ReadFile("ui/login.html")
	if err != nil {
		http.Error(w, "login page missing", http.StatusInternalServerError)
		return
	}
	body := string(page)
	if errMsg != "" {
		body = strings.Replace(body, "<!--ERR-->", `<div class="err">`+errMsg+`</div>`, 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}
