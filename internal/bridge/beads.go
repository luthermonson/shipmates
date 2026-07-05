package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
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

// handleBeadAssign is bead reassignment as cross-ship dispatch: assign a
// bead to persona@ship and the work MOVES there. Three tunnel calls in
// sequence:
//
//  1. update the assignee on the ship carrying the bead (shared graph — any
//     synced ship works; we use the one the operator is looking at),
//  2. if the target is a different ship, force a SYNCHRONOUS pull there so
//     the bead exists locally before anyone references it,
//  3. tell the target persona to bd show + claim it — the existing tell path
//     spawns the mate if it's asleep, so dispatch also wakes the crew.
func (b *Server) handleBeadAssign(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	id := r.PathValue("id")
	if !beadIDOK(id) {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	var body struct {
		Ship    string `json:"ship"`    // target clientKey (e.g. "homelab:lead")
		Persona string `json:"persona"` // target mate
		Title   string `json:"title"`   // bead title, for the dispatch message
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		strings.TrimSpace(body.Ship) == "" || strings.TrimSpace(body.Persona) == "" {
		http.Error(w, "want {ship, persona, title?}", http.StatusBadRequest)
		return
	}
	persona := strings.TrimSpace(body.Persona)
	shipName, _, _ := strings.Cut(strings.TrimSpace(body.Ship), ":")
	assignee := persona + "@" + shipName

	upd, _ := json.Marshal(map[string]string{"assignee": assignee})
	out, status, err := b.proxy(r.Context(), key, "POST", "/bead/"+url.PathEscape(id)+"/update", upd)
	if err != nil || status >= 300 {
		writeProxied(w, status, out, err)
		return
	}

	if body.Ship != key {
		out, status, err = b.proxy(r.Context(), body.Ship, "POST", "/beads/pull?wait=1", nil)
		if err != nil || status >= 300 {
			msg := fmt.Sprintf("assigned %s, but %s could not pull the graph: %s", assignee, body.Ship, string(out))
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
	}

	title := strings.TrimSpace(body.Title)
	if title != "" {
		title = fmt.Sprintf(" — %q", title)
	}
	msg := fmt.Sprintf(
		"[bridge dispatch] You have been assigned bead %s%s. Run `bd show %s` for the full context, claim it with `bd update %s --claim`, then do the work. Record findings as bd comments and `bd close %s` when done.",
		id, title, id, id, id)
	tell, _ := json.Marshal(map[string]string{"message": msg})
	out, status, err = b.proxy(r.Context(), body.Ship, "POST", "/tell/"+url.PathEscape(persona), tell)
	if err != nil || status >= 300 {
		msg := fmt.Sprintf("assigned %s, but the dispatch tell failed: %s", assignee, string(out))
		http.Error(w, msg, http.StatusBadGateway)
		return
	}

	slog.Info("bead dispatched", "bead", id, "assignee", assignee, "via", key)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"assignee": assignee, "dispatched": "true"})
}

// beadIDOK mirrors the lead-side guard: prefix-hash ids only, so a path
// segment can never smuggle request-line framing into the tunnel proxy.
func beadIDOK(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return id[0] != '-'
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
