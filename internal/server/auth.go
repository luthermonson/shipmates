package server

import (
	"crypto/subtle"
	"mime"
	"net/http"
	"strings"
)

// Authentication for the captain's local API.
//
// The server binds 127.0.0.1 only, and that used to be the whole defence. It
// isn't one. Loopback is reachable by every process on the box, and — worse —
// by any web page the operator visits: a cross-origin
// `fetch('http://127.0.0.1:PORT/tell/captain', {method:'POST', body:'{"message":"…"}'})`
// is a CORS-"simple" request, so the browser sends it without a preflight.
// The attacker never reads the response and does not need to; the side effect
// (a prompt injected into a live agent, a mate spawned, the captain shut down)
// has already happened. The port is no secret either: it is written into the
// repo tree, and a page can simply scan loopback ports.
//
// So every route except the liveness probe requires a bearer token minted per
// server run (see project.TokenFile), and requests that look like they came
// from a browser are refused outright.

// guard wraps the route table with authentication and the cross-site checks.
// It is applied in Run rather than inside routes() so the route table stays
// separately testable, and so there is exactly one place where the API's
// admission rules live.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defence in depth, and cheap: nothing in shipmates talks to the
		// captain from a browser, so anything that announces a web origin is
		// a CSRF attempt by construction — refuse before looking at the
		// credential.
		if crossSite(r) {
			http.Error(w, "cross-site requests are not accepted", http.StatusForbidden)
			return
		}
		if !openEndpoint(r) {
			if !s.authenticated(r) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="shipmates"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// A JSON body must arrive labelled as JSON. text/plain and
			// form-encoded bodies are the shapes a page can send without a
			// preflight; requiring application/json means a browser has to
			// ask permission first, and gets refused.
			if wantsJSONBody(r) && !isJSONContentType(r) {
				http.Error(w, "want Content-Type: application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// openEndpoint is the one exemption: GET /health. It is a liveness probe that
// returns a constant "ok", changes nothing, and discloses nothing the port
// file doesn't already. Clients poll it before they have any reason to
// believe a captain exists — client.EnsureRunning waits on it while the
// server is still starting, i.e. potentially before the token file lands — so
// gating it would mean handing every caller a credential error in place of
// "not running yet".
func openEndpoint(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/health"
}

// crossSite reports whether the request was issued by a browser on behalf of
// some other site. An Origin header on a request to the captain has no
// legitimate source: the captain serves no web UI, and every real client
// (CLI, bridge, fleet proxy, crew hooks) is a Go HTTP client that sends none.
// Sec-Fetch-Site is the modern, unspoofable-by-script version of the same
// signal — "cross-site" is evil.com, and "same-site" covers a page served
// from another port on this host.
func crossSite(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "cross-site", "same-site":
		return true
	}
	return false
}

// authenticated compares the presented bearer token with this run's token in
// constant time. An empty server token fails every request: a server that
// could not mint a credential serves nothing rather than serving everything.
func (s *Server) authenticated(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	got := bearerToken(r)
	if got == "" && strings.HasPrefix(r.URL.Path, "/hook/") {
		// Crew hooks are configured by hookSettings, which sets an
		// Authorization header on the HTTP hook. Older Claude Code builds
		// ignore unknown hook fields, so the same token also rides in the
		// query string as a compatibility fallback — the alternative is a
		// silently ungated permission gate on an operator's older CLI. The
		// URL is only ever handed to the crew process we spawned, and the
		// token is unguessable either way.
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// bearerToken extracts the credential from an Authorization header, returning
// "" when the header is absent or not a bearer scheme.
func bearerToken(r *http.Request) string {
	const prefix = "bearer "
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// wantsJSONBody reports whether this request is one of the JSON-decoding
// endpoints carrying a body. Bodyless POSTs (start/takeover/release/shutdown/
// register) are exempt: there is nothing to mislabel. So are the two
// endpoints that legitimately carry something else — /attach is a multipart
// upload and /pty/{persona}/input is raw keystrokes.
func wantsJSONBody(r *http.Request) bool {
	if r.Method != http.MethodPost || r.ContentLength == 0 {
		return false
	}
	if r.URL.Path == "/attach" || strings.HasSuffix(r.URL.Path, "/input") {
		return false
	}
	return true
}

// isJSONContentType reports whether the request's media type is exactly
// application/json, ignoring parameters such as charset.
func isJSONContentType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mt, "application/json")
}
