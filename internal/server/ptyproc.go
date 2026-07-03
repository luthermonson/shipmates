package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/luthermonson/shipmates/internal/project"
)

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ptyProc is a PTY-hosted interactive mate: `claude --agent <persona>` running
// under a real pseudo-terminal (ConPTY on Windows). Its screen bytes are the
// human-facing channel; the persona's hooks still post to this server, so the
// machine-facing channel (status dots, permission gate) works unchanged.
type ptyProc struct {
	persona string
	pt      pty.Pty
	cmd     *pty.Cmd

	mu      sync.Mutex
	ring    *ring
	subs    map[int]chan []byte
	nextSub int
	closed  bool
}

// ptyRingCap is the per-mate backscroll: enough for a screenful of heavy TUI
// redraw plus recent history, small enough to hold dozens of mates in memory.
const ptyRingCap = 64 * 1024

// subBufChunks bounds each subscriber's channel. A slow consumer drops oldest
// frames rather than backpressuring the PTY pump — never stall the agent for
// a slow browser tab.
const subBufChunks = 256

// ensurePTY returns the persona's PTY-hosted mate, spawning one if needed.
// PTY mates and stream-json live mates are separate pools: `tell` drives the
// headless pool; PTY mates are driven by keystrokes.
func (s *Server) ensurePTY(persona string) (*ptyProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.ptys[persona]; ok {
		return p, nil
	}

	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude not on PATH: %w", err)
	}
	pt, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if err := pt.Resize(120, 30); err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("resize pty: %w", err)
	}

	args := []string{
		"--settings", s.hookSettings(persona),
		"--agent", persona,
		"--name", project.SessionName(persona) + "-pty",
	}
	if cfg, err := project.ResolvePersonaConfig(persona); err == nil {
		args = append(args, cfg.LaunchFlags(false)...)
	}
	// go-pty resolves bare names relative to Dir — absolute path required.
	cmd := pt.Command(claudePath, args...)
	if err := cmd.Start(); err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("spawn claude under pty: %w", err)
	}

	p := &ptyProc{
		persona: persona,
		pt:      pt,
		cmd:     cmd,
		ring:    newRing(ptyRingCap),
		subs:    map[int]chan []byte{},
	}
	s.ptys[persona] = p
	delete(s.exited, persona)
	s.lastSeen[persona] = time.Now()
	s.refs++
	s.lastActivity = time.Now()
	go s.pumpPTY(p)
	return p, nil
}

// pumpPTY reads the mate's screen bytes into the ring and fans them out to
// subscribers. On EOF (process exit) it retires the mate the same way pump
// retires a stream-json mate, so /status.json flips it to done.
func (s *Server) pumpPTY(p *ptyProc) {
	b := make([]byte, 4096)
	for {
		n, err := p.pt.Read(b)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, b[:n])
			p.mu.Lock()
			p.ring.Write(chunk)
			for _, ch := range p.subs {
				select {
				case ch <- chunk:
				default:
					// drop-oldest: evict one frame, then deliver
					select {
					case <-ch:
					default:
					}
					select {
					case ch <- chunk:
					default:
					}
				}
			}
			p.mu.Unlock()
			s.mu.Lock()
			s.lastSeen[p.persona] = time.Now()
			s.lastActivity = time.Now()
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	p.mu.Lock()
	p.closed = true
	for id, ch := range p.subs {
		close(ch)
		delete(p.subs, id)
	}
	p.mu.Unlock()
	_ = p.pt.Close()

	s.mu.Lock()
	delete(s.ptys, p.persona)
	s.exited[p.persona] = true
	s.lastSeen[p.persona] = time.Now()
	if s.refs > 0 {
		s.refs--
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// subscribe registers a viewer: returns the backscroll snapshot, a channel of
// future chunks, and an unsubscribe func. The channel closes when the mate
// exits.
func (p *ptyProc) subscribe() (snapshot []byte, ch chan []byte, cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot = p.ring.Snapshot()
	ch = make(chan []byte, subBufChunks)
	if p.closed {
		close(ch)
		return snapshot, ch, func() {}
	}
	id := p.nextSub
	p.nextSub++
	p.subs[id] = ch
	return snapshot, ch, func() {
		p.mu.Lock()
		if c, ok := p.subs[id]; ok {
			delete(p.subs, id)
			close(c)
		}
		p.mu.Unlock()
	}
}

// --- HTTP surface -----------------------------------------------------------

// handlePTYStart spawns (or finds) the persona's PTY mate.
func (s *Server) handlePTYStart(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	if _, err := s.ensurePTY(persona); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handlePTYSnapshot returns the current backscroll as raw bytes. Debug/attach
// bootstrap; the live stream endpoint lands with the bridge proxy work.
func (s *Server) handlePTYSnapshot(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	s.mu.Lock()
	p := s.ptys[persona]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	p.mu.Lock()
	snap := p.ring.Snapshot()
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(snap)
}

// handlePTYInput writes the request body to the mate's PTY as keystrokes.
func (s *Server) handlePTYInput(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	s.mu.Lock()
	p := s.ptys[persona]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil || len(body) == 0 {
		http.Error(w, "empty input", http.StatusBadRequest)
		return
	}
	if _, err := p.pt.Write(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handlePTYResize resizes the mate's terminal.
func (s *Server) handlePTYResize(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	var body struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Cols <= 0 || body.Rows <= 0 {
		http.Error(w, "want {cols, rows} > 0", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p := s.ptys[persona]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	if err := p.pt.Resize(body.Cols, body.Rows); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handlePTYStream streams the mate's screen over SSE: one base64 data event
// per chunk, prefixed by a snapshot event carrying the backscroll. SSE rides
// plain HTTP, so the existing bridge tunnel proxies it without a websocket
// dependency; the browser feeds decoded bytes straight into xterm.js.
func (s *Server) handlePTYStream(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	s.mu.Lock()
	p := s.ptys[persona]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no pty mate", http.StatusNotFound)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	snapshot, ch, cancel := p.subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	writeChunk := func(event string, b []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, base64Std(b)); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if len(snapshot) > 0 {
		if !writeChunk("snapshot", snapshot) {
			return
		}
	}
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				_, _ = fmt.Fprint(w, "event: exit\ndata: \n\n")
				fl.Flush()
				return
			}
			if !writeChunk("data", chunk) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) closePTYs() {
	s.mu.Lock()
	procs := make([]*ptyProc, 0, len(s.ptys))
	for _, p := range s.ptys {
		procs = append(procs, p)
	}
	s.mu.Unlock()
	for _, p := range procs {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.pt.Close()
	}
}
