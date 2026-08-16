package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/permissions"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestToWebsocketURL(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{name: "https gets wss", in: "https://fleet.example.com", want: "wss://fleet.example.com/connect"},
		{name: "bare root path is replaced", in: "https://fleet.example.com/", want: "wss://fleet.example.com/connect"},
		{name: "explicit path is preserved", in: "https://fleet.example.com/tunnel", want: "wss://fleet.example.com/tunnel"},
		{name: "ws passes through for loopback", in: "ws://localhost:9000/connect", want: "ws://localhost:9000/connect"},
		{name: "wss passes through", in: "wss://fleet.example.com/connect", want: "wss://fleet.example.com/connect"},
		{name: "loopback http gets ws and the default path", in: "http://127.0.0.1:8443", want: "ws://127.0.0.1:8443/connect"},
		{name: "surrounding whitespace is trimmed", in: "  https://fleet.example.com  ", want: "wss://fleet.example.com/connect"},
		// L3: the connect headers carry the fleet token AND this ship's own
		// API token. Plaintext to anything but loopback is refused, never
		// silently downgraded.
		{name: "plaintext http to a remote host is refused", in: "http://fleet.example.com", wantErr: true},
		{name: "plaintext http to a LAN address is refused", in: "http://10.0.0.5:8080", wantErr: true},
		{name: "plaintext ws to a remote host is refused", in: "ws://fleet.example.com/connect", wantErr: true},
		{name: "credentials in the url are refused", in: "https://user:pw@fleet.example.com", wantErr: true},
		// A scheme-less host parses as a bare path, not a host — silently
		// dialing that would produce a confusing connection error much later.
		{name: "no scheme is rejected", in: "fleet.example.com", wantErr: true},
		{name: "unsupported scheme is rejected", in: "ftp://fleet.example.com", wantErr: true},
		{name: "empty is rejected", in: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toWebsocketURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("toWebsocketURL(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toWebsocketURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("toWebsocketURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFleetAuthorizer is a security boundary, not a formatting detail. The
// fleet dials BACK through the captain's own tunnel; this callback is the only
// thing stopping Fleet Command from using a connected ship as a proxy into
// whatever else that machine can reach — the LAN, cloud metadata endpoints,
// other local services. It must fail closed on anything unexpected.
func TestFleetAuthorizer(t *testing.T) {
	s := &Server{port: 51515}
	auth := s.fleetAuthorizer()

	cases := []struct {
		name, proto, address string
		want                 bool
	}{
		{"our own loopback port", "tcp", "127.0.0.1:51515", true},
		{"loopback by name", "tcp", "localhost:51515", true},

		{"a different local port", "tcp", "127.0.0.1:22", false},
		{"another service on the box", "tcp", "127.0.0.1:5432", false},
		{"a LAN host", "tcp", "192.168.1.10:51515", false},
		{"cloud metadata", "tcp", "169.254.169.254:80", false},
		{"an external host", "tcp", "evil.example.com:51515", false},
		{"a host that merely starts with 127", "tcp", "127.0.0.1.evil.com:51515", false},
		{"udp is not a tunnel protocol", "udp", "127.0.0.1:51515", false},
		{"unix sockets are refused", "unix", "/tmp/sock", false},
		{"unparseable address", "tcp", "127.0.0.1", false},
		{"empty address", "tcp", "", false},
		{"no port", "tcp", "localhost:", false},
		// IPv6 loopback is not in the allow-list. Documented as-is: the
		// captain always binds 127.0.0.1, so [::1] is never its own address.
		{"ipv6 loopback is not allow-listed", "tcp", "[::1]:51515", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth(tc.proto, tc.address); got != tc.want {
				t.Fatalf("authorize(%q, %q) = %v, want %v", tc.proto, tc.address, got, tc.want)
			}
		})
	}
}

func TestCaptainPersonaDefaultsAndOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := captainPersona(); got != "captain" {
		t.Fatalf("with no config = %q, want captain", got)
	}
	if err := os.WriteFile(project.ConfigName, []byte("captainPersona: skipper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := captainPersona(); got != "skipper" {
		t.Fatalf("with an override = %q, want skipper", got)
	}
	// Whitespace-only must fall back rather than produce an empty clientKey.
	if err := os.WriteFile(project.ConfigName, []byte("captainPersona: \"   \"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := captainPersona(); got != "captain" {
		t.Fatalf("with a blank override = %q, want the captain default", got)
	}
}

// TestStartFleetIsANoOpWithoutAConfiguredFleet: a ship-only deploy must not
// open a tunnel, must not spawn a policy poller, and must not record fleet
// identity that notifyBeadsNudge would then try to call.
func TestStartFleetNoOpWithoutFleetURL(t *testing.T) {
	t.Chdir(t.TempDir())
	s := New()

	for _, conf := range []*project.Config{nil, {}} {
		s.startFleet(context.Background(), conf)
		s.startFleetPolicy(context.Background(), conf)
		s.mu.Lock()
		url, token, key := s.fleetURL, s.fleetToken, s.fleetKey
		s.mu.Unlock()
		if url != "" || token != "" || key != "" {
			t.Fatalf("conf=%v recorded fleet identity %q/%q/%q", conf, url, token, key)
		}
	}
}

func TestStartFleetPolicyNoOpWithoutEvaluator(t *testing.T) {
	// A nil evaluator has nowhere to install a policy; starting the poller
	// anyway would nil-panic inside fetchFleetPolicy.
	t.Chdir(t.TempDir())
	s := &Server{stopCh: make(chan struct{}), perms: nil}
	conf := &project.Config{}
	conf.Fleet.URL = "https://fleet.example.com"
	s.startFleetPolicy(context.Background(), conf) // must not panic
}

func TestFetchFleetPolicyRejectsGarbage(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"not json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>hi</html>")) }},
		{"404", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusNotFound) }},
		{"401", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad token", http.StatusUnauthorized) }},
		{"502", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "upstream", http.StatusBadGateway) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/fleet-policy", tc.handler)
			fake := httptest.NewServer(mux)
			defer fake.Close()

			s := &Server{
				perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
				stopCh: make(chan struct{}),
			}
			if err := s.fetchFleetPolicy(context.Background(), fake.URL, ""); err == nil {
				t.Fatal("want an error so the caller keeps the last-known policy")
			}
		})
	}
}

func TestFetchFleetPolicySendsNoBearerWhenTokenless(t *testing.T) {
	// An empty token must mean no Authorization header at all — sending
	// "Bearer " reads as a malformed credential to most gateways.
	t.Chdir(t.TempDir()) // a successful fetch persists the policy cache
	var sawAuth string
	var sawAccept string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawAccept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"deny":[]}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	s := &Server{perms: permissions.NewEvaluatorWithRules(permissions.MergedRules{}), stopCh: make(chan struct{})}
	if err := s.fetchFleetPolicy(context.Background(), fake.URL, ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if sawAuth != "" {
		t.Fatalf("Authorization = %q, want none", sawAuth)
	}
	if sawAccept != "application/json" {
		t.Fatalf("Accept = %q", sawAccept)
	}
}

func TestFetchFleetPolicyHonorsContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	s := &Server{perms: permissions.NewEvaluatorWithRules(permissions.MergedRules{}), stopCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.fetchFleetPolicy(ctx, fake.URL, "") }()
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want an error after cancellation")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fetchFleetPolicy ignored context cancellation")
	}
}

func TestNotifyBeadsNudgeSkipsWhenNotFleeted(t *testing.T) {
	// No fleet URL means no callback target; the function must return
	// immediately instead of building a request against "".
	t.Chdir(t.TempDir())
	s := New()
	s.notifyBeadsNudge() // must not panic or hang
}

func TestNotifyBeadsNudgePostsIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	type got struct {
		auth, ctype, body string
	}
	seen := make(chan got, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/beads/nudge", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		seen <- got{r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b[:n])}
		w.WriteHeader(http.StatusOK)
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	s := New()
	s.mu.Lock()
	s.fleetURL = fake.URL + "/" // trailing slash must be trimmed, not doubled
	s.fleetToken = "tok"
	s.fleetKey = "myrepo:captain"
	s.mu.Unlock()

	s.notifyBeadsNudge()

	select {
	case g := <-seen:
		if g.auth != "Bearer tok" {
			t.Errorf("Authorization = %q", g.auth)
		}
		if g.ctype != "application/json" {
			t.Errorf("Content-Type = %q", g.ctype)
		}
		if !strings.Contains(g.body, `"from":"myrepo:captain"`) {
			t.Errorf("body = %q, want the ship's clientKey", g.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nudge never reached the fleet")
	}
}
