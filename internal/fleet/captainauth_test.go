package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSanitizeAPIToken: the proxy writes request lines by hand, so a token
// carrying CR/LF would let a hostile "captain" inject headers — or a whole
// second request — into the tunneled stream. Tokens we mint are hex, so
// anything else is dropped rather than escaped.
func TestSanitizeAPIToken(t *testing.T) {
	good := strings.Repeat("a1b2", 8)
	cases := []struct {
		name, in, want string
	}{
		{"hex token passes", good, good},
		{"empty", "", ""},
		{"too short", "abc123", ""},
		{"crlf injection", good + "\r\nX-Evil: 1", ""},
		{"lf injection", good + "\nX-Evil: 1", ""},
		{"space", good[:16] + " " + good[16:], ""},
		{"colon", good + ":x", ""},
		{"non-ascii", good + "é", ""},
		{"absurdly long", strings.Repeat("a", 257), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeAPIToken(tc.in); got != tc.want {
				t.Fatalf("sanitizeAPIToken(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAuthHeaderLine(t *testing.T) {
	if got := authHeaderLine(""); got != "" {
		t.Fatalf("no token should render no header line, got %q", got)
	}
	if got := authHeaderLine("abc"); got != "Authorization: Bearer abc\r\n" {
		t.Fatalf("authHeaderLine = %q", got)
	}
}

// TestProxyReplaysCaptainToken: the captain's local API now requires a bearer
// token, and the fleet usually runs on another host where it cannot read the
// captain's token file. The credential arrives on the tunnel connect and has
// to ride on every proxied request, or the whole fleet UI 401s.
func TestProxyReplaysCaptainToken(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/beads", b.proxyGet("/beads.json"))
	mux.HandleFunc("POST /api/captain/{key}/bead", b.proxyPost("/bead"))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, u := range []string{"/api/captain/homelab:captain/beads"} {
		resp, err := ts.Client().Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	resp, err := ts.Client().Post(ts.URL+"/api/captain/homelab:captain/bead", "application/json", strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	hits := ship.allHits()
	if len(hits) == 0 {
		t.Fatal("ship saw no proxied requests")
	}
	want := "Bearer " + fakeCaptainAPIToken
	for _, h := range hits {
		if h.auth != want {
			t.Errorf("%s %s arrived with Authorization %q, want %q", h.method, h.rawPath, h.auth, want)
		}
	}
}

// TestCaptainTokenNeverRendered: the credential must not leak back out of the
// fleet's own API. Captain rows are rendered to every fleet viewer.
func TestCaptainTokenNeverRendered(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	if got := b.captainAPIToken("homelab:captain"); got != fakeCaptainAPIToken {
		t.Fatalf("fleet recorded token %q, want %q", got, fakeCaptainAPIToken)
	}

	w := httptest.NewRecorder()
	b.handleCaptains(w, httptest.NewRequest("GET", "/api/captains", nil))
	body := w.Body.String()
	if strings.Contains(body, fakeCaptainAPIToken) {
		t.Fatalf("/api/captains rendered the captain's API token: %s", body)
	}
	// Sanity: it really did render the captain, so the check above isn't
	// passing on an empty list.
	var out []map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("captains json: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 captain rendered, got %d", len(out))
	}
}

// TestHostileCaptainTokenDropped: a token that could break the hand-rolled
// request framing is discarded at the door, not carried into proxy().
func TestHostileCaptainTokenDropped(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	headers := shipHeaders("homelab:captain", ship.port, "")
	headers.Set("X-Shipmates-API-Token", "0123456789abcdef\r\nX-Evil: 1")
	connectShipWithHeaders(t, b, "homelab:captain", headers)

	if got := b.captainAPIToken("homelab:captain"); got != "" {
		t.Fatalf("fleet kept a CRLF-bearing token: %q", got)
	}
}
