package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unauthenticatedRoutes are the only routes allowed to answer without the
// control token. /health is the liveness probe callers use before they trust
// anything else; /register and /deregister are 204 stubs retained for legacy
// clients and touch no state.
var unauthenticatedRoutes = map[string]bool{
	"GET /health":      true,
	"POST /register":   true,
	"POST /deregister": true,
}

// authenticatedRoutes is the full authenticated surface. Adding a route to
// handler() without adding it here fails TestEveryRouteIsCovered, and adding
// it here without putting it behind localControlOnly fails the auth tests
// below. That pairing is the point: the /api/live surface once relied on the
// project scope alone — a SHA of the project path, documented non-secret and
// echoed by /health — which is not a boundary between local user accounts.
var authenticatedRoutes = []struct{ method, path string }{
	{http.MethodPost, "/shutdown"},
	{http.MethodPost, "/api/live/skipper"},
	{http.MethodPost, "/api/live/skipper/attach"},
	{http.MethodPost, "/api/live/skipper/release"},
	{http.MethodPost, "/api/live/skipper/heartbeat"},
	{http.MethodPost, "/api/live/skipper/sync"},
	{http.MethodPost, "/api/live/skipper/action"},
	{http.MethodPost, "/api/live/skipper/approval"},
	{http.MethodGet, "/api/live/skipper/feed"},
	{http.MethodPost, "/api/live/skipper/tell"},
	{http.MethodPost, "/api/live/skipper/show"},
	{http.MethodPost, "/api/live/skipper/interrupt"},
	{http.MethodGet, "/api/local/v1/steer-targets"},
	{http.MethodPost, "/api/local/v1/steer-exact"},
	{http.MethodPost, "/api/local/v1/interrupt-exact"},
}

// testControlToken is the control token every server test authenticates
// with. Handler tests exercise handler behavior, so they must get past the
// auth middleware first — see authenticate.
const testControlToken = "tttttttttttttttttttttttttttttttt"

// authenticate adds the credentials the live-session routes now require.
// Handler tests call this so a 401 never masquerades as the 400/404 they are
// actually asserting.
func authenticate(r *http.Request, scope string) {
	r.Header.Set("X-Shipmates-Project", scope)
	r.Header.Set("Authorization", "Bearer "+testControlToken)
}

func authTestServer() *Server {
	s := New()
	s.projectScope = "scope"
	s.controlToken = testControlToken
	return s
}

// A missing, malformed, or wrong bearer token must be refused before the
// handler runs — on every authenticated route, not just the ones someone
// remembered.
func TestAuthenticatedRoutesRejectBadTokens(t *testing.T) {
	for _, r := range authenticatedRoutes {
		for name, headers := range map[string]map[string]string{
			"no headers at all": {},
			"scope only":        {"X-Shipmates-Project": "scope"},
			"wrong token":       {"X-Shipmates-Project": "scope", "Authorization": "Bearer " + strings.Repeat("x", 32)},
			"empty bearer":      {"X-Shipmates-Project": "scope", "Authorization": "Bearer "},
			"not bearer":        {"X-Shipmates-Project": "scope", "Authorization": strings.Repeat("t", 32)},
			"right token wrong scope": {
				"X-Shipmates-Project": "other-scope",
				"Authorization":       "Bearer " + strings.Repeat("t", 32),
			},
		} {
			s := authTestServer()
			req := httptest.NewRequest(r.method, r.path, nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized && w.Code != http.StatusConflict {
				t.Errorf("%s %s with %s: status=%d, want 401 or 409", r.method, r.path, name, w.Code)
			}
		}
	}
}

// The scope is the only thing the old, weaker gate checked, so /health must
// not hand it to a caller that has not already proved it knows it.
func TestHealthDoesNotDiscloseProjectScope(t *testing.T) {
	s := authTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409", w.Code)
	}
	if got := w.Header().Get("X-Shipmates-Project"); got != "" {
		t.Fatalf("unauthenticated /health disclosed scope %q", got)
	}
	if body := w.Body.String(); strings.Contains(body, "scope") {
		t.Fatalf("unauthenticated /health body disclosed scope: %q", body)
	}
}

func TestHealthEchoesScopeWhenItMatches(t *testing.T) {
	s := authTestServer()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Shipmates-Project", "scope")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("X-Shipmates-Project") != "scope" {
		t.Fatalf("status=%d scope=%q, want 200 and the scope echoed", w.Code, w.Header().Get("X-Shipmates-Project"))
	}
}

// TestEveryRouteIsCovered asserts the two tables above describe the whole
// mux, so a newly registered route cannot silently escape the auth tests.
func TestEveryRouteIsCovered(t *testing.T) {
	covered := map[string]bool{}
	for k := range unauthenticatedRoutes {
		covered[k] = true
	}
	for _, r := range authenticatedRoutes {
		// Collapse the {persona} wildcard back to its pattern form.
		covered[r.method+" "+strings.Replace(r.path, "/skipper", "/{persona}", 1)] = true
	}
	for _, pattern := range authTestServer().registeredRoutePatterns() {
		if !covered[pattern] {
			t.Errorf("route %q is registered but not covered by route_auth_test.go; add it to authenticatedRoutes (and put it behind localControlOnly) or to unauthenticatedRoutes with a reason", pattern)
		}
	}
}
