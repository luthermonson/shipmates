package server

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
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
