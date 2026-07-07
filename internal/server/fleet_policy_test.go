package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luthermonson/shipmates/internal/permissions"
)

func TestFetchFleetPolicy_HappyPath(t *testing.T) {
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
