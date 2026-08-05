package permissions

import "sync"

// brigSlot holds the evaluator-side cache of the Brig's compiled kernel
// rules (the Ship's Articles — see internal/brig). Same shape and locking
// discipline as fleetSlot: setter and reader lock together so a torn read
// cannot fail-open mid-replace.
//
// Only ask and deny exist. The Brig is a restrictive overlay by
// construction — it never grants anything, so there is no allow list to
// hold.
type brigSlot struct {
	mu   sync.RWMutex
	ask  []Rule
	deny []Rule
}

func newBrigSlot() *brigSlot { return &brigSlot{} }

// set replaces the cached rule lists. Called once at server construction
// from the operator's resolved brig settings; empty slices mean "brig off".
func (b *brigSlot) set(ask, deny []Rule) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ask = cloneRules(ask)
	b.deny = cloneRules(deny)
}

// overlay returns base with the Brig rules prepended to the ask and deny
// lists. The base is never mutated — callers may share it across personas.
// A slot with no rules returns base unchanged (no copy).
func (b *brigSlot) overlay(base MergedRules) MergedRules {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.ask) == 0 && len(b.deny) == 0 {
		return base
	}
	out := MergedRules{Allow: base.Allow}
	out.Ask = append(cloneRules(b.ask), base.Ask...)
	out.Deny = append(cloneRules(b.deny), base.Deny...)
	return out
}

func cloneRules(in []Rule) []Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]Rule, len(in))
	copy(out, in)
	return out
}
