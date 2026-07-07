package fleet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyState_MissingFileIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	p := newPolicyState(filepath.Join(tmp, "nope.yaml"))
	pol, err := p.load()
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if len(pol.Deny) != 0 {
		t.Errorf("missing file must yield empty policy, got %+v", pol)
	}
}

func TestPolicyState_LoadsYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "fleet-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(rm -rf /)\n  - Bash(gh secret *)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newPolicyState(path)
	pol, err := p.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(pol.Deny) != 2 {
		t.Errorf("expected 2 deny rules, got %v", pol.Deny)
	}
}

func TestPolicyState_MalformedKeepsLastKnown(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "fleet-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(sudo *)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newPolicyState(path)
	if _, err := p.load(); err != nil {
		t.Fatal(err)
	}
	// Corrupt the file — next load should return the last-known policy,
	// with an error surfaced to the caller.
	if err := os.WriteFile(path, []byte("this is: not: yaml [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol, err := p.load()
	if err == nil {
		t.Fatal("expected an error from malformed YAML")
	}
	if len(pol.Deny) == 0 {
		t.Errorf("bad YAML must not drop last-known policy, got empty")
	}
}

func TestHandleFleetPolicy_ServesJSON(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "fleet-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(kubectl delete ns *)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{policy: newPolicyState(path)}
	r := httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil)
	w := httptest.NewRecorder()
	srv.handleFleetPolicy(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var pol FleetPolicy
	if err := json.NewDecoder(w.Body).Decode(&pol); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pol.Deny) != 1 || pol.Deny[0] != "Bash(kubectl delete ns *)" {
		t.Errorf("wrong body: %+v", pol)
	}
}

func TestHandleFleetPolicy_EmptyWhenNoPath(t *testing.T) {
	srv := &Server{policy: newPolicyState("")}
	r := httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil)
	w := httptest.NewRecorder()
	srv.handleFleetPolicy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var pol FleetPolicy
	if err := json.NewDecoder(w.Body).Decode(&pol); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A ship polling before an Admiral publishes any policy should get a
	// well-formed empty response, not an error.
	if len(pol.Deny) != 0 {
		t.Errorf("expected empty deny, got %+v", pol)
	}
}
