package fleet

import (
	"encoding/json"
	"net/http"

	"github.com/luthermonson/shipmates/internal/api"
)

// handleAggregateStatus fans a /status.json poll out to every connected captain
// and returns the union, each mate tagged with its captain's client key and repo.
// This is the one-poll feed for the UI's fleet-wide status dots.
func (b *Server) handleAggregateStatus(w http.ResponseWriter, r *http.Request) {
	clients := b.dialer.ListClients()
	results := make(chan []api.FleetMateStatus, len(clients))
	for _, key := range clients {
		go func(key string) {
			body, status, err := b.proxy(r.Context(), key, "GET", "/status.json", nil)
			if err != nil || status >= 300 {
				results <- nil
				return
			}
			var raw []api.MateStatus
			if err := json.Unmarshal(body, &raw); err != nil {
				results <- nil
				return
			}
			b.mu.Lock()
			repo := ""
			if l := b.captains[key]; l != nil {
				repo = l.Repo
			}
			b.mu.Unlock()
			out := make([]api.FleetMateStatus, 0, len(raw))
			for _, m := range raw {
				out = append(out, api.FleetMateStatus{MateStatus: m, ClientKey: key, Repo: repo})
			}
			results <- out
		}(key)
	}
	all := make([]api.FleetMateStatus, 0)
	for range clients {
		all = append(all, <-results...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}
