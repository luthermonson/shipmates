// Package-level fleet policy: the fleet-wide deny list Fleet Command hands to
// every connected ship. Same YAML shape as .claude/settings.json's
// permissions.deny, but authored ONCE by the Admiral and shipped out via a
// tiny read-only HTTP endpoint (`GET /api/fleet-policy`). Ships poll it on
// connect and every few minutes thereafter; a fleet-wide deny always beats any
// ship-side allow.
package fleet

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// FleetPolicy is the on-the-wire shape. Only Deny is honored — allow/ask are
// per-ship concerns and don't belong here.
type FleetPolicy struct {
	Deny []string `json:"deny" yaml:"deny"`
}

// policyState is the fleet's cached copy of the on-disk policy. It's reloaded
// lazily on each request so an operator editing the YAML sees the change on
// the next ship poll, without a fleet restart.
type policyState struct {
	mu       sync.RWMutex
	path     string
	last     FleetPolicy
	lastLoad string // "" until the first successful load; used in errors only
}

// DefaultFleetPolicyPath returns the well-known path where the Admiral's YAML
// lives. Resolution order:
//
//  1. $SHIPMATES_FLEET_POLICY (absolute path override, useful in tests and
//     for operators who keep config in an unusual location).
//  2. ~/.shipmates/fleet-policy.yaml (matches the "~/.shipmates/" convention
//     the rest of shipmates' fleet-side config uses).
//
// Empty return means "no home dir resolvable and no override set" — the
// endpoint will still respond (with an empty policy), so ships can boot.
func DefaultFleetPolicyPath() string {
	if p := strings.TrimSpace(os.Getenv("SHIPMATES_FLEET_POLICY")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".shipmates", "fleet-policy.yaml")
}

// newPolicyState prepares a cache for a specific YAML path. Pass "" to use
// DefaultFleetPolicyPath. The file doesn't have to exist yet — an operator
// can install shipmates fleet first, then drop the YAML in later.
func newPolicyState(path string) *policyState {
	if path == "" {
		path = DefaultFleetPolicyPath()
	}
	return &policyState{path: path}
}

// load reads the YAML file and refreshes the cached policy. A missing file is
// NOT an error — it just means the Admiral hasn't set a policy yet, which is
// a valid state (equivalent to an empty deny list). Malformed YAML is logged
// and the last-known policy is kept so a bad edit doesn't wipe the fleet's
// safety net.
func (p *policyState) load() (FleetPolicy, error) {
	if p.path == "" {
		return FleetPolicy{}, nil
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			p.mu.Lock()
			p.last = FleetPolicy{}
			p.mu.Unlock()
			return FleetPolicy{}, nil
		}
		return FleetPolicy{}, err
	}
	var pol FleetPolicy
	if err := yaml.Unmarshal(b, &pol); err != nil {
		p.mu.RLock()
		last := p.last
		p.mu.RUnlock()
		slog.Warn("fleet-policy YAML invalid, keeping last-known", "path", p.path, "err", err)
		return last, err
	}
	p.mu.Lock()
	p.last = pol
	p.lastLoad = p.path
	p.mu.Unlock()
	return pol, nil
}

// handleFleetPolicy is the GET /api/fleet-policy endpoint. It (re)reads the
// on-disk YAML on every request — the file is tiny and requests are rare
// (once per ship every 5 minutes), so caching would just add a stale-policy
// footgun for the Admiral.
func (b *Server) handleFleetPolicy(w http.ResponseWriter, r *http.Request) {
	if b.policy == nil {
		writeJSON(w, http.StatusOK, FleetPolicy{})
		return
	}
	pol, err := b.policy.load()
	if err != nil {
		// Malformed YAML: still 200 with the last-known policy — a broken
		// fleet-policy.yaml must not fail-open every ship in the fleet.
		slog.Warn("serving stale fleet-policy after load error", "err", err)
	}
	writeJSON(w, http.StatusOK, pol)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
