package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/permissions"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestFetchFleetPolicy_HappyPath(t *testing.T) {
	// A successful fetch persists the policy under .shipmates/ — run in a
	// temp dir so the package directory stays clean.
	t.Chdir(t.TempDir())
	handler := http.NewServeMux()
	handler.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("missing/wrong bearer, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(permissions.FleetPolicy{
			Deny: []string{"Bash(kubectl delete ns *)"},
		})
	})
	fake := httptest.NewServer(handler)
	defer fake.Close()

	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(permissions.MergedRules{}),
		stopCh: make(chan struct{}),
	}
	// Broadly allow so only the fleet deny would gate the call.
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil))

	if err := s.fetchFleetPolicy(context.Background(), fake.URL, "secret-token"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// After fetch, kubectl-delete-ns must be denied even though ship
	// broadly allows Bash.
	d := s.perms.EvaluateFor("backend", "Bash", map[string]any{"command": "kubectl delete ns production"})
	if d.Effect != permissions.EffectDeny {
		t.Fatalf("expected fleet-deny, got %v (%s)", d.Effect, d.Reason)
	}
}

func TestFetchFleetPolicy_ErrorKeepsLastKnown(t *testing.T) {
	t.Chdir(t.TempDir())
	// First serve a good policy…
	current := &permissions.FleetPolicy{Deny: []string{"Bash(sudo *)"}}
	mux := http.NewServeMux()
	failing := false
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		if failing {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(current)
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	if err := s.fetchFleetPolicy(context.Background(), fake.URL, ""); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	// Now the fleet starts failing. The last-known deny must survive.
	failing = true
	if err := s.fetchFleetPolicy(context.Background(), fake.URL, ""); err == nil {
		t.Fatal("expected error on 500")
	}
	// sudo should still be denied.
	d := s.perms.EvaluateFor("backend", "Bash", map[string]any{"command": "sudo apt install"})
	if d.Effect != permissions.EffectDeny {
		t.Fatalf("last-known policy must survive fetch failure, got %v (%s)", d.Effect, d.Reason)
	}
}

// rulesFromRaw is a small helper used inside this package's tests. Kept
// local so the permissions package doesn't have to export its test helper.
func rulesFromRaw(allow, ask, deny []string) permissions.MergedRules {
	var r permissions.MergedRules
	for _, a := range allow {
		r.Allow = append(r.Allow, permissions.ParseRule(a))
	}
	for _, a := range ask {
		r.Ask = append(r.Ask, permissions.ParseRule(a))
	}
	for _, d := range deny {
		r.Deny = append(r.Deny, permissions.ParseRule(d))
	}
	return r
}

// ---------------------------------------------------------------------------
// L3 / fail-closed: the fleet policy is a security control, so the transport
// it arrives on is checked and the last-known copy survives a restart.
// ---------------------------------------------------------------------------

// A plaintext fleet URL is refused before a request goes out. Otherwise the
// bearer token rides in cleartext and anyone on the path can answer the fetch
// with an empty deny list — switching the Admiral's floor off silently.
func TestFetchFleetPolicy_RefusesPlaintextNonLoopback(t *testing.T) {
	t.Chdir(t.TempDir())
	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	for _, base := range []string{"http://fleet.example.com", "http://10.0.0.5:8443"} {
		err := s.fetchFleetPolicy(context.Background(), base, "secret-token")
		if err == nil {
			t.Fatalf("fetchFleetPolicy(%q) was allowed; the token would go out in cleartext", base)
		}
		if !strings.Contains(err.Error(), "plaintext") {
			t.Errorf("err = %v, want it to name the plaintext transport", err)
		}
	}
	// Loopback plaintext still works — that is the local-development case.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(permissions.FleetPolicy{Deny: []string{"Bash(sudo *)"}})
	})
	fake := httptest.NewServer(mux) // http://127.0.0.1:PORT
	defer fake.Close()
	if err := s.fetchFleetPolicy(context.Background(), fake.URL, "secret-token"); err != nil {
		t.Fatalf("loopback http must stay usable: %v", err)
	}
}

// An unbounded decode hands a remote peer this ship's memory.
func TestFetchFleetPolicy_RefusesAnOversizeBody(t *testing.T) {
	t.Chdir(t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deny":["`))
		chunk := bytes.Repeat([]byte("A"), 64<<10)
		for written := 0; written < maxFleetPolicyBytes+(1<<20); written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`"]}`))
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()

	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	err := s.fetchFleetPolicy(context.Background(), fake.URL, "")
	if err == nil {
		t.Fatal("an oversize policy body was decoded")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want it to name the size cap", err)
	}
}

// THE fail-closed test. A ship that restarts while Fleet Command is
// unreachable used to come up with no fleet deny list at all: the policy lived
// only in memory. It is now persisted and re-applied at boot, before the first
// fetch.
func TestFleetPolicySurvivesARestartWithTheFleetUnreachable(t *testing.T) {
	t.Chdir(t.TempDir())

	// Run one: the fleet is up and hands down a deny rule.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet-policy", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(permissions.FleetPolicy{Deny: []string{"Bash(kubectl delete ns *)"}})
	})
	fake := httptest.NewServer(mux)
	first := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	if err := first.fetchFleetPolicy(context.Background(), fake.URL, ""); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(fleetPolicyCachePath()); err != nil {
		t.Fatalf("the fetched policy was not persisted: %v", err)
	}
	fake.Close() // the fleet goes away

	// Run two: a brand-new ship process, fleet unreachable. The Admiral's
	// floor must be in force before any fetch could possibly succeed.
	second := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	if !second.loadCachedFleetPolicy() {
		t.Fatal("the cached policy was not loaded at boot")
	}
	d := second.perms.EvaluateFor("backend", "Bash", map[string]any{"command": "kubectl delete ns production"})
	if d.Effect != permissions.EffectDeny {
		t.Fatalf("boot with the fleet down = %v (%s), want the cached fleet deny to hold", d.Effect, d.Reason)
	}
}

// Same guarantee through the real entry point, and with the transport refused:
// a plaintext fleet URL must not cost the ship its cached policy.
func TestStartFleetPolicy_PlaintextURLKeepsTheCachedPolicy(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := saveFleetPolicyCache(&permissions.FleetPolicy{Deny: []string{"Bash(rm -rf /*)"}}); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	conf := &project.Config{}
	conf.Fleet.URL = "http://fleet.example.com" // refused: plaintext, remote
	s.startFleetPolicy(context.Background(), conf)
	defer close(s.stopCh)

	d := s.perms.EvaluateFor("backend", "Bash", map[string]any{"command": "rm -rf /home"})
	if d.Effect != permissions.EffectDeny {
		t.Fatalf("effect = %v (%s), want the cached fleet deny to still apply", d.Effect, d.Reason)
	}
}

// A corrupt cache must not stop the ship, and must not be mistaken for policy.
func TestLoadCachedFleetPolicy_IgnoresGarbage(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(project.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fleetPolicyCachePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		perms:  permissions.NewEvaluatorWithRules(rulesFromRaw([]string{"Bash"}, nil, nil)),
		stopCh: make(chan struct{}),
	}
	if s.loadCachedFleetPolicy() {
		t.Fatal("a corrupt cache was reported as a policy in force")
	}
	// A missing cache is the ordinary first-boot case, not an error.
	if err := os.Remove(fleetPolicyCachePath()); err != nil {
		t.Fatal(err)
	}
	if s.loadCachedFleetPolicy() {
		t.Fatal("a missing cache was reported as a policy in force")
	}
}
