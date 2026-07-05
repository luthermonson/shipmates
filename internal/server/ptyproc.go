package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

	// modes latches DEC private mode state (CSI ? Pm h/l) seen in the output
	// stream — alt-screen 1049, alternate-scroll 1007, bracketed paste 2004,
	// mouse tracking, cursor visibility, etc. The TUI sets these once at
	// startup; once that scrolls out of the ring, a late attacher's snapshot
	// alone leaves its terminal in default mode (wheel dead, paste wrong).
	// subscribe() replays the latched state ahead of the snapshot, the same
	// trick tmux uses on attach.
	modes     map[int]bool
	scanCarry []byte // tail of the previous chunk, so split sequences still match
}

// ptyRingCap is the per-mate backscroll: enough for a screenful of heavy TUI
// redraw plus recent history, small enough to hold dozens of mates in memory.
const ptyRingCap = 64 * 1024

// subBufChunks bounds each subscriber's channel. A slow consumer drops oldest
// frames rather than backpressuring the PTY pump — never stall the agent for
// a slow browser tab.
const subBufChunks = 256

// ensurePTY returns the persona's PTY-hosted mate, spawning one if needed.
// The PTY resumes the shipmate's ONE long-term session (the same one
// ask/tell use). Single attachment per shipmate: an existing headless live
// proc is retired first — the terminal takes over the conversation, with
// full history rendered by claude's own session resume.
func (s *Server) ensurePTY(persona string) (*ptyProc, error) {
	s.mu.Lock()
	if p, ok := s.ptys[persona]; ok {
		s.mu.Unlock()
		return p, nil
	}
	if lp, ok := s.live[persona]; ok {
		// retire the headless attachment; its pump cleans up asynchronously
		_ = lp.stdin.Close()
		if lp.cmd.Process != nil {
			_ = lp.cmd.Process.Kill()
		}
	}
	s.mu.Unlock()
	// wait for the pump to release the session before resuming it here
	for i := 0; i < 40; i++ {
		s.mu.Lock()
		_, still := s.live[persona]
		s.mu.Unlock()
		if !still {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.ptys[persona]; ok { // raced with another attach
		return p, nil
	}

	pt, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	if err := pt.Resize(120, 30); err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("resize pty: %w", err)
	}

	// The backend driver seam: claude mates get the full integration
	// (session resume, observe hooks, beads prime); command-backed mates
	// (opencode, aider, ...) just get their argv under a PTY — their status
	// derives from screen activity, which pumpPTY already records.
	var cmd *pty.Cmd
	var sessID, sessName, fp string
	pcfg, _ := project.ResolvePersonaConfig(persona)
	if pcfg.CommandBacked() {
		if len(pcfg.Command) == 0 {
			_ = pt.Close()
			return nil, fmt.Errorf("persona %s has backend: command but no command", persona)
		}
		// go-pty resolves bare names relative to Dir — absolute path required.
		prog, err := exec.LookPath(pcfg.Command[0])
		if err != nil {
			_ = pt.Close()
			return nil, fmt.Errorf("%s not on PATH: %w", pcfg.Command[0], err)
		}
		cmd = pt.Command(prog, pcfg.Command[1:]...)
	} else {
		claudePath, err := exec.LookPath("claude")
		if err != nil {
			_ = pt.Close()
			return nil, fmt.Errorf("claude not on PATH: %w", err)
		}
		var cfg project.PersonaConfig
		var idArgs []string
		cfg, idArgs, sessID, sessName, fp = project.SessionLaunch(persona, false)
		args := []string{
			// observe-only hooks: interactive claude prompts for permissions
			// natively in the terminal — the operator approves right where they
			// are typing instead of hunting for the bridge's pending pane behind
			// the full-screen term
			"--settings", s.hookSettings(persona, false),
		}
		args = append(args, idArgs...)
		args = append(args, cfg.LaunchFlags(false)...)
		// beads: same prime injection as live mates, for consistent context
		if prime := beadsPrime(); prime != "" {
			args = append(args, "--append-system-prompt", prime)
		}
		cmd = pt.Command(claudePath, args...)
	}
	if err := cmd.Start(); err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("spawn mate under pty: %w", err)
	}

	p := &ptyProc{
		persona: persona,
		pt:      pt,
		cmd:     cmd,
		ring:    newRing(ptyRingCap),
		subs:    map[int]chan []byte{},
		modes:   map[int]bool{},
	}
	s.ptys[persona] = p
	delete(s.exited, persona)
	s.lastSeen[persona] = time.Now()
	s.refs++
	s.lastActivity = time.Now()
	if sessID != "" {
		_ = project.WriteSessionMeta(persona, sessName, sessID, fp)
	}
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
			p.trackModes(chunk)
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

// modeSeq matches DEC private mode set/reset: ESC [ ? Pm h|l (Pm may be a
// semicolon list). Compiled once; used under the ptyProc mutex.
var modeSeq = regexp.MustCompile(`\x1b\[\?([0-9;]+)([hl])`)

// trackModes latches the most recent h/l state of every DEC private mode seen
// in the output stream. Caller holds p.mu. A small carry stitches sequences
// split across read-chunk boundaries.
func (p *ptyProc) trackModes(chunk []byte) {
	buf := chunk
	if len(p.scanCarry) > 0 {
		buf = append(append([]byte{}, p.scanCarry...), chunk...)
	}
	for _, m := range modeSeq.FindAllSubmatch(buf, -1) {
		set := m[2][0] == 'h'
		for _, num := range strings.Split(string(m[1]), ";") {
			if n, err := strconv.Atoi(num); err == nil {
				p.modes[n] = set
			}
		}
	}
	// keep a tail shorter than the smallest full sequence so a split one
	// re-matches on the next chunk without double-counting completed ones
	const carry = 15
	if len(buf) > carry {
		buf = buf[len(buf)-carry:]
	}
	p.scanCarry = append(p.scanCarry[:0], buf...)
}

// modePrefix renders the latched mode state as a replayable byte sequence.
// Caller holds p.mu.
func (p *ptyProc) modePrefix() []byte {
	if len(p.modes) == 0 {
		return nil
	}
	nums := make([]int, 0, len(p.modes))
	for n := range p.modes {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var b []byte
	for _, n := range nums {
		hl := byte('l')
		if p.modes[n] {
			hl = 'h'
		}
		b = append(b, fmt.Sprintf("\x1b[?%d%c", n, hl)...)
	}
	return b
}

// subscribe registers a viewer: returns the backscroll snapshot (prefixed with
// the latched terminal-mode state), a channel of future chunks, and an
// unsubscribe func. The channel closes when the mate exits.
func (p *ptyProc) subscribe() (snapshot []byte, ch chan []byte, cancel func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	snapshot = append(p.modePrefix(), p.ring.Snapshot()...)
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
