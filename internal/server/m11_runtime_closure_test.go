package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
)

func TestM11RemovedLegacyRoutesAreUnregistered(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	canary := filepath.Join(root, ".legacy-runtime", "route-canary")
	if err := os.MkdirAll(filepath.Dir(canary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canary, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := m11SnapshotServerState(t, root)
	s := NewWithCodexOptions(codexapp.StartOptions{})
	s.projectScope, s.controlToken = "scope", strings.Repeat("t", 43)
	h := s.handler()
	paths := []string{"/events", "/tell/x", "/attach", "/hook/x/y", "/pending", "/pending.json", "/resolve/x", "/feed", "/status.json", "/pty/x/start", "/pty/x/stream", "/pty/x/snapshot", "/pty/x/input", "/pty/x/resize", "/pty/x/takeover", "/pty/x/release", "/beads.json", "/beads/summary", "/bead/x", "/bead", "/bead/x/close", "/bead/x/update", "/beads/pull", "/connect", "/api/fleet-policy", "/api/conversation", "/api/tts", "/api/stt", "/api/voice/config"}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace} {
			for _, suffix := range []string{"", "/", "?probe=1"} {
				for _, contentType := range []string{"application/json", "text/plain", "multipart/form-data; boundary=x"} {
					r := httptest.NewRequest(method, path+suffix, strings.NewReader("{}"))
					r.Header.Set("Content-Type", contentType)
					r.Header.Set("Connection", "Upgrade")
					r.Header.Set("Upgrade", "websocket")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != http.StatusNotFound {
						t.Fatalf("%s %s %s = %d, want 404", method, path+suffix, contentType, w.Code)
					}
				}
			}
		}
	}
	after := m11SnapshotServerState(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("removed route probes mutated state: before=%+v after=%+v", before, after)
	}
	if raw, err := os.ReadFile(canary); err != nil || string(raw) != "unchanged" {
		t.Fatalf("route probes changed LegacyRuntime canary: %q %v", raw, err)
	}
}

func TestM11ShutdownRequiresExactLocalControlAuthority(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		s := NewWithCodexOptions(codexapp.StartOptions{})
		s.projectScope, s.controlToken = "scope", strings.Repeat("t", 43)
		r := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
		r.Header.Set("X-Shipmates-Project", "scope")
		if authorized {
			r.Header.Set("Authorization", "Bearer "+s.controlToken)
		}
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, r)
		if authorized && w.Code != http.StatusAccepted {
			t.Fatalf("authorized = %d", w.Code)
		}
		if !authorized && w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized = %d", w.Code)
		}
	}
}

func TestM11ServerConstructionCreatesNoLegacyState(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	_ = NewWithCodexOptions(codexapp.StartOptions{})
	for _, rel := range []string{".shipmates/inbox", ".shipmates/pending", ".shipmates/grants", ".shipmates/fleet-policy", ".shipmates/pty", ".legacy-runtime"} {
		if _, err := os.Lstat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("legacy state %s exists: %v", rel, err)
		}
	}
}

type m11ServerState struct {
	legacyFiles []string
	children    string
}

func m11SnapshotServerState(t *testing.T, root string) m11ServerState {
	t.Helper()
	var legacy []string
	patterns := []string{
		".legacy-runtime", ".shipmates/inbox", ".shipmates/pending", ".shipmates/grants",
		".shipmates/fleet-policy", ".shipmates/pty", ".shipmates/sessions/*.session",
		".shipmates/sessions/*pending*", ".shipmates/sessions/*grant*",
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			rel, _ := filepath.Rel(root, match)
			legacy = append(legacy, filepath.ToSlash(rel))
		}
	}
	sort.Strings(legacy)
	children := ""
	if runtime.GOOS == "linux" {
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", os.Getpid(), os.Getpid())); err == nil {
			children = strings.TrimSpace(string(raw))
		}
	}
	return m11ServerState{legacyFiles: legacy, children: children}
}

func TestM11ProductionServerStartupShutdownLeavesNoLegacyAuthority(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	canary := filepath.Join(root, ".legacy-runtime", "settings.json")
	if err := os.MkdirAll(filepath.Dir(canary), 0o700); err != nil {
		t.Fatal(err)
	}
	canaryRaw := []byte(`{"hooks":{"SessionStart":[{"command":"legacyagent"}]}}`)
	if err := os.WriteFile(canary, canaryRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	before := m11SnapshotServerState(t, root)
	baselineGoroutines := runtime.NumGoroutine()
	s := NewWithCodexOptions(codexapp.StartOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case <-s.Ready():
	case err := <-done:
		if errors.Is(err, fs.ErrPermission) || strings.Contains(fmt.Sprint(err), "operation not permitted") {
			t.Skipf("local listener prohibited by test sandbox: %v", err)
		}
		t.Fatalf("server startup: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server startup timeout")
	}

	recordRaw, err := os.ReadFile(filepath.Join(root, ".shipmates", "sessions", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record discoveryRecord
	if err := json.Unmarshal(recordRaw, &record); err != nil {
		t.Fatal(err)
	}
	during := m11SnapshotServerState(t, root)
	if !reflect.DeepEqual(before.legacyFiles, during.legacyFiles) {
		t.Fatalf("legacy state changed during startup: before=%v during=%v", before.legacyFiles, during.legacyFiles)
	}
	if before.children != during.children {
		t.Fatalf("server startup launched child process: before=%q during=%q", before.children, during.children)
	}
	if s.controlToken == "" || s.remoteSteer == nil || s.remoteInterrupt == nil {
		t.Fatal("canonical server authority was not initialized")
	}
	for _, forbidden := range []string{"pending", "grant", "inbox", "pty", "poller", "credential", "legacyagent", "fleetpolicy"} {
		typ := reflect.TypeOf(s).Elem()
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), forbidden) {
				t.Fatalf("legacy server state field remains: %s", typ.Field(i).Name)
			}
		}
	}

	req, _ := http.NewRequest(http.MethodPost, "http://"+record.Address+"/shutdown", nil)
	req.Header.Set("X-Shipmates-Project", record.ProjectScope)
	req.Header.Set("Authorization", "Bearer "+record.ControlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status=%d", resp.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server shutdown: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server shutdown timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baselineGoroutines+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > baselineGoroutines+2 {
		t.Fatalf("server workers remained after shutdown: before=%d after=%d", baselineGoroutines, runtime.NumGoroutine())
	}
	after := m11SnapshotServerState(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy/process state changed across lifecycle: before=%+v after=%+v", before, after)
	}
	gotCanary, err := os.ReadFile(canary)
	if err != nil || !reflect.DeepEqual(gotCanary, canaryRaw) {
		t.Fatalf("LegacyRuntime canary changed: %q %v", gotCanary, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".shipmates", "sessions", "server.json")); !os.IsNotExist(err) {
		t.Fatalf("server credential/discovery record retained: %v", err)
	}
}
