package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/remotedialer"
)

// This file wires a REAL remotedialer tunnel between a fleet Server and one or
// more fake ships. Most of the fleet's interesting code — proxy(), the
// fan-out aggregators, the conversation tool implementations — only runs when
// a captain is actually connected, so stubbing b.proxy would test nothing.
// The tunnel is cheap (two in-process websockets over loopback) and every
// helper here is bounded by an explicit timeout so a broken build fails fast
// instead of hanging the suite.

// tunnelWait bounds how long we'll wait for a websocket session to register.
const tunnelWait = 15 * time.Second

// newTestFleet builds a Server with a real dialer (so ListClients/HasSession
// work) and no store. Pass a token to exercise the connect-time bearer check.
func newTestFleet(t *testing.T, token string) *Server {
	t.Helper()
	b := &Server{
		token:    token,
		captains: map[string]*Captain{},
		policy:   newPolicyState(t.TempDir() + "/absent-policy.yaml"),
	}
	b.dialer = remotedialer.New(b.authorize, remotedialer.DefaultErrorWriter)
	return b
}

// shipHit is one request a fake ship received.
type shipHit struct {
	method  string
	path    string // path only
	rawPath string // path + "?" + query, as the fleet framed it
	body    []byte
	ctype   string
	auth    string // Authorization the fleet replayed (the captain's own API token)
}

// fakeShip stands in for a shipmates captain's local 127.0.0.1 server. Routes
// answer 200 with an empty JSON array unless bodies/status say otherwise.
type fakeShip struct {
	srv  *httptest.Server
	port int

	mu   sync.Mutex
	seen []shipHit

	// keyed by "METHOD /path" (no query)
	bodies map[string][]byte
	status map[string]int
}

func newFakeShip(t *testing.T) *fakeShip {
	t.Helper()
	s := &fakeShip{
		bodies: map[string][]byte{},
		status: map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		raw := r.URL.Path
		if r.URL.RawQuery != "" {
			raw += "?" + r.URL.RawQuery
		}
		s.mu.Lock()
		s.seen = append(s.seen, shipHit{
			method: r.Method, path: r.URL.Path, rawPath: raw,
			body: body, ctype: r.Header.Get("Content-Type"),
			auth: r.Header.Get("Authorization"),
		})
		key := r.Method + " " + r.URL.Path
		out, hasBody := s.bodies[key]
		code, hasCode := s.status[key]
		s.mu.Unlock()

		if !hasCode {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if hasBody {
			_, _ = w.Write(out)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(s.srv.Close)

	u, err := url.Parse(s.srv.URL)
	if err != nil {
		t.Fatalf("parse fake ship url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("fake ship port: %v", err)
	}
	s.port = p
	return s
}

// setJSON configures a route's response body from a Go value.
func (s *fakeShip) setJSON(route string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	s.mu.Lock()
	s.bodies[route] = raw
	s.mu.Unlock()
}

// hits returns every request matching "METHOD /path".
func (s *fakeShip) hits(route string) []shipHit {
	method, path, _ := strings.Cut(route, " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []shipHit
	for _, h := range s.seen {
		if h.method == method && h.path == path {
			out = append(out, h)
		}
	}
	return out
}

func (s *fakeShip) allHits() []shipHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shipHit(nil), s.seen...)
}

// connectShip dials a remotedialer tunnel from the fake ship into the fleet
// and blocks until the fleet reports a live session for clientKey. The
// connection is torn down at test end.
func connectShip(t *testing.T, b *Server, ship *fakeShip, clientKey string) {
	t.Helper()
	connectShipWithHeaders(t, b, clientKey, shipHeaders(clientKey, ship.port, b.token))
}

// fakeCaptainAPIToken is the credential a fake ship claims for its own local
// API — the fleet has to replay it on everything it proxies back through the
// tunnel, or the captain answers 401.
const fakeCaptainAPIToken = "0123456789abcdef0123456789abcdef"

func shipHeaders(clientKey string, port int, token string) http.Header {
	h := http.Header{}
	h.Set("X-Shipmates-Identity", clientKey)
	h.Set("X-Shipmates-API-Token", fakeCaptainAPIToken)
	h.Set("X-Shipmates-Repo", strings.SplitN(clientKey, ":", 2)[0])
	h.Set("X-Shipmates-Repo-URL", "https://example.invalid/"+strings.SplitN(clientKey, ":", 2)[0])
	h.Set("X-Shipmates-Install-ID", "install-"+clientKey)
	h.Set("X-Shipmates-Persona", "captain")
	h.Set("X-Shipmates-Port", strconv.Itoa(port))
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func connectShipWithHeaders(t *testing.T, b *Server, clientKey string, headers http.Header) {
	t.Helper()

	connectSrv := httptest.NewServer(b.dialer)
	t.Cleanup(connectSrv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The ship authorizes inbound dials back to its own loopback server.
		_ = remotedialer.ClientConnect(ctx, "ws"+strings.TrimPrefix(connectSrv.URL, "http")+"/connect",
			headers, nil,
			func(proto, address string) bool {
				return proto == "tcp" && strings.HasPrefix(address, "127.0.0.1:")
			}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(tunnelWait):
			t.Log("remotedialer client did not shut down within the timeout")
		}
	})

	deadline := time.Now().Add(tunnelWait)
	for !b.dialer.HasSession(clientKey) {
		if time.Now().After(deadline) {
			t.Fatalf("tunnel for %s never came up", clientKey)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// decodeJSON is a small assertion helper: fail the test rather than return an
// error nobody checks.
func decodeJSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %T from %q: %v", v, raw, err)
	}
	return v
}
