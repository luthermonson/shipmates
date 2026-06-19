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

// Server holds feed state and live crew processes.
type Server struct {
	port     int
	mu       sync.Mutex
	events   []Event
	live     map[string]*liveProc
	stopOnce sync.Once
	stopCh   chan struct{}
}

// New constructs an empty server.
func New() *Server {
	return &Server{live: map[string]*liveProc{}, stopCh: make(chan struct{})}
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

// handleHook receives a Claude Code hook callback (e.g. PreToolUse) and records
// it in the feed. Returning 200 with an empty object means "proceed" — we
// observe, we don't block.
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
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// hookSettings builds a --settings JSON string that routes a crew member's
// tool-use hooks back to this server, tagged with the persona.
func (s *Server) hookSettings(persona string) string {
	httpHook := func(event string) map[string]any {
		return map[string]any{
			"hooks": []map[string]any{
				{"type": "http", "url": fmt.Sprintf("http://127.0.0.1:%d/hook/%s/%s", s.port, persona, event)},
			},
		}
	}
	cfg := map[string]any{
		"hooks": map[string]any{
			"PreToolUse":  []map[string]any{httpHook("PreToolUse")},
			"PostToolUse": []map[string]any{httpHook("PostToolUse")},
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
