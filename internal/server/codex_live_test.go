package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/livesession"
)

func TestLocalControlMiddlewareEchoesAuthenticatedProjectScope(t *testing.T) {
	s := &Server{projectScope: "scope", controlToken: "token"}
	req := httptest.NewRequest(http.MethodGet, "/api/local/v1/steer-targets", nil)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Shipmates-Project", "scope")
	w := httptest.NewRecorder()
	s.localControlOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent || w.Header().Get("X-Shipmates-Project") != "scope" {
		t.Fatalf("status=%d scope=%q", w.Code, w.Header().Get("X-Shipmates-Project"))
	}
}

func TestInterruptHTTPFakeAppServer(t *testing.T) {
	if os.Getenv("SHIPMATES_FAKE_INTERRUPT_SERVER") != "1" {
		return
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			os.Exit(2)
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]string{"userAgent": "codex-cli 0.144.1"}
		case "initialized":
			continue
		case "thread/start":
			result = map[string]any{"thread": map[string]string{"id": "thread"}}
		case "turn/start":
			result = map[string]any{"turn": map[string]string{"id": "turn"}}
		case "turn/interrupt":
			result = map[string]bool{"accepted": true}
		default:
			os.Exit(3)
		}
		b, _ := json.Marshal(map[string]any{"id": json.RawMessage(req.ID), "result": result})
		fmt.Println(string(b))
	}
}

func interruptTestServer(t *testing.T) (*Server, livesession.Snapshot) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".shipmates", "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".codex", "agents", "backend.toml"), []byte("developer_instructions = \"backend\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policyFixture := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	for _, path := range []string{
		filepath.Join(".shipmates", "policy.yaml"),
		filepath.Join(".shipmates", "policies", "backend.yaml"),
	} {
		if err := os.WriteFile(path, policyFixture, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := New()
	s.liveSessions = livesession.New(nil, codexapp.StartOptions{
		Command:         []string{os.Args[0], "-test.run=^TestInterruptHTTPFakeAppServer$"},
		Environment:     map[string]string{"SHIPMATES_FAKE_INTERRUPT_SERVER": "1"},
		StartupTimeout:  time.Second,
		ShutdownTimeout: 100 * time.Millisecond,
	})
	sess, err := s.liveSessions.StartLive(context.Background(), livesession.StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
	return s, sess.Snapshot()
}

func TestServerLiveStartRemainsBoundToCapturedProjectRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, ".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".shipmates", "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex", "agents", "backend.toml"), []byte("developer_instructions = \"backend\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policyFixture := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	for _, path := range []string{filepath.Join(root, ".shipmates", "policy.yaml"), filepath.Join(root, ".shipmates", "policies", "backend.yaml")} {
		if err := os.WriteFile(path, policyFixture, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewWithCodexOptions(codexapp.StartOptions{
		Command: []string{os.Args[0], "-test.run=^TestInterruptHTTPFakeAppServer$"}, Environment: map[string]string{"SHIPMATES_FAKE_INTERRUPT_SERVER": "1"},
		StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond,
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/live/backend", bytes.NewBufferString(`{"prompt":"work"}`))
	r.SetPathValue("persona", "backend")
	w := httptest.NewRecorder()
	s.handleCodexLive(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%q", w.Code, w.Body.String())
	}
	var snap livesession.Snapshot
	if json.Unmarshal(w.Body.Bytes(), &snap) != nil || snap.State != livesession.Working || snap.TurnID == "" {
		t.Fatalf("start snapshot=%+v body=%q", snap, w.Body.String())
	}
	t.Cleanup(func() { s.liveSessions.ShutdownAll(context.Background()) })
}

func TestInterruptHTTPRejectsOmittedAndEmptyTurnID(t *testing.T) {
	s := New()
	for _, body := range []string{
		`{"session_id":"session","thread_id":"thread"}`,
		`{"session_id":"session","thread_id":"thread","turn_id":""}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/live/backend/interrupt", bytes.NewBufferString(body))
		r.SetPathValue("persona", "backend")
		w := httptest.NewRecorder()
		s.handleCodexInterrupt(w, r)
		if w.Code != http.StatusBadRequest || w.Body.String() != "{\"code\":\"invalid_target\",\"message\":\"live session request failed\"}\n" {
			t.Fatalf("body=%s status=%d response=%q", body, w.Code, w.Body.String())
		}
	}
}

func TestInterruptHTTPNonEmptyStaleAndMismatchedTargets(t *testing.T) {
	s, snap := interruptTestServer(t)
	for _, body := range []string{
		`{"session_id":"stale","thread_id":"thread","turn_id":"turn"}`,
		`{"session_id":"` + snap.SessionID + `","thread_id":"thread","turn_id":"other-turn"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/live/backend/interrupt", bytes.NewBufferString(body)).WithContext(context.Background())
		r.SetPathValue("persona", "backend")
		w := httptest.NewRecorder()
		s.handleCodexInterrupt(w, r)
		if w.Code != http.StatusConflict || w.Body.String() != "{\"code\":\"stale_target\",\"message\":\"live session request failed\"}\n" {
			t.Fatalf("body=%s status=%d response=%q", body, w.Code, w.Body.String())
		}
	}
}

func TestInterruptHTTPValidPolicyFixtureAcceptsExactTarget(t *testing.T) {
	s, snap := interruptTestServer(t)
	body := `{"session_id":"` + snap.SessionID + `","thread_id":"` + snap.ThreadID + `","turn_id":"` + snap.TurnID + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/live/backend/interrupt", bytes.NewBufferString(body))
	r.SetPathValue("persona", "backend")
	w := httptest.NewRecorder()

	s.handleCodexInterrupt(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d response=%q", w.Code, w.Body.String())
	}
	var got livesession.ControlResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.State != livesession.Interrupting || got.Snapshot.TurnID != snap.TurnID {
		t.Fatalf("snapshot=%+v", got.Snapshot)
	}
}
