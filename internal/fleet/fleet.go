// Package fleet is the central rendezvous server a power user can run when
// they want a single pane across many shipmates captains. Captains dial out
// to the fleet over a websocket (rancher/remotedialer); the fleet then
// proxies operator-facing /api/* calls back through the tunnel to each
// captain's local 127.0.0.1 server. It also hosts the embedded Fleet Command
// browser UI, which consumes the same JSON API as the shipmates CLI.
package fleet

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rancher/remotedialer"

	"github.com/luthermonson/shipmates/internal/api"
)

//go:embed all:ui
var uiFS embed.FS

// Server is the fleet: it embeds a remotedialer.Server (which accepts inbound
// websocket connections from captains) and adds a small /api/* surface that
// the shipmates CLI hits to list captains, tail their event feeds, and proxy
// commands back through the tunnels.
type Server struct {
	dialer *remotedialer.Server
	token  string

	mu       sync.Mutex
	captains map[string]*Captain // keyed by clientKey

	// dispatchQ holds bead dispatches whose target ship was offline at
	// assign time; a sweep loop delivers them when the ship reconnects.
	// In-memory only — a fleet restart drops the queue (the assignee is
	// already on the graph, so the work isn't lost, just un-nudged).
	dispatchQ []queuedDispatch

	conv   *convConfig  // nil unless voice/conversation flags are set
	policy *policyState // fleet-wide deny list source; served via /api/fleet-policy

	// attachRelay and attachTell are test hooks: production leaves them nil
	// and the attach handler falls through to b.proxyRaw / b.proxy over the
	// real tunnel. Setting them lets tests stub the ship without spinning
	// up remotedialer.
	attachRelay func(ctx context.Context, clientKey, method, path, contentType string, body []byte) ([]byte, int, error)
	attachTell  func(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error)
}

// convConfig holds the runtime config for the voice surface: /api/conversation
// (Ollama tool-loop), /api/tts (Edge neural voices), and /api/stt (an
// OpenAI-compatible or whisper.cpp transcription server). The http client is
// reused so connections stay pooled.
type convConfig struct {
	url      string       // OpenAI-compatible base URL (…/v1); "" disables /api/conversation
	model    string       // model tag/name sent in requests
	key      string       // bearer key for hosted OAI-compatible endpoints; "" = no auth (local)
	brain    *claudeBrain // non-nil when --llm-backend=claude-cli
	voice    string       // voice tag: Edge (en-US-AriaNeural) or the OAI server's voice name
	ttsURL   string       // OpenAI-compatible /v1/audio/speech endpoint; "" = Edge websocket
	ttsModel string       // model field for OAI-style TTS servers
	sttURL   string       // transcription endpoint; "" disables /api/stt
	sttModel string       // model field sent to the transcription server (OAI-style)
	client   *http.Client
}

type Captain = api.Captain

// Options configures the fleet.
type Options struct {
	Addr       string // listen address (e.g. ":8443")
	Token      string // shared secret; if empty, auth is disabled (dev only)
	LLMBackend string // "openai" (default: chat-completions HTTP) or "claude-cli" (spawn claude -p on this host)
	LLMURL     string // OpenAI-compatible base URL incl. /v1 (ollama, llama.cpp, LM Studio, OpenAI…); enables /api/conversation
	LLMModel   string // model name for the conversation loop, e.g. "qwen2.5:7b" or "haiku"
	LLMKey     string // bearer key for hosted endpoints (read from env by the CLI layer); "" = no auth
	TTSVoice   string // voice tag (Edge: en-US-AriaNeural; OAI servers: e.g. af_heart); empty + no TTSURL disables /api/tts
	TTSURL     string // optional OpenAI-compatible /v1/audio/speech endpoint (kokoro-fastapi etc.); overrides Edge
	TTSModel   string // model field for OAI-style TTS servers
	STTURL     string // optional transcription endpoint (whisper.cpp /inference or OAI /v1/audio/transcriptions); empty disables /api/stt
	STTModel   string // model name forwarded to OAI-style STT servers; whisper.cpp ignores it
	// PolicyPath overrides the on-disk fleet-policy YAML location. Empty =
	// use the SHIPMATES_FLEET_POLICY env var or ~/.shipmates/fleet-policy.yaml.
	PolicyPath string
}

// New constructs the fleet. The returned Server is ready to Run.
func New(opts Options) (*Server, error) {
	b := &Server{
		token:    strings.TrimSpace(opts.Token),
		captains: map[string]*Captain{},
	}
	if opts.LLMURL != "" || opts.LLMBackend == "claude-cli" || opts.TTSVoice != "" || opts.TTSURL != "" || opts.STTURL != "" {
		b.conv = &convConfig{
			url:      strings.TrimRight(opts.LLMURL, "/"),
			model:    opts.LLMModel,
			key:      strings.TrimSpace(opts.LLMKey),
			voice:    strings.TrimSpace(opts.TTSVoice),
			ttsURL:   strings.TrimSpace(opts.TTSURL),
			ttsModel: strings.TrimSpace(opts.TTSModel),
			sttURL:   strings.TrimSpace(opts.STTURL),
			sttModel: strings.TrimSpace(opts.STTModel),
			// Long timeout because a tool-call loop with multiple LLM round
			// trips, each waiting on a captain's tunnelled response, can take a
			// while. Voice timeouts are enforced by the UI client, not here.
			client: &http.Client{Timeout: 5 * time.Minute},
		}
		if opts.LLMBackend == "claude-cli" {
			b.conv.brain = newClaudeBrain(opts.LLMModel, opts.Addr, b.token)
		}
	}
	b.policy = newPolicyState(opts.PolicyPath)
	// Prime once so a malformed file surfaces in the fleet's own logs at
	// startup rather than only on the first ship poll.
	if _, err := b.policy.load(); err != nil {
		slog.Warn("initial fleet-policy load failed (endpoint will retry per request)", "err", err)
	}
	b.dialer = remotedialer.New(b.authorize, remotedialer.DefaultErrorWriter)
	return b, nil
}

// Handler returns Fleet Command's complete authenticated HTTP surface. Keeping
// route construction separate from listener ownership lets production Run and
// autonomous integration tests exercise the exact same mux.
func (b *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/connect", b.dialer)
	mux.HandleFunc("GET /login", b.handleLogin)
	mux.HandleFunc("POST /login", b.handleLogin)
	mux.HandleFunc("POST /logout", b.handleLogout)
	mux.HandleFunc("GET /api/captains", b.handleCaptains)
	mux.HandleFunc("GET /api/fleet-policy", b.handleFleetPolicy)
	mux.HandleFunc("GET /api/pending", b.handleAggregatePending)
	mux.HandleFunc("GET /api/captain/{key}/feed", b.proxyGet("/feed"))
	mux.HandleFunc("GET /api/captain/{key}/pending", b.proxyGet("/pending"))
	mux.HandleFunc("GET /api/captain/{key}/beads", b.proxyGet("/beads.json"))
	mux.HandleFunc("GET /api/captain/{key}/beads/summary", b.proxyGet("/beads/summary"))
	mux.HandleFunc("GET /api/status", b.handleAggregateStatus)
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/start", b.proxyPTYPost("/pty/%s/start"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/input", b.proxyPTYPost("/pty/%s/input"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/resize", b.proxyPTYPost("/pty/%s/resize"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/takeover", b.proxyPTYPost("/pty/%s/takeover"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/release", b.proxyPTYPost("/pty/%s/release"))
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/snapshot", b.proxyGet2("/pty/%s/snapshot", "persona"))
	mux.HandleFunc("GET /api/captain/{key}/bead/{id}", b.proxyGet2("/bead/%s", "id"))
	mux.HandleFunc("POST /api/captain/{key}/bead", b.proxyPost("/bead"))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/close", b.proxyPost2("/bead/%s/close", "id"))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/update", b.proxyPost2("/bead/%s/update", "id"))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/assign", b.handleBeadAssign)
	mux.HandleFunc("GET /api/beads", b.handleAggregateBeads)
	mux.HandleFunc("POST /api/beads/nudge", b.handleBeadsNudge)
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	mux.HandleFunc("GET /api/captain/{key}/stream", b.handleStream)
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", b.handleTell)
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	mux.HandleFunc("POST /api/captain/{key}/resolve/{id}", b.handleResolve)
	mux.HandleFunc("POST /api/conversation", b.handleConversation)
	mux.HandleFunc("POST /api/tts", b.handleTTS)
	mux.HandleFunc("POST /api/stt", b.handleSTT)
	mux.HandleFunc("GET /api/voice/config", b.handleVoiceConfig)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	if sub, err := fs.Sub(uiFS, "ui"); err == nil {
		fsrv := http.FileServer(http.FS(sub))
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			fsrv.ServeHTTP(w, r)
		}))
	}
	return b.authGate(mux)
}

// Run binds the fleet HTTP listener and serves until ctx is cancelled.
func (b *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: b.Handler()}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	go b.dispatchSweepLoop(ctx)

	slog.Info("fleet listening", "addr", addr, "auth", b.token != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// authorize is the remotedialer Authorizer: validates the captain's bearer token,
// extracts identity headers, and records the captain in the registry. Returning
// authed=false (with no error) rejects the connection with 401.
func (b *Server) authorize(req *http.Request) (clientKey string, authed bool, err error) {
	if b.token != "" {
		got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if strings.TrimSpace(got) != b.token {
			return "", false, nil
		}
	}
	clientKey = strings.TrimSpace(req.Header.Get("X-Shipmates-Identity"))
	if clientKey == "" {
		return "", false, fmt.Errorf("missing X-Shipmates-Identity")
	}
	port := 0
	if p := req.Header.Get("X-Shipmates-Port"); p != "" {
		_, _ = fmt.Sscanf(p, "%d", &port)
	}
	now := time.Now()
	b.mu.Lock()
	existing, ok := b.captains[clientKey]
	if !ok {
		existing = &Captain{ClientKey: clientKey, FirstSeen: now}
		b.captains[clientKey] = existing
	}
	existing.Repo = req.Header.Get("X-Shipmates-Repo")
	existing.RepoURL = req.Header.Get("X-Shipmates-Repo-URL")
	existing.InstallID = req.Header.Get("X-Shipmates-Install-ID")
	existing.Persona = req.Header.Get("X-Shipmates-Persona")
	existing.Port = port
	existing.LastSeen = now
	b.mu.Unlock()
	slog.Info("fleet: captain connected", "client_key", clientKey, "port", port)
	return clientKey, true, nil
}

// handleCaptains lists all captains this Fleet process has seen and marks
// which currently have a live tunnel session.
func (b *Server) handleCaptains(w http.ResponseWriter, r *http.Request) {
	connected := map[string]bool{}
	for _, k := range b.dialer.ListClients() {
		connected[k] = true
	}
	b.mu.Lock()
	out := make([]api.CaptainStatus, 0, len(b.captains))
	for k, l := range b.captains {
		out = append(out, api.CaptainStatus{Captain: *l, Connected: connected[k]})
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// proxyGet returns a handler that reverse-proxies an inbound GET to the named
// path on a specific captain's local server, via the remotedialer tunnel. The
// query string rides along (e.g. /beads?all=1 → /beads.json?all=1).
func (b *Server) proxyGet(captainPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		path := captainPath
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		body, status, err := b.proxy(r.Context(), key, "GET", path, nil)
		writeProxied(w, status, body, err)
	}
}

func (b *Server) handleTell(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	persona := r.PathValue("persona")
	bodyBytes, _ := io.ReadAll(r.Body)
	body, status, err := b.proxy(r.Context(), key, "POST", "/tell/"+persona, bodyBytes)
	writeProxied(w, status, body, err)
}

func (b *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	id := r.PathValue("id")
	bodyBytes, _ := io.ReadAll(r.Body)
	body, status, err := b.proxy(r.Context(), key, "POST", "/resolve/"+id, bodyBytes)
	writeProxied(w, status, body, err)
}

// handleAggregatePending fans out to every connected captain's /pending,
// flattens the results, and returns one array tagged with client_key. Lets the
// UI poll once per tick instead of (N leads) calls.
func (b *Server) handleAggregatePending(w http.ResponseWriter, r *http.Request) {
	clients := b.dialer.ListClients()
	results := make(chan []api.FleetPending, len(clients))
	for _, key := range clients {
		go func(key string) {
			body, status, err := b.proxy(r.Context(), key, "GET", "/pending", nil)
			if err != nil || status >= 300 {
				results <- nil
				return
			}
			var raw []api.Pending
			if err := json.Unmarshal(body, &raw); err != nil {
				results <- nil
				return
			}
			b.mu.Lock()
			captain, _ := b.captains[key]
			b.mu.Unlock()
			repo := ""
			if captain != nil {
				repo = captain.Repo
			}
			out := make([]api.FleetPending, 0, len(raw))
			for _, p := range raw {
				out = append(out, api.FleetPending{Pending: p, ClientKey: key, Repo: repo})
			}
			results <- out
		}(key)
	}
	all := make([]api.FleetPending, 0)
	for range clients {
		all = append(all, <-results...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// handleStream is an SSE endpoint that advances through the captain's bounded
// event log by sequence cursor and pushes new events to the browser.
func (b *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	b.mu.Lock()
	_, known := b.captains[key]
	b.mu.Unlock()
	if !known {
		http.Error(w, "no such captain", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	cursor := uint64(0)
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if parsed, err := strconv.ParseUint(last, 10, 64); err == nil {
			cursor = parsed
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// SSE comment line — browsers ignore it but it keeps the connection
			// warm across HTTP-aware intermediaries that drop idle TCP.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		case <-ticker.C:
		}
		path := fmt.Sprintf("/events?after=%d", cursor)
		body, status, err := b.proxy(r.Context(), key, "GET", path, nil)
		if err != nil || status >= 300 {
			continue
		}
		var events []api.Event
		if err := json.Unmarshal(body, &events); err != nil {
			continue
		}
		if len(events) == 0 {
			continue
		}
		for _, e := range events {
			payload, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, payload); err != nil {
				return
			}
			if e.Seq > cursor {
				cursor = e.Seq
			}
		}
		flusher.Flush()
	}
}

// proxy opens a tunneled TCP connection to the named captain's local server,
// writes a single HTTP request, and returns the response body. We use
// http.NewRequestWithContext + http.ReadResponse on a hand-rolled connection
// because the standard http.Transport doesn't accept an arbitrary net.Conn.
func (b *Server) proxy(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error) {
	rt, status, err := b.captainTransport(clientKey)
	if err != nil {
		return nil, status, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://captain"+path, bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("request captain: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	return out, resp.StatusCode, nil
}

func writeProxied(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
