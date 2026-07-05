// Package server implements the transient, lead-spawned coordination server.
// It brokers messages in both directions: crew -> server (hooks post activity
// and permission requests) and lead -> crew (`shipmates tell` injects messages
// into a live crew process's stdin over the stream-json channel).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
)

// Event is one line in the activity feed. Tool, Input, and ID are populated
// only for permission events so the UI can render allow/deny actions without
// parsing the free-form Text.
type Event struct {
	Time    string `json:"time"`
	Persona string `json:"persona"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Tool    string `json:"tool,omitempty"`
	Input   string `json:"input,omitempty"` // e.g. the Bash command, the path being written
	ID      string `json:"id,omitempty"`
}

// liveProc is a persistent crew process the server can talk to mid-work.
type liveProc struct {
	persona string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
}

// pending is a crew permission request awaiting an allow/deny decision.
type pending struct {
	id      string
	persona string
	tool    string
	input   string // human-friendly summary of tool_input (e.g. the Bash command)
	ch      chan string // receives "allow" or "deny"
}

// Server holds feed state, live crew processes, and pending permission requests.
type Server struct {
	port     int
	mu       sync.Mutex
	events   []Event
	live     map[string]*liveProc
	ptys     map[string]*ptyProc
	pendings map[string]*pending
	lastSeen map[string]time.Time // persona -> last event time (for /status.json)
	exited   map[string]bool      // personas whose live process ended (for /status.json)
	stopOnce sync.Once
	stopCh   chan struct{}

	refs         int           // active /register count
	lastActivity time.Time     // last register/deregister/event (guarded by s.mu)
	idleBound    time.Duration // chosen in Run, based on whether a bridge is configured
	bridged      bool          // wired to a central bridge (changes idle behavior)
}

// idleTimeoutEphemeral is the lifecycle bound for a server spawned by a
// one-shot tell with no persistent parent: no activity for this long and it
// self-terminates. Kept short so abandoned scratch processes don't linger.
const idleTimeoutEphemeral = 5 * time.Minute

// idleTimeoutBridged is the longer bound used when the lead is wired to a
// central bridge. A bridged lead does NOT exit on idle — exiting severs the
// tunnel and leaves nothing on the ship for the bridge to wake. Instead the
// idle bound reaps the crew processes (the actual resource cost: idle claude
// processes) while the lead itself — a tiny HTTP server plus a websocket —
// stays connected. Any tell or PTY start through the bridge spawns fresh
// crew: the ship never sleeps, only the mates do.
const idleTimeoutBridged = 1 * time.Hour

// New constructs an empty server.
func New() *Server {
	return &Server{
		live:     map[string]*liveProc{},
		ptys:     map[string]*ptyProc{},
		pendings: map[string]*pending{},
		lastSeen: map[string]time.Time{},
		exited:   map[string]bool{},
		stopCh:   make(chan struct{}),
	}
}

func (s *Server) addEvent(e Event) {
	e.Time = time.Now().Format(time.RFC3339)
	s.mu.Lock()
	s.events = append(s.events, e)
	s.lastActivity = time.Now()
	if e.Persona != "" {
		s.lastSeen[e.Persona] = time.Now()
	}
	s.mu.Unlock()
	slog.Debug("event", "persona", e.Persona, "type", e.Type)
}

// Run binds to a random localhost port, records port/pid, and serves until a
// /shutdown request closes the stop channel.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s.port = port

	s.mu.Lock()
	s.lastActivity = time.Now() // seed so the idle watcher never fires before activity
	s.mu.Unlock()

	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(project.PortFile(), []byte(strconv.Itoa(port)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(project.PidFile(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(project.PortFile())
	defer os.Remove(project.PidFile())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /events", s.handleEventsJSON)
	mux.HandleFunc("POST /events", s.handleEvents)
	mux.HandleFunc("POST /tell/{persona}", s.handleTell)
	mux.HandleFunc("POST /hook/{persona}/{event}", s.handleHook)
	mux.HandleFunc("GET /pending", s.handlePending)
	mux.HandleFunc("GET /pending.json", s.handlePendingJSON)
	mux.HandleFunc("GET /status.json", s.handleStatusJSON)
	mux.HandleFunc("GET /beads.json", s.handleBeadsJSON)
	mux.HandleFunc("GET /bead/{id}", s.handleBeadShow)
	mux.HandleFunc("POST /pty/{persona}/start", s.handlePTYStart)
	mux.HandleFunc("GET /pty/{persona}/stream", s.handlePTYStream)
	mux.HandleFunc("GET /pty/{persona}/snapshot", s.handlePTYSnapshot)
	mux.HandleFunc("POST /pty/{persona}/input", s.handlePTYInput)
	mux.HandleFunc("POST /pty/{persona}/resize", s.handlePTYResize)
	mux.HandleFunc("POST /resolve/{id}", s.handleResolve)
	mux.HandleFunc("GET /feed", s.handleFeed)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.refs++
		s.lastActivity = time.Now()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /deregister", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.refs > 0 {
			s.refs--
		}
		s.lastActivity = time.Now()
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	httpSrv := &http.Server{Handler: mux}
	go func() {
		<-s.stopCh
		s.closeLive()
		s.closePTYs()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()

	// Idle-timeout auto-shutdown: once nothing has happened for idleTimeout,
	// trigger the same graceful shutdown as POST /shutdown.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.mu.Lock()
				idle := time.Since(s.lastActivity)
				bound := s.idleBound
				bridged := s.bridged
				s.mu.Unlock()
				if idle > bound {
					if bridged {
						// reap idle crew but keep the ship reachable — the
						// bridge can wake it with a tell/pty-start any time
						slog.Info("idle timeout: reaping crew, staying connected", "idle", idle)
						s.closeLive()
						s.closePTYs()
						s.mu.Lock()
						s.lastActivity = time.Now()
						s.mu.Unlock()
						continue
					}
					slog.Info("idle timeout reached, shutting down", "idle", idle, "bound", bound)
					s.stopOnce.Do(func() { close(s.stopCh) })
					return
				}
			}
		}
	}()

	// Open the outbound bridge connection if one is configured. No-op when not.
	// The bridge can then dial back through the tunnel to reach this server's
	// localhost endpoints (which the bridge proxies under its /api/* surface).
	idleBound := idleTimeoutEphemeral
	if conf, err := project.LoadConfig(); err == nil {
		s.startBridge(ctx, conf)
		if conf != nil && strings.TrimSpace(conf.Bridge.URL) != "" {
			idleBound = idleTimeoutBridged
			s.mu.Lock()
			s.bridged = true
			s.mu.Unlock()
		}
	}
	s.idleBound = idleBound

	go s.beadsSyncLoop(ctx)

	slog.Info("shipmates server listening", "port", port, "pid", os.Getpid())
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var e Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	s.addEvent(e)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		fmt.Fprintf(w, "[%s] %s/%s: %s\n", e.Time, e.Persona, e.Type, e.Text)
	}
}

// handleEventsJSON returns the full event slice as JSON. Used by the bridge to
// poll for new events to mirror into its SQLite store; also handy for any
// programmatic consumer that wants structured data instead of pre-formatted
// text lines (which is what handleFeed gives you).
func (s *Server) handleEventsJSON(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *Server) handleTell(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Message == "" {
		http.Error(w, "missing message", http.StatusBadRequest)
		return
	}

	// Single attachment per shipmate: when a terminal holds the persona's
	// session, the tell is typed INTO the terminal (bracketed paste + enter)
	// — same conversation, visible where the operator is looking — instead
	// of spawning a second process against the same session.
	s.mu.Lock()
	pp := s.ptys[persona]
	s.mu.Unlock()
	if pp != nil {
		s.addEvent(Event{Persona: persona, Type: "tell", Text: body.Message})
		keys := "\x1b[200~" + body.Message + "\x1b[201~\r"
		if _, err := pp.pt.Write([]byte(keys)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	lp, err := s.ensureLive(persona)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": body.Message}},
		},
	}
	line, _ := json.Marshal(msg)
	s.addEvent(Event{Persona: persona, Type: "tell", Text: body.Message})
	if _, err := lp.stdin.Write(append(line, '\n')); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleHook receives a Claude Code hook callback. Observe events (PreToolUse,
// PostToolUse) are recorded and acknowledged immediately; PermissionRequest is
// a blocking decision and is delegated to handlePermission.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	persona := r.PathValue("persona")
	event := r.PathValue("event")
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)

	text, _ := payload["tool_name"].(string)
	if text == "" {
		text = "(no tool_name)"
	}
	input, _ := payload["tool_input"].(map[string]any)
	inputSummary := summarizeToolInput(text, input)
	s.addEvent(Event{Persona: persona, Type: "hook:" + event, Text: text, Tool: text, Input: inputSummary})
	w.Header().Set("Content-Type", "application/json")

	// In headless -p mode there is no separate PermissionRequest event: the
	// PreToolUse hook *is* the gate. It returns a permissionDecision of
	// allow/deny. For risky tools we block on a human decision; everything else
	// is auto-allowed. (An empty 200 would be treated as allow — never do that
	// for a gated tool.)
	if event == "PreToolUse" {
		decision := "allow"
		if gatedTool(text) && !personaPermissive(persona) {
			decision = s.awaitDecision(persona, text, inputSummary)
		}
		out := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":      "PreToolUse",
				"permissionDecision": decision,
			},
		}
		if decision == "deny" {
			out["hookSpecificOutput"].(map[string]any)["permissionDecisionReason"] = "denied via shipmates lead/captain"
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// gatedTool reports whether a tool category requires human approval — shell
// execution. Whether gating actually applies also depends on the persona's
// permission mode (see personaPermissive).
func gatedTool(tool string) bool {
	switch tool {
	case "Bash", "PowerShell":
		return true
	default:
		return false
	}
}

// personaPermissive reports whether the persona's effective permission mode
// skips human gating entirely. On resolver error we fail closed (treat as not
// permissive) so gating defaults to the prior behavior.
func personaPermissive(persona string) bool {
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return false
	}
	return cfg.DangerouslySkipPermissions || cfg.Mode == "bypassPermissions"
}

// awaitDecision registers a pending request, blocks until a human resolves it
// (allow/deny) or it times out (deny), and returns the decision. input is a
// human-readable summary of the tool call (e.g. the Bash command) shown in
// the UI so the operator can judge before approving.
func (s *Server) awaitDecision(persona, tool, input string) string {
	id := project.NewUUID()[:8]
	p := &pending{id: id, persona: persona, tool: tool, input: input, ch: make(chan string, 1)}

	s.mu.Lock()
	s.pendings[id] = p
	s.mu.Unlock()
	text := fmt.Sprintf("%s wants %s", persona, tool)
	if input != "" {
		text = fmt.Sprintf("%s wants %s: %s", persona, tool, input)
	}
	s.addEvent(Event{
		Persona: persona,
		Type:    "permission?",
		Text:    text,
		Tool:    tool,
		Input:   input,
		ID:      id,
	})

	var decision string
	select {
	case decision = <-p.ch:
	case <-time.After(110 * time.Second):
		decision = "deny"
	}

	s.mu.Lock()
	delete(s.pendings, id)
	s.mu.Unlock()
	s.addEvent(Event{Persona: persona, Type: "permission:" + decision, Text: tool, Tool: tool, ID: id})
	return decision
}

// handlePending lists permission requests currently awaiting a decision.
func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendings) == 0 {
		fmt.Fprintln(w, "(none)")
		return
	}
	for id, p := range s.pendings {
		fmt.Fprintf(w, "%s  %s wants %s\n", id, p.persona, p.tool)
	}
}

// handlePendingJSON is the structured form of /pending: an array the bridge
// can ingest without parsing text. Used by the bridge's /api/pending aggregator
// to populate the UI's permission pane.
func (s *Server) handlePendingJSON(w http.ResponseWriter, r *http.Request) {
	type wire struct {
		ID      string `json:"id"`
		Persona string `json:"persona"`
		Tool    string `json:"tool"`
		Input   string `json:"input,omitempty"`
	}
	s.mu.Lock()
	out := make([]wire, 0, len(s.pendings))
	for id, p := range s.pendings {
		out = append(out, wire{ID: id, Persona: p.persona, Tool: p.tool, Input: p.input})
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// summarizeToolInput renders a tool's input map as a single-line, human-readable
// string for the permission UI. Most gated tools (Bash, PowerShell) have a
// canonical "command" field; file tools have a "file_path". Fallback is a
// compact JSON dump so the operator at least sees what's being requested.
func summarizeToolInput(tool string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch tool {
	case "Bash", "PowerShell":
		if cmd, ok := input["command"].(string); ok {
			return cmd
		}
	case "Read", "Write", "Edit":
		if p, ok := input["file_path"].(string); ok {
			return p
		}
	}
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleResolve delivers an allow/deny decision to a waiting permission request.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Behavior string `json:"behavior"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if body.Behavior != "allow" && body.Behavior != "deny" {
		http.Error(w, "behavior must be allow|deny", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	p := s.pendings[id]
	s.mu.Unlock()
	if p == nil {
		http.Error(w, "no such pending request", http.StatusNotFound)
		return
	}
	p.ch <- body.Behavior
	w.WriteHeader(http.StatusAccepted)
}

// hookSettings builds a --settings JSON string that routes a crew member's
// tool-use hooks back to this server, tagged with the persona.
//
// gate=true installs the blocking PreToolUse permission hook — right for
// headless mates, where nobody sees a prompt and the bridge's pending pane is
// the only approval surface. gate=false is observe-only (PostToolUse): right
// for PTY mates, where interactive claude renders its own y/n permission
// prompt in the terminal the operator is already looking at.
func (s *Server) hookSettings(persona string, gate bool) string {
	url := func(event string) string {
		return fmt.Sprintf("http://127.0.0.1:%d/hook/%s/%s", s.port, persona, event)
	}
	hooks := map[string]any{
		"PostToolUse": []map[string]any{{
			"hooks": []map[string]any{{"type": "http", "url": url("PostToolUse")}},
		}},
	}
	if gate {
		// PreToolUse is the gate (it may block on a human decision), so give
		// it a generous timeout.
		hooks["PreToolUse"] = []map[string]any{{
			"hooks": []map[string]any{{"type": "http", "url": url("PreToolUse"), "timeout": 120}},
		}}
	}
	b, _ := json.Marshal(map[string]any{"hooks": hooks})
	return string(b)
}

// ensureLive returns the persona's live process, spawning one if needed.
func (s *Server) ensureLive(persona string) (*liveProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lp, ok := s.live[persona]; ok {
		return lp, nil
	}

	// Command-backed mates (backend: command) have no stream-json channel to
	// inject a tell into — they only exist under a PTY.
	if pcfg, _ := project.ResolvePersonaConfig(persona); pcfg.CommandBacked() {
		return nil, fmt.Errorf("persona %s is PTY-only (backend: command) — open a terminal to talk to it", persona)
	}

	// One long-term session per shipmate: resume the same tracked session
	// that ask/open/fanout use, so tell/term/ask are one continuous
	// conversation instead of per-surface forks.
	cfg, idArgs, sessID, sessName, fp := project.SessionLaunch(persona, false)
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // required by claude when --print is combined with stream-json output
		"--include-partial-messages",
		"--settings", s.hookSettings(persona, true),
	}
	args = append(args, idArgs...)
	// Live-server path mediates permission via the PreToolUse gate, so it
	// passes model/effort only (permission=false).
	args = append(args, cfg.LaunchFlags(false)...)
	// beads: -p sessions never fire SessionStart hooks, so bd's auto-prime
	// can't run — inject the workflow context at spawn instead.
	if prime := beadsPrime(); prime != "" {
		args = append(args, "--append-system-prompt", prime)
	}
	cmd := exec.Command("claude", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn claude: %w", err)
	}

	// Server-driven ref-count: crew run in `claude -p` mode never fire the
	// SessionStart hook, so /register is never called. Count the spawn itself.
	// (s.mu is held by the caller, ensureLive.)
	s.refs++
	s.lastActivity = time.Now()

	lp := &liveProc{persona: persona, cmd: cmd, stdin: stdin}
	s.live[persona] = lp
	delete(s.exited, persona) // resurrect: a re-spawned mate is no longer "done"
	s.lastSeen[persona] = time.Now()
	_ = project.WriteSessionMeta(persona, sessName, sessID, fp)
	go s.pump(persona, stdout)
	slog.Info("spawned live crew process", "persona", persona, "pid", cmd.Process.Pid, "session", sessID)
	return lp, nil
}

// pump reads a crew process's stream-json output and tees text into the feed.
// When the read loop ends (process exited / stdout EOF) it decrements the
// server-driven ref-count, the counterpart of the increment in ensureLive.
func (s *Server) pump(persona string, stdout io.Reader) {
	dec := json.NewDecoder(stdout)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			break
		}
		switch obj["type"] {
		case "assistant":
			if t := assistantText(obj); t != "" {
				s.addEvent(Event{Persona: persona, Type: "assistant", Text: t})
			}
		case "result":
			s.addEvent(Event{Persona: persona, Type: "result", Text: "(turn complete)"})
		}
	}

	s.mu.Lock()
	if s.refs > 0 {
		s.refs--
	}
	refs := s.refs
	s.lastActivity = time.Now()
	delete(s.live, persona)
	s.exited[persona] = true
	s.lastSeen[persona] = time.Now()
	s.mu.Unlock()
	slog.Info("live crew process ended", "persona", persona, "refs", refs)
}

func assistantText(obj map[string]any) string {
	m, ok := obj["message"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := m["content"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "text" {
			if t, ok := cm["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func (s *Server) closeLive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lp := range s.live {
		_ = lp.stdin.Close()
		if lp.cmd.Process != nil {
			_ = lp.cmd.Process.Kill()
		}
	}
}
