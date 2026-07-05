package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Beads integration (fleet phase 4, single ship). Mates are beads-native: bd
// ships its own Claude Code integration and the mates create/close their own
// work beads. The lead server only provides plumbing:
//
//   - detect a beads workspace (.beads/ in the project root)
//   - inject `bd prime` output into mate spawns — live/PTY mates run
//     `claude -p`/`claude` under our control and bd's SessionStart auto-prime
//     doesn't fire there, so we pass it via --append-system-prompt
//   - serve GET /beads.json (bd list --json) so the bridge can render the
//     ship's work graph read-only
//
// No shadow-writing of feed events: a "you alive?" tell is not a task. The
// graph's content is owned by the agents and the humans, not the transport.

// beadsEnabled reports whether this project is a beads workspace.
func beadsEnabled() bool {
	st, err := os.Stat(".beads")
	return err == nil && st.IsDir()
}

// beadsPrimeCache avoids re-running bd prime for every mate spawned in quick
// succession; prime output changes slowly (memories + protocol text).
var (
	beadsPrimeMu   sync.Mutex
	beadsPrimeOut  string
	beadsPrimeAt   time.Time
	beadsPrimeTTL  = 5 * time.Minute
	beadsCmdBudget = 20 * time.Second
)

// beadsPrime returns `bd prime` output for spawn injection, cached. Empty
// string when beads is absent or bd fails — mates then run exactly as before.
func beadsPrime() string {
	if !beadsEnabled() {
		return ""
	}
	beadsPrimeMu.Lock()
	defer beadsPrimeMu.Unlock()
	if time.Since(beadsPrimeAt) < beadsPrimeTTL && beadsPrimeOut != "" {
		return beadsPrimeOut
	}
	out, err := runBD("prime")
	if err != nil {
		return ""
	}
	beadsPrimeOut = out
	beadsPrimeAt = time.Now()
	return beadsPrimeOut
}

// runBD executes a bd subcommand in the project root with a hard time budget
// (bd auto-starts its embedded dolt server on first use, which can be slow).
func runBD(args ...string) (string, error) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bdPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
	case <-time.After(beadsCmdBudget):
		_ = cmd.Process.Kill()
		<-done
		return "", errBeadsTimeout
	}
	return stdout.String(), nil
}

type beadsTimeoutError struct{}

func (beadsTimeoutError) Error() string { return "bd command timed out" }

var errBeadsTimeout = beadsTimeoutError{}

// beadsSyncRemote reports whether the workspace has a sync remote configured
// (a non-comment `sync.remote:` entry in .beads/config.yaml). bd writes this
// on `bd dolt remote add`; the fleet heartbeat only runs when it's present.
func beadsSyncRemote() bool {
	f, err := os.Open(".beads/config.yaml")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "sync.remote:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "sync.remote:")), `"'`)
			return v != ""
		}
	}
	return false
}

// beadsSyncInterval is the fleet-graph heartbeat cadence — now the SAFETY NET
// under the real-time nudge path, catching anything the watcher missed. Dolt
// merges are cell-level and bead ids are hash-based, so late syncs converge
// instead of conflicting — the interval is a freshness knob, not a
// correctness one.
const beadsSyncInterval = 3 * time.Minute

// beadsWatchInterval is how often the watcher probes for out-of-band local
// writes (mates running bd themselves). A 2KB file read per tick.
const beadsWatchInterval = 3 * time.Second

// beadsStateSig fingerprints the workspace's data state: the content of the
// dolt noms manifest(s), which carry the root hash and change exactly when
// the graph changes. Chosen over file mtimes (journal writes churn those
// without data changes) and over `.beads/last-touched` (bd updates it on
// SHOW, so reads would false-trigger).
func beadsStateSig() string {
	matches, _ := filepath.Glob(filepath.Join(".beads", "embeddeddolt", "*", ".dolt", "noms", "manifest"))
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	h := sha256.New()
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// markBeadsDirty records a local graph write and wakes the sync loop: the
// change should be pushed AND announced to the fleet. Called by the watcher
// and by the bridge-mediated write endpoints (create/close).
func (s *Server) markBeadsDirty() {
	s.beadsMu.Lock()
	s.beadsDirty = true
	s.beadsMu.Unlock()
	select {
	case s.beadsTrigger <- struct{}{}:
	default: // a wake-up is already queued; the loop will see dirty
	}
}

// requestBeadsPull wakes the sync loop without marking dirty — the bridge
// says another ship pushed; pull now, nothing local to announce.
func (s *Server) requestBeadsPull() {
	select {
	case s.beadsTrigger <- struct{}{}:
	default:
	}
}

// handleBeadsPull is the bridge's nudge target. ?wait=1 pulls synchronously
// before answering — the dispatch path needs "the bead is HERE now" before
// telling a mate to bd show it, not "a pull is queued".
func (s *Server) handleBeadsPull(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() || !beadsSyncRemote() {
		http.Error(w, "no synced beads workspace", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("wait") == "1" {
		if _, err := runBD("dolt", "pull"); err != nil {
			http.Error(w, "bd dolt pull: "+err.Error(), http.StatusBadGateway)
			return
		}
		// keep the watcher from announcing our own pull as a local write
		s.beadsMu.Lock()
		s.beadsSig = beadsStateSig()
		s.beadsMu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	s.requestBeadsPull()
	w.WriteHeader(http.StatusAccepted)
}

// handleBeadUpdate applies field updates to one bead: assignee (persona@ship
// — the fleet dispatch convention), priority, and human edits to title/
// description from the bridge UI. Values ride flag=value form, same
// injection rules as create.
func (s *Server) handleBeadUpdate(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if !beadIDOK(id) {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	var body struct {
		Assignee    string  `json:"assignee"`
		Priority    string  `json:"priority"`
		Title       string  `json:"title"`
		Description *string `json:"description"` // pointer: "" is a real edit (clear it)
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	args := []string{"update", id}
	if a := strings.TrimSpace(body.Assignee); a != "" {
		args = append(args, "--assignee="+a)
	}
	if p := strings.TrimSpace(body.Priority); p != "" {
		if len(p) != 1 || p[0] < '0' || p[0] > '4' {
			http.Error(w, "priority must be 0-4", http.StatusBadRequest)
			return
		}
		args = append(args, "--priority="+p)
	}
	if t := strings.TrimSpace(body.Title); t != "" {
		args = append(args, "--title="+t)
	}
	if body.Description != nil {
		args = append(args, "--description="+strings.TrimSpace(*body.Description), "--allow-empty-description")
	}
	if len(args) == 2 {
		http.Error(w, "want {assignee?, priority?, title?, description?}", http.StatusBadRequest)
		return
	}
	out, err := runBD(args...)
	if err != nil {
		http.Error(w, "bd update: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.addEvent(Event{Persona: "(bridge)", Type: "bead:update", Text: id + " → " + strings.TrimSpace(body.Assignee+" "+body.Priority)})
	invalidateBeadsSummary()
	s.markBeadsDirty() // announce to the fleet without waiting on the watcher
	_, _ = w.Write([]byte(out))
}

// beadsWatchLoop probes the workspace signature and marks dirty on change.
// The sync loop re-baselines the signature after every pull/push, so changes
// caused by syncing (our own pulls) never re-trigger — only out-of-band
// writes (a mate's bd create/close/update) get announced.
func (s *Server) beadsWatchLoop(ctx context.Context) {
	s.beadsMu.Lock()
	s.beadsSig = beadsStateSig()
	s.beadsMu.Unlock()
	ticker := time.NewTicker(beadsWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		sig := beadsStateSig()
		s.beadsMu.Lock()
		changed := sig != "" && sig != s.beadsSig
		if changed {
			s.beadsSig = sig
		}
		s.beadsMu.Unlock()
		if changed {
			s.markBeadsDirty()
		}
	}
}

// beadsSyncLoop pulls then pushes the bead graph — on the heartbeat, and
// immediately when woken (local write detected, or the bridge nudged us to
// pull another ship's push). After a dirty sync it notifies the bridge so
// the rest of the fleet pulls within seconds instead of a heartbeat.
// Failures log and never disturb serving.
func (s *Server) beadsSyncLoop(ctx context.Context) {
	if !beadsEnabled() || !beadsSyncRemote() {
		return
	}
	go s.beadsWatchLoop(ctx)
	slog.Info("beads sync started", "heartbeat", beadsSyncInterval, "watch", beadsWatchInterval)
	ticker := time.NewTicker(beadsSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
		case <-s.beadsTrigger:
		}

		s.beadsMu.Lock()
		wasDirty := s.beadsDirty
		s.beadsDirty = false
		s.beadsMu.Unlock()

		pushed := true
		if _, err := runBD("dolt", "pull"); err != nil {
			slog.Warn("beads pull failed", "err", err)
			pushed = false
		} else if _, err := runBD("dolt", "push"); err != nil {
			slog.Warn("beads push failed", "err", err)
			pushed = false
		}

		// absorb this sync's own mutations so the watcher doesn't re-fire
		s.beadsMu.Lock()
		s.beadsSig = beadsStateSig()
		if !pushed && wasDirty {
			s.beadsDirty = true // announce on the next successful sync instead
		}
		s.beadsMu.Unlock()

		if pushed && wasDirty {
			s.notifyBeadsNudge()
		}
	}
}

// notifyBeadsNudge tells the bridge "we pushed — have the other ships pull."
// Plain outbound HTTPS with the same bearer token the tunnel uses; a failure
// just means the fleet falls back to heartbeat freshness.
func (s *Server) notifyBeadsNudge() {
	s.mu.Lock()
	url, token, key := s.bridgeURL, s.bridgeToken, s.bridgeKey
	s.mu.Unlock()
	if url == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"from": key})
	req, err := http.NewRequest("POST", strings.TrimRight(url, "/")+"/api/beads/nudge", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("beads nudge failed", "err", err)
		return
	}
	defer resp.Body.Close()
	slog.Debug("beads nudge sent", "status", resp.StatusCode)
}

// beadsSummaryCache backs the ⛃ badge poll: the count changes slowly and a
// bd spawn per UI tick would be silly, so it's cached briefly.
var (
	beadsSummaryMu   sync.Mutex
	beadsSummaryAt   time.Time
	beadsSummaryOpen int
	beadsSummaryTTL  = 30 * time.Second
)

// invalidateBeadsSummary drops the badge cache after a write so the count
// the operator just changed doesn't lag behind their own action.
func invalidateBeadsSummary() {
	beadsSummaryMu.Lock()
	beadsSummaryAt = time.Time{}
	beadsSummaryMu.Unlock()
}

// handleBeadsSummary serves a tiny {open: N} for the bridge's badge.
func (s *Server) handleBeadsSummary(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	beadsSummaryMu.Lock()
	fresh := time.Since(beadsSummaryAt) < beadsSummaryTTL
	open := beadsSummaryOpen
	beadsSummaryMu.Unlock()
	if !fresh {
		out, err := runBD("list", "--json")
		if err != nil {
			http.Error(w, "bd list: "+err.Error(), http.StatusBadGateway)
			return
		}
		var beads []json.RawMessage
		_ = json.Unmarshal([]byte(out), &beads)
		open = len(beads)
		beadsSummaryMu.Lock()
		beadsSummaryOpen, beadsSummaryAt = open, time.Now()
		beadsSummaryMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"open": open})
}

// beadIDOK guards the show endpoint: bd ids are prefix-hash (proj-c03, with
// dotted epics like proj-a3f8.1). Reject anything else so a path segment can
// never smuggle flags or shell-ish input into the bd invocation.
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

// handleBeadShow serves one bead's full record: `bd show <id> --json`.
func (s *Server) handleBeadShow(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if !beadIDOK(id) {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	out, err := runBD("show", id, "--json")
	if err != nil {
		http.Error(w, "bd show: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(out))
}

// handleBeadCreate creates a bead from the bridge: `bd create --json` with
// the fields the UI's quick form offers. Values ride flag=value form so a
// leading dash in user text can never be parsed as a flag.
func (s *Server) handleBeadCreate(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	var body struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    string `json:"priority"`
		Type        string `json:"type"`
		ExternalRef string `json:"external_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
		http.Error(w, "want {title, description?, priority?, type?, external_ref?}", http.StatusBadRequest)
		return
	}
	args := []string{"create", "--json", "--title=" + strings.TrimSpace(body.Title)}
	if d := strings.TrimSpace(body.Description); d != "" {
		args = append(args, "--description="+d)
	}
	if p := strings.TrimSpace(body.Priority); p != "" {
		if len(p) != 1 || p[0] < '0' || p[0] > '4' {
			http.Error(w, "priority must be 0-4", http.StatusBadRequest)
			return
		}
		args = append(args, "--priority="+p)
	}
	if t := strings.TrimSpace(body.Type); t != "" {
		switch t {
		case "bug", "feature", "task", "epic", "chore", "decision":
			args = append(args, "--type="+t)
		default:
			http.Error(w, "unknown issue type", http.StatusBadRequest)
			return
		}
	}
	if ref := strings.TrimSpace(body.ExternalRef); ref != "" {
		args = append(args, "--external-ref="+ref)
	}
	out, err := runBD(args...)
	if err != nil {
		http.Error(w, "bd create: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.addEvent(Event{Persona: "(bridge)", Type: "bead:create", Text: strings.TrimSpace(body.Title)})
	invalidateBeadsSummary()
	s.markBeadsDirty() // announce to the fleet without waiting on the watcher
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(out))
}

// handleBeadClose closes one bead: `bd close <id> [--reason=...]`.
func (s *Server) handleBeadClose(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	id := r.PathValue("id")
	if !beadIDOK(id) {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	args := []string{"close", id}
	if reason := strings.TrimSpace(body.Reason); reason != "" {
		args = append(args, "--reason="+reason)
	}
	out, err := runBD(args...)
	if err != nil {
		http.Error(w, "bd close: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.addEvent(Event{Persona: "(bridge)", Type: "bead:close", Text: id})
	invalidateBeadsSummary()
	s.markBeadsDirty() // announce to the fleet without waiting on the watcher
	_, _ = w.Write([]byte(out))
}

// handleBeadsJSON serves the ship's bead graph: `bd list --json`, passed
// through verbatim. 404 when the project has no beads workspace so the bridge
// can distinguish "no beads here" from "empty graph".
func (s *Server) handleBeadsJSON(w http.ResponseWriter, r *http.Request) {
	if !beadsEnabled() {
		http.Error(w, "no beads workspace", http.StatusNotFound)
		return
	}
	args := []string{"list", "--json"}
	if strings.TrimSpace(r.URL.Query().Get("all")) == "1" {
		args = append(args, "--all")
	}
	out, err := runBD(args...)
	if err != nil {
		http.Error(w, "bd list: "+err.Error(), http.StatusBadGateway)
		return
	}
	if strings.TrimSpace(out) == "" {
		out = "[]"
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(out))
}
