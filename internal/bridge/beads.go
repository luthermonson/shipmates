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

	// Target ship offline: the assignee is already on the graph (it'll sync
	// when the ship returns), so queue the wake-up instead of failing.
	if !b.dialer.HasSession(body.Ship) {
		b.mu.Lock()
		b.dispatchQ = append(b.dispatchQ, queuedDispatch{
			Ship: body.Ship, Persona: persona, Bead: id, Title: strings.TrimSpace(body.Title), At: time.Now(),
		})
		b.mu.Unlock()
		slog.Info("bead dispatch queued (ship offline)", "bead", id, "assignee", assignee)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"assignee": assignee, "queued": "true"})
		return
	}

	if err := b.deliverDispatch(r.Context(), queuedDispatch{
		Ship: body.Ship, Persona: persona, Bead: id, Title: strings.TrimSpace(body.Title),
	}, body.Ship != key); err != nil {
		http.Error(w, fmt.Sprintf("assigned %s, but dispatch failed: %v", assignee, err), http.StatusBadGateway)
		return
	}

	slog.Info("bead dispatched", "bead", id, "assignee", assignee, "via", key)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"assignee": assignee, "dispatched": "true"})
}

// queuedDispatch is a wake-up owed to a ship that was offline at assign time.
type queuedDispatch struct {
	Ship    string
	Persona string
	Bead    string
	Title   string
	At      time.Time
}

// deliverDispatch performs the wake-up half of an assignment: force-pull the
// graph on the target ship (so the bead exists locally) then tell the mate.
func (b *Server) deliverDispatch(ctx context.Context, q queuedDispatch, pull bool) error {
	if pull {
		if out, status, err := b.proxy(ctx, q.Ship, "POST", "/beads/pull?wait=1", nil); err != nil || status >= 300 {
			return fmt.Errorf("%s could not pull the graph: %s", q.Ship, string(out))
		}
	}
	title := q.Title
	if title != "" {
		title = fmt.Sprintf(" — %q", title)
	}
	msg := fmt.Sprintf(
		"[bridge dispatch] You have been assigned bead %s%s. Run `bd show %s` for the full context, claim it with `bd update %s --claim`, then do the work. Record findings as bd comments and `bd close %s` when done.",
		q.Bead, title, q.Bead, q.Bead, q.Bead)
	tell, _ := json.Marshal(map[string]string{"message": msg})
	if out, status, err := b.proxy(ctx, q.Ship, "POST", "/tell/"+url.PathEscape(q.Persona), tell); err != nil || status >= 300 {
		return fmt.Errorf("dispatch tell failed: %s", string(out))
	}
	return nil
}

// dispatchQueueTTL bounds how long an undelivered dispatch stays queued. The
// assignee is on the graph regardless; after this long a wake-up tell would
// be more confusing than useful.
const dispatchQueueTTL = 24 * time.Hour

// dispatchSweepLoop retries queued dispatches as their target ships
// reconnect. Delivered and expired entries drop; the rest stay for the next
// sweep.
func (b *Server) dispatchSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		b.mu.Lock()
		pending := b.dispatchQ
		b.mu.Unlock()
		if len(pending) == 0 {
			continue
		}
		var keep []queuedDispatch
		for _, q := range pending {
			if time.Since(q.At) > dispatchQueueTTL {
				slog.Warn("queued dispatch expired", "bead", q.Bead, "ship", q.Ship)
				continue
			}
			if !b.dialer.HasSession(q.Ship) {
				keep = append(keep, q)
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := b.deliverDispatch(dctx, q, true)
			cancel()
			if err != nil {
				slog.Warn("queued dispatch retry failed", "bead", q.Bead, "ship", q.Ship, "err", err)
				keep = append(keep, q)
				continue
			}
			slog.Info("queued dispatch delivered", "bead", q.Bead, "ship", q.Ship, "persona", q.Persona)
		}
		b.mu.Lock()
		// entries assigned while we swept were appended to b.dispatchQ; keep them
		b.dispatchQ = append(keep, b.dispatchQ[len(pending):]...)
		b.mu.Unlock()
	}
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
