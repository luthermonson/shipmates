package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeDiscoveryFixture(t *testing.T, mutate func(map[string]any)) {
	t.Helper()
	root, err := project.CanonicalRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := project.ScopeID(root)
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]any{"schema_version": 1, "project_root": root, "project_scope": scope, "address": "127.0.0.1:43210", "pid": os.Getpid(), "control_token": strings.Repeat("x", 43)}
	if mutate != nil {
		mutate(v)
	}
	b, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(project.ServerRecordFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.ServerRecordFile(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryValidatesAtomicServerRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"valid", nil},
		{"schema", func(v map[string]any) { v["schema_version"] = 2 }},
		{"root", func(v map[string]any) { v["project_root"] = t.TempDir() }},
		{"scope", func(v map[string]any) { v["project_scope"] = "other" }},
		{"address", func(v map[string]any) { v["address"] = "192.0.2.1:1" }},
		{"pid", func(v map[string]any) { v["pid"] = -1 }},
		{"token", func(v map[string]any) { v["control_token"] = "short" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeDiscoveryFixture(t, tc.mutate)
			_, err := discover()
			if (tc.name == "valid") != (err == nil) {
				t.Fatalf("discover error = %v", err)
			}
		})
	}
}

// TestBearerAccompaniesEveryRequest replaces an earlier test that asserted
// the opposite — that the bearer was withheld from /api/live requests. That
// was the bug, not the invariant: the live routes start sessions, mint
// controller leases, steer and interrupt live turns, and resolve tool
// approvals, and they were gated on the project scope alone, which is a SHA
// of the project path that the server documents as non-secret. Loopback is
// not a boundary between local user accounts, so the token must ride on
// every request. The client no longer keeps a per-path allowlist, because an
// allowlist that must be extended for each new route is what produced the
// gap in the first place.
func TestBearerAccompaniesEveryRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	writeDiscoveryFixture(t, nil)
	old := localControlHTTPClient.Transport
	defer func() { localControlHTTPClient.Transport = old }()
	seen := map[string]http.Header{}
	localControlHTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Path] = r.Header.Clone()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	for _, path := range []string{
		"/api/live/backend",
		"/api/live/backend/attach",
		"/api/live/backend/approval",
		"/api/live/backend/interrupt",
		"/api/local/v1/steer-exact",
		"/shutdown",
	} {
		if _, err := Do(context.Background(), http.MethodPost, path, nil); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	wantBearer := "Bearer " + strings.Repeat("x", 43)
	for path, header := range seen {
		if got := header.Get("Authorization"); got != wantBearer {
			t.Errorf("%s Authorization = %q, want the control token", path, got)
		}
		if header.Get(projectHeader) == "" {
			t.Errorf("%s missing project scope", path)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("saw %d requests, want 6", len(seen))
	}
}

// The redirect-stripping client must be used for every request now that
// every request bears the token: http.DefaultClient would carry
// Authorization across a redirect.
func TestControlClientStripsAuthorizationOnRedirect(t *testing.T) {
	if controlHTTPClient() != localControlHTTPClient {
		t.Fatal("requests must use the redirect-stripping client")
	}
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/api/live/backend", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	if err := localControlHTTPClient.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization survived a redirect: %q", got)
	}
}

func TestDiscoveryRefusesSymlinkAndHardlinkState(t *testing.T) {
	t.Run("symlink ancestor", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, project.Dir)); err != nil {
			t.Fatal(err)
		}
		if _, err := discover(); err == nil {
			t.Fatal("symlinked state ancestor accepted")
		}
	})
	t.Run("hardlinked record", func(t *testing.T) {
		t.Chdir(t.TempDir())
		writeDiscoveryFixture(t, nil)
		if err := os.Link(project.ServerRecordFile(), project.ServerRecordFile()+".link"); err != nil {
			t.Fatal(err)
		}
		if _, err := discover(); err == nil {
			t.Fatal("hardlinked discovery record accepted")
		}
	})
}

func TestLocalControlRedirectPolicyStripsBearerAndRefuses(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/elsewhere", strings.NewReader("secret"))
	req.Header.Set("Authorization", "Bearer secret")
	err := localControlHTTPClient.CheckRedirect(req, nil)
	if err != http.ErrUseLastResponse || req.Header.Get("Authorization") != "" || req.Body != nil {
		t.Fatalf("redirect policy err=%v auth=%q body=%v", err, req.Header.Get("Authorization"), req.Body)
	}
}
