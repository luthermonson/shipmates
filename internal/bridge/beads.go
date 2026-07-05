package bridge

import (
	"encoding/json"
	"net/http"
	"sort"
)

// handleAggregateBeads fans /beads.json out to every connected lead and
// returns the union deduped by bead id — ships syncing one shared graph
// (phase 5) would otherwise show every bead once per ship. Each entry lists
// the ships it was seen on, so the UI can attribute and route detail fetches.
func (b *Server) handleAggregateBeads(w http.ResponseWriter, r *http.Request) {
	type bead struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description,omitempty"`
		Status      string   `json:"status"`
		Priority    *int     `json:"priority,omitempty"`
		Ships       []string `json:"ships"`
	}
	type shipBeads struct {
		key   string
		beads []bead
	}
	clients := b.dialer.ListClients()
	results := make(chan shipBeads, len(clients))
	for _, key := range clients {
		go func(key string) {
			body, status, err := b.proxy(r.Context(), key, "GET", "/beads.json", nil)
			if err != nil || status >= 300 {
				results <- shipBeads{key: key} // no beads workspace or unreachable
				return
			}
			var raw []bead
			if err := json.Unmarshal(body, &raw); err != nil {
				results <- shipBeads{key: key}
				return
			}
			results <- shipBeads{key: key, beads: raw}
		}(key)
	}

	merged := map[string]*bead{}
	for range clients {
		sb := <-results
		for i := range sb.beads {
			bd := sb.beads[i]
			if existing, ok := merged[bd.ID]; ok {
				existing.Ships = append(existing.Ships, sb.key)
				continue
			}
			bd.Ships = []string{sb.key}
			merged[bd.ID] = &bd
		}
	}
	out := make([]bead, 0, len(merged))
	for _, v := range merged {
		sort.Strings(v.Ships)
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
