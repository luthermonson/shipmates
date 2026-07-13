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

func TestBearerIsScopedToLocalControlRequests(t *testing.T) {
	t.Chdir(t.TempDir())
	writeDiscoveryFixture(t, nil)
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	seen := map[string]http.Header{}
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen[r.URL.Path] = r.Header.Clone()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	if _, err := Do(context.Background(), http.MethodPost, "/api/live/backend", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Do(context.Background(), http.MethodPost, "/shutdown", nil); err != nil {
		t.Fatal(err)
	}
	if got := seen["/api/live/backend"].Get("Authorization"); got != "" {
		t.Fatalf("live request leaked bearer %q", got)
	}
	if got := seen["/shutdown"].Get("Authorization"); got != "Bearer "+strings.Repeat("x", 43) {
		t.Fatalf("shutdown bearer = %q", got)
	}
	for path, header := range seen {
		if header.Get(projectHeader) == "" {
			t.Fatalf("%s missing project scope", path)
		}
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
