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
	// Point the default-path resolution at a file that cannot exist. Without
	// this the assertion depends on whether the machine running the tests
	// happens to have a real ~/.shipmates/fleet-policy.yaml.
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "absent.yaml"))
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

func TestDefaultFleetPolicyPath_EnvOverrideWins(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-policy.yaml")
	t.Setenv("SHIPMATES_FLEET_POLICY", "  "+want+"  ")
	if got := DefaultFleetPolicyPath(); got != want {
		t.Fatalf("env override not honored (and not trimmed): got %q want %q", got, want)
	}
}

func TestDefaultFleetPolicyPath_FallsBackToHome(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", "")
	got := DefaultFleetPolicyPath()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if got != "" {
			t.Fatalf("no resolvable home should yield an empty path, got %q", got)
		}
		return
	}
	want := filepath.Join(home, ".shipmates", "fleet-policy.yaml")
	if got != want {
		t.Fatalf("home fallback: got %q want %q", got, want)
	}
}

// The endpoint re-reads on every request on purpose: an Admiral editing the
// YAML must see the change on the next ship poll, without a fleet restart.
func TestHandleFleetPolicy_HotReloadsBetweenRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(sudo *)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{policy: newPolicyState(path)}

	get := func() FleetPolicy {
		t.Helper()
		w := httptest.NewRecorder()
		srv.handleFleetPolicy(w, httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		var pol FleetPolicy
		if err := json.NewDecoder(w.Body).Decode(&pol); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return pol
	}

	if got := get(); len(got.Deny) != 1 {
		t.Fatalf("first read: %+v", got)
	}
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(sudo *)\n  - Bash(rm -rf /)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got.Deny) != 2 {
		t.Fatalf("edit not picked up without a restart: %+v", got)
	}

	// A policy deleted entirely means "no fleet-wide deny" — the endpoint must
	// keep answering so ships can still boot.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got.Deny) != 0 {
		t.Fatalf("removed policy should serve empty, got %+v", got)
	}
}

// A broken YAML edit must not fail-open every ship in the fleet: the endpoint
// still answers 200 with the last-known deny list.
func TestHandleFleetPolicy_MalformedServesLastKnown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-policy.yaml")
	if err := os.WriteFile(path, []byte("deny:\n  - Bash(sudo *)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{policy: newPolicyState(path)}
	w := httptest.NewRecorder()
	srv.handleFleetPolicy(w, httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}

	if err := os.WriteFile(path, []byte("deny: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	srv.handleFleetPolicy(w2, httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("malformed YAML must still answer 200, got %d", w2.Code)
	}
	var pol FleetPolicy
	if err := json.NewDecoder(w2.Body).Decode(&pol); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pol.Deny) != 1 || pol.Deny[0] != "Bash(sudo *)" {
		t.Fatalf("last-known policy not served: %+v", pol)
	}
}

// A fleet with no policy state at all (defensive nil check) must still serve a
// well-formed empty policy rather than panic in a ship's poll path.
func TestHandleFleetPolicy_NilPolicyState(t *testing.T) {
	srv := &Server{}
	w := httptest.NewRecorder()
	srv.handleFleetPolicy(w, httptest.NewRequest(http.MethodGet, "/api/fleet-policy", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var pol FleetPolicy
	if err := json.NewDecoder(w.Body).Decode(&pol); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pol.Deny) != 0 {
		t.Fatalf("want empty policy, got %+v", pol)
	}
}

// The wire shape is consumed by ships; deny must serialize as a JSON array
// under the "deny" key even when empty.
func TestFleetPolicy_WireShape(t *testing.T) {
	raw, err := json.Marshal(FleetPolicy{Deny: []string{"Bash(sudo *)"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"deny":["Bash(sudo *)"]}` {
		t.Fatalf("unexpected wire shape: %s", raw)
	}
}
