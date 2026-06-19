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

// Event is one line in the activity feed.
type Event struct {
	Time    string `json:"time"`
	Persona string `json:"persona"`
	Type    string `json:"type"`
	Text    string `json:"text"`
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
	ch      chan string // receives "allow" or "deny"
}

// Server holds feed state, live crew processes, and pending permission requests.
type Server struct {
	port     int
	mu       sync.Mutex
	events   []Event
	live     map[string]*liveProc
	pendings map[string]*pending
	stopOnce sync.Once
	stopCh   chan struct{}
}

// New constructs an empty server.
func New() *Server {
	return &Server{
		live:     map[string]*liveProc{},
		pendings: map[string]*pending{},
		stopCh:   make(chan struct{}),
	}
}

func (s *Server) addEvent(e Event) {
	e.Time = time.Now().Format(time.RFC3339)
	s.mu.Lock()
	s.events = append(s.events, e)
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
	mux.HandleFunc("POST /events", s.handleEvents)
	mux.HandleFunc("POST /tell/{persona}", s.handleTell)
	mux.HandleFunc("POST /hook/{persona}/{event}", s.handleHook)
	mux.HandleFunc("GET /pending", s.handlePending)
	mux.HandleFunc("POST /resolve/{id}", s.handleResolve)
	mux.HandleFunc("GET /feed", s.handleFeed)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /deregister", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	httpSrv := &http.Server{Handler: mux}
	go func() {
		<-s.stopCh
		s.closeLive()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()

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
	s.addEvent(Event{Persona: persona, Type: "hook:" + event, Text: text})
	w.Header().Set("Content-Type", "application/json")

	// In headless -p mode there is no separate PermissionRequest event: the
	// PreToolUse hook *is* the gate. It returns a permissionDecision of
	// allow/deny. For risky tools we block on a human decision; everything else
	// is auto-allowed. (An empty 200 would be treated as allow — never do that
	// for a gated tool.)
	if event == "PreToolUse" {
		decision := "allow"
		if gatedTool(text) {
			decision = s.awaitDecision(persona, text)
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

// gatedTool reports whether a tool requires human approval. For now: shell
// execution. (Future: drive this from per-persona permission policy.)
func gatedTool(tool string) bool {
	switch tool {
	case "Bash", "PowerShell":
		return true
	default:
		return false
	}
}

// awaitDecision registers a pending request, blocks until a human resolves it
// (allow/deny) or it times out (deny), and returns the decision.
func (s *Server) awaitDecision(persona, tool string) string {
	id := project.NewUUID()[:8]
	p := &pending{id: id, persona: persona, tool: tool, ch: make(chan string, 1)}

	s.mu.Lock()
	s.pendings[id] = p
	s.mu.Unlock()
	s.addEvent(Event{Persona: persona, Type: "permission?", Text: fmt.Sprintf("%s wants %s — approve: shipmates allow %s (or deny %s)", persona, tool, id, id)})

	var decision string
	select {
	case decision = <-p.ch:
	case <-time.After(110 * time.Second):
		decision = "deny"
	}

	s.mu.Lock()
	delete(s.pendings, id)
	s.mu.Unlock()
	s.addEvent(Event{Persona: persona, Type: "permission:" + decision, Text: tool})
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
func (s *Server) hookSettings(persona string) string {
	url := func(event string) string {
		return fmt.Sprintf("http://127.0.0.1:%d/hook/%s/%s", s.port, persona, event)
	}
	// PreToolUse is the gate (it may block on a human decision), so give it a
	// generous timeout. PostToolUse is observe-only.
	preTool := map[string]any{
		"hooks": []map[string]any{{"type": "http", "url": url("PreToolUse"), "timeout": 120}},
	}
	postTool := map[string]any{
		"hooks": []map[string]any{{"type": "http", "url": url("PostToolUse")}},
	}
	cfg := map[string]any{
		"hooks": map[string]any{
			"PreToolUse":  []map[string]any{preTool},
			"PostToolUse": []map[string]any{postTool},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// ensureLive returns the persona's live process, spawning one if needed.
func (s *Server) ensureLive(persona string) (*liveProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lp, ok := s.live[persona]; ok {
		return lp, nil
	}

	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // required by claude when --print is combined with stream-json output
		"--include-partial-messages",
		"--settings", s.hookSettings(persona),
		"--agent", persona,
		"--name", project.SessionName(persona) + "-live",
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

	lp := &liveProc{persona: persona, cmd: cmd, stdin: stdin}
	s.live[persona] = lp
	go s.pump(persona, stdout)
	slog.Info("spawned live crew process", "persona", persona, "pid", cmd.Process.Pid)
	return lp, nil
}

// pump reads a crew process's stream-json output and tees text into the feed.
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
