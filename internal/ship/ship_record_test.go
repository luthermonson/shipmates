package ship

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	projectstate "github.com/luthermonson/shipmates/internal/project"
)

// TestServerRecordDiscoveryAndControl covers the supervisor's server.json
// path end to end: the discovery record written by `server serve` is what
// serverHealthy and shutdownServer must read (server.port/server.pid no
// longer exist), and /shutdown sits behind the control token, so the
// graceful-stop request must present it.
func TestServerRecordDiscoveryAndControl(t *testing.T) {
	root, err := projectstate.CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := projectstate.ScopeID(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectstate.EnsureServerStateDirectory(root); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Shipmates-Project", scope)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasPrefix(gotAuth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	token := strings.Repeat("t", 32)
	record, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"project_root":   root,
		"project_scope":  scope,
		"address":        strings.TrimPrefix(srv.URL, "http://"),
		"pid":            os.Getpid(),
		"control_token":  token,
	})
	if err != nil {
		t.Fatal(err)
	}
	record = append(record, '\n')
	if err := projectstate.WriteServerStateFile(root, "server.json", record); err != nil {
		t.Fatal(err)
	}

	rec, ok := readServerRecord(root)
	if !ok || rec.pid != os.Getpid() || rec.port == 0 || rec.token != token {
		t.Fatalf("readServerRecord=%+v ok=%v", rec, ok)
	}
	if !serverHealthy(root) {
		t.Fatal("serverHealthy should accept the live record")
	}
	if !shutdownServer(root) {
		t.Fatal("shutdownServer should succeed against the live record")
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("shutdown Authorization=%q, want bearer control token", gotAuth)
	}

	// A record naming a different project root is a poisoned or copied file
	// and must read as absent.
	forged, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"project_root":   root + "-elsewhere",
		"project_scope":  scope,
		"address":        strings.TrimPrefix(srv.URL, "http://"),
		"pid":            os.Getpid(),
		"control_token":  token,
	})
	if err != nil {
		t.Fatal(err)
	}
	forged = append(forged, '\n')
	if err := projectstate.WriteServerStateFile(root, "server.json", forged); err != nil {
		t.Fatal(err)
	}
	if _, ok := readServerRecord(root); ok {
		t.Fatal("record naming another project root must be rejected")
	}
}
