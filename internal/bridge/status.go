package bridge

import (
	"encoding/json"
	"net/http"
)

// handleAggregateStatus fans a /status.json poll out to every connected lead
// and returns the union, each mate tagged with its lead's client key and repo.
// This is the one-poll feed for the UI's fleet-wide status dots; the per-lead
// form lives at /api/lead/{key}/status.
func (b *Server) handleAggregateStatus(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		Persona   string `json:"persona"`
		Status    string `json:"status"`
		Since     string `json:"since,omitempty"`
		Tool      string `json:"tool,omitempty"`
		Input     string `json:"input,omitempty"`
		PendingID string `json:"pending_id,omitempty"`
	}
	clients := b.dialer.ListClients()
	results := make(chan []entry, len(clients))
	for _, key := range clients {
		go func(key string) {
			body, status, err := b.proxy(r.Context(), key, "GET", "/status.json", nil)
			if err != nil || status >= 300 {
				results <- nil
				return
			}
			var raw []struct {
				Persona   string `json:"persona"`
				Status    string `json:"status"`
				Since     string `json:"since"`
				Tool      string `json:"tool"`
				Input     string `json:"input"`
				PendingID string `json:"pending_id"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				results <- nil
				return
			}
			b.mu.Lock()
			repo := ""
			if l := b.leads[key]; l != nil {
				repo = l.Repo
			}
			b.mu.Unlock()
			out := make([]entry, 0, len(raw))
			for _, m := range raw {
				out = append(out, entry{
					ClientKey: key,
					Repo:      repo,
					Persona:   m.Persona,
					Status:    m.Status,
					Since:     m.Since,
					Tool:      m.Tool,
					Input:     m.Input,
					PendingID: m.PendingID,
				})
			}
			results <- out
		}(key)
	}
	all := make([]entry, 0)
	for range clients {
		all = append(all, <-results...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}
