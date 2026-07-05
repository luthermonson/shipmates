package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// handleBeadsNudge is a lead's "we just pushed" callback: fan a pull-now out
// to every OTHER connected ship so the shared graph converges in seconds
// instead of a heartbeat. Fan-out is async on a background context — the
// nudging lead shouldn't block on the slowest ship's pull, and r.Context()
// dies when this handler returns.
func (b *Server) handleBeadsNudge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	targets := 0
	for _, key := range b.dialer.ListClients() {
		if key == body.From {
			continue
		}
		targets++
		go func(key string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, status, err := b.proxy(ctx, key, "POST", "/beads/pull", nil); err != nil && status != http.StatusNotFound {
				slog.Warn("beads nudge: pull dispatch failed", "ship", key, "err", err)
			}
		}(key)
	}
	slog.Debug("beads nudge", "from", body.From, "ships", targets)
	w.WriteHeader(http.StatusAccepted)
}

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
		ExternalRef string   `json:"external_ref,omitempty"`
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
