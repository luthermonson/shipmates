package permissions

import "sync"

// FleetPolicy is the fleet-wide deny list Fleet Command hands down to every
// ship. Only Deny is honored — allow/ask are per-ship concerns. Deny in the
// fleet policy is unconditional: no ship-side rule, persona overlay, or
// time-box can override it.
type FleetPolicy struct {
	// Deny is the raw rule strings, same shape as settings.json's
	// permissions.deny (`Bash(rm -rf /)`, `Bash(kubectl delete ns *)`, …).
	Deny []string `json:"deny" yaml:"deny"`
}

// fleetSlot holds the ship-side cache of the fleet policy. It's a struct so
// setter and reader lock together — a torn read (empty policy at the moment a
// new one is being installed) could silently fail-open on a dangerous command.
type fleetSlot struct {
	mu     sync.RWMutex
	rules  []Rule
	loaded bool
}

func newFleetSlot() *fleetSlot { return &fleetSlot{} }

// set replaces the cached fleet deny list. Called by the ship's periodic
// fetcher; passing nil clears the cache back to "no fleet rules known".
func (f *fleetSlot) set(pol *FleetPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pol == nil {
		f.rules = nil
		f.loaded = false
		return
	}
	parsed := make([]Rule, 0, len(pol.Deny))
	for _, raw := range pol.Deny {
		parsed = append(parsed, ParseRule(raw))
	}
	f.rules = parsed
	f.loaded = true
}

// snapshot returns a copy of the current deny list. Reading against a copy
// keeps evaluation lock-free after the initial RLock.
func (f *fleetSlot) snapshot() []Rule {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.rules) == 0 {
		return nil
	}
	out := make([]Rule, len(f.rules))
	copy(out, f.rules)
	return out
}
