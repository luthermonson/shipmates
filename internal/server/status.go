package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// MateStatus is the derived per-persona state served at /status.json. The four
// values mirror the herdr-style glanceable dots, but are derived from
// structured hook/pending/process state rather than PTY-output heuristics:
//
//	blocked — a permission request is waiting on a human decision
//	working — live process with a hook/feed event inside workingWindow
//	idle    — live process, but nothing heard for over workingWindow
//	done    — the persona ran in this server's lifetime and its process exited
type MateStatus struct {
	Persona   string `json:"persona"`
	Status    string `json:"status"`
	Since     string `json:"since,omitempty"`      // last activity, RFC3339
	Tool      string `json:"tool,omitempty"`       // blocked only: the gated tool
	Input     string `json:"input,omitempty"`      // blocked only: human summary of the call
	PendingID string `json:"pending_id,omitempty"` // blocked only: resolve handle
}

// workingWindow is how recently a persona must have emitted an event to count
// as working rather than idle. Hook events fire on every tool use, so a mate
// mid-task refreshes this constantly; a mate waiting on a long silent
// generation can dip to idle and pop back — acceptable for a glanceable dot.
const workingWindow = 20 * time.Second

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	out := s.computeStatus(time.Now())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// computeStatus folds pendings, live processes, and last-activity times into
// one MateStatus per persona this server has seen. Pending beats live beats
// exited: a blocked mate is blocked even though its process is alive.
func (s *Server) computeStatus(now time.Time) []MateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	blocked := map[string]*pending{}
	for _, p := range s.pendings {
		// Deterministic pick when a persona somehow has several pendings:
		// keep the lexicographically-smallest id so repeated polls agree.
		if cur, ok := blocked[p.persona]; !ok || p.id < cur.id {
			blocked[p.persona] = p
		}
	}

	personas := map[string]bool{}
	for p := range s.live {
		personas[p] = true
	}
	for p := range s.ptys {
		personas[p] = true
	}
	for p := range blocked {
		personas[p] = true
	}
	for p := range s.exited {
		personas[p] = true
	}

	out := make([]MateStatus, 0, len(personas))
	for persona := range personas {
		st := MateStatus{Persona: persona}
		switch {
		case blocked[persona] != nil:
			p := blocked[persona]
			st.Status = "blocked"
			st.Tool = p.tool
			st.Input = p.input
			st.PendingID = p.id
		case s.live[persona] != nil || s.ptys[persona] != nil:
			last := s.lastSeen[persona]
			if !last.IsZero() && now.Sub(last) <= workingWindow {
				st.Status = "working"
			} else {
				st.Status = "idle"
			}
		default:
			st.Status = "done"
		}
		if t := s.lastSeen[persona]; !t.IsZero() {
			st.Since = t.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Persona < out[j].Persona })
	return out
}
