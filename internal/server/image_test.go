package server

import (
	"github.com/luthermonson/shipmates/internal/codexapp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageLiveRequestRejectsMalformedPathsAndContentTypeWithoutLeak(t *testing.T) {
	s := NewWithCodexOptions(codexapp.StartOptions{})
	s.projectScope = "scope"
	s.controlToken = testControlToken
	for _, tc := range []struct{ content, body string }{{"text/plain", `{"prompt":"x","images":["secret.png"]}`}, {"application/json", `{"prompt":"x","images":["../secret.png"]}`}} {
		r := httptest.NewRequest(http.MethodPost, "/api/live/backend", strings.NewReader(tc.body))
		authenticate(r, "scope")
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
		}
	}
}

func TestImageRoutesRejectMultipartURLDataAndRemoteShapes(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile("valid.png", []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewWithCodexOptions(codexapp.StartOptions{})
	s.projectRoot, s.projectScope = root, "scope"
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"multipart", "multipart/form-data; boundary=x", `{"prompt":"x","images":["valid.png"]}`},
		{"url", "application/json", `{"prompt":"x","images":["https://example.test/x.png"]}`},
		{"data", "application/json", `{"prompt":"x","images":["data:image/png;base64,AAAA"]}`},
		{"remote-object", "application/json", `{"prompt":"x","images":[{"url":"https://example.test/x"}]}`},
		{"attachment-id", "application/json", `{"prompt":"x","attachment_id":"att_secret"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/live/backend", strings.NewReader(tc.body))
			r.SetPathValue("persona", "backend")
			r.Header.Set("X-Shipmates-Project", "scope")
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			s.handleCodexLive(w, r)
			if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "example") || strings.Contains(w.Body.String(), "att_secret") {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, ".shipmates", "inbox")); !os.IsNotExist(err) {
		t.Fatalf("image request created inbox: %v", err)
	}
}
