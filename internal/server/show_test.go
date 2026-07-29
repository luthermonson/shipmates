package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/codexapp"
)

// TestShowEndpointRevalidatesPathsAndNeverLeaks proves the server treats the
// client's file list as untrusted input: paths are revalidated against the
// server's own project root, rejections carry a stable code only, and a
// persona with no live session answers not_found so the CLI falls back to a
// one-shot turn.
func TestShowEndpointRevalidatesPathsAndNeverLeaks(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("valid.png", []byte("\x89PNG\r\n\x1a\npayload"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewWithCodexOptions(codexapp.StartOptions{})
	s.projectRoot, s.projectScope = root, "scope"
	s.controlToken = testControlToken

	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"no files", "application/json", `{"files":[]}`, http.StatusBadRequest},
		{"wrong content type", "text/plain", `{"files":["valid.png"]}`, http.StatusBadRequest},
		{"traversal", "application/json", `{"files":["../secret.txt"]}`, http.StatusBadRequest},
		{"remote url", "application/json", `{"files":["https://example.test/secret.png"]}`, http.StatusBadRequest},
		{"unknown field", "application/json", `{"files":["valid.png"],"attachment_id":"att_secret"}`, http.StatusBadRequest},
		{"no live session", "application/json", `{"files":["valid.png"],"caption":"hi"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/live/backend/show", strings.NewReader(tc.body))
			authenticate(r, "scope")
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.status, w.Body.String())
			}
			for _, leak := range []string{"secret", "example", root} {
				if strings.Contains(w.Body.String(), leak) {
					t.Fatalf("response leaked %q: %s", leak, w.Body.String())
				}
			}
		})
	}
}
