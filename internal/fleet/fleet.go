// Package fleet is the central rendezvous server a power user can run when
// they want a single pane across many shipmates captains. Captains dial out
// to the fleet over a websocket (rancher/remotedialer); the fleet then
// proxies operator-facing /api/* calls back through the tunnel to each
// captain's local 127.0.0.1 server. Optional SQLite persistence mirrors
// captain events for replay.
//
// The fleet is *not* a UI host. It exposes a JSON API consumed by the
// shipmates CLI (`shipmates fleet ls / tail / tell / pending / resolve`). A
// browser frontend could be glued on later via the same API.
package fleet

import (
	"bufio"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/personaname"
	"github.com/rancher/remotedialer"
	_ "modernc.org/sqlite"
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

	store  *store       // nil when --store wasn't passed
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
	url      string // OpenAI-compatible base URL (…/v1); "" disables /api/conversation
	model    string // model tag/name sent in requests
	key      string // bearer key for hosted OAI-compatible endpoints; "" = no auth (local)
	brain    *claudeBrain // non-nil when --llm-backend=claude-cli
	voice    string // voice tag: Edge (en-US-AriaNeural) or the OAI server's voice name
	ttsURL   string // OpenAI-compatible /v1/audio/speech endpoint; "" = Edge websocket
	ttsModel string // model field for OAI-style TTS servers
	sttURL   string // transcription endpoint; "" disables /api/stt
	sttModel string // model field sent to the transcription server (OAI-style)
	client   *http.Client
}

// Captain is the fleet's record of one connected shipmates captain.
type Captain struct {
	ClientKey   string    `json:"client_key"`
	Repo        string    `json:"repo"`
	RepoURL     string    `json:"repo_url,omitempty"` // browsable origin URL (for gh links)
	InstallID   string    `json:"install_id"`
	Persona     string    `json:"persona"`
	Port        int       `json:"port"` // captain's local server port (for tunnel dial)
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`

	// APIToken is the captain's per-run bearer credential for its own local
	// HTTP API, handed to us on the tunnel connect. Every proxied request has
	// to carry it. json:"-" keeps it out of /api/captains and anything else
	// that renders a Captain — it is a credential, not identity.
	APIToken string `json:"-"`
}

// Options configures the fleet.
type Options struct {
	Addr        string // listen address (e.g. ":8443")
	Token       string // shared secret; if empty, auth is disabled (dev only)
	Store       string // optional SQLite path; empty = ephemeral
	LLMBackend  string // "openai" (default: chat-completions HTTP) or "claude-cli" (spawn claude -p on this host)
	LLMURL      string // OpenAI-compatible base URL incl. /v1 (ollama, llama.cpp, LM Studio, OpenAI…); enables /api/conversation
	LLMModel    string // model name for the conversation loop, e.g. "qwen2.5:7b" or "haiku"
	LLMKey      string // bearer key for hosted endpoints (read from env by the CLI layer); "" = no auth
	TTSVoice    string // voice tag (Edge: en-US-AriaNeural; OAI servers: e.g. af_heart); empty + no TTSURL disables /api/tts
	TTSURL      string // optional OpenAI-compatible /v1/audio/speech endpoint (kokoro-fastapi etc.); overrides Edge
	TTSModel    string // model field for OAI-style TTS servers
	STTURL      string // optional transcription endpoint (whisper.cpp /inference or OAI /v1/audio/transcriptions); empty disables /api/stt
	STTModel    string // model name forwarded to OAI-style STT servers; whisper.cpp ignores it
	// PolicyPath overrides the on-disk fleet-policy YAML location. Empty =
	// use the SHIPMATES_FLEET_POLICY env var or ~/.shipmates/fleet-policy.yaml.
	PolicyPath string
}

// New constructs the fleet. The returned Server is ready to Run.
func New(opts Options) (*Server, error) {
	b := &Server{
		token: strings.TrimSpace(opts.Token),
		captains: map[string]*Captain{},
	}
	if opts.Store != "" {
		s, err := openStore(opts.Store)
		if err != nil {
			return nil, fmt.Errorf("open store %s: %w", opts.Store, err)
		}
		b.store = s
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

// Run binds the fleet HTTP listener and serves until ctx is cancelled.
func (b *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/connect", b.dialer)
	mux.HandleFunc("GET /login", b.handleLogin)
	mux.HandleFunc("POST /login", b.handleLogin)
	mux.HandleFunc("POST /logout", b.handleLogout)
	mux.HandleFunc("GET /api/captains", b.handleCaptains)
	mux.HandleFunc("GET /api/fleet-policy", b.handleFleetPolicy)
	mux.HandleFunc("GET /api/pending", b.handleAggregatePending)
	mux.HandleFunc("GET /api/captain/{key}/feed", b.proxyGet("/feed"))
	mux.HandleFunc("GET /api/captain/{key}/events", b.proxyGet("/events"))
	mux.HandleFunc("GET /api/captain/{key}/pending", b.proxyGet("/pending"))
	mux.HandleFunc("GET /api/captain/{key}/status", b.proxyGet("/status.json"))
	mux.HandleFunc("GET /api/captain/{key}/beads", b.proxyGet("/beads.json"))
	mux.HandleFunc("GET /api/captain/{key}/beads/summary", b.proxyGet("/beads/summary"))
	mux.HandleFunc("GET /api/status", b.handleAggregateStatus)
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/start", b.proxyPTYPost("/pty/%s/start"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/input", b.proxyPTYPost("/pty/%s/input"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/resize", b.proxyPTYPost("/pty/%s/resize"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/takeover", b.proxyPTYPost("/pty/%s/takeover"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/release", b.proxyPTYPost("/pty/%s/release"))
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/snapshot", b.proxyGet2("/pty/%s/snapshot", "persona", personaname.Valid))
	mux.HandleFunc("GET /api/captain/{key}/bead/{id}", b.proxyGet2("/bead/%s", "id", beadIDOK))
	mux.HandleFunc("POST /api/captain/{key}/bead", b.proxyPost("/bead"))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/close", b.proxyPost2("/bead/%s/close", "id", beadIDOK))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/update", b.proxyPost2("/bead/%s/update", "id", beadIDOK))
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

	// Mount the embedded UI at /. fs.Sub drops the "ui" prefix so paths like
	// /style.css and /app.js resolve to ui/style.css and ui/app.js. The
	// individual UI files (style.css, app.js) need to be readable without
	// auth so the /login page can style itself; auth-gating is enforced by
	// authGate based on path, and the login.html / style.css / app.js assets
	// are harmless on their own.
	if sub, err := fs.Sub(uiFS, "ui"); err == nil {
		fsrv := http.FileServer(http.FS(sub))
		// no-cache: the UI ships embedded in the binary and changes on every
		// deploy; a stale cached app.js against fresh index.html breaks the
		// page in confusing ways. Revalidating a ~300KB UI per load is cheap.
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			fsrv.ServeHTTP(w, r)
		}))
	}

	srv := &http.Server{Addr: addr, Handler: b.authGate(mux)}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	if b.store != nil {
		go b.mirrorLoop(ctx)
	}
	go b.dispatchSweepLoop(ctx)

	slog.Info("fleet listening", "addr", addr, "auth", b.token != "", "store", b.store != nil)
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
	existing.APIToken = sanitizeAPIToken(req.Header.Get("X-Shipmates-API-Token"))
	existing.LastSeen = now
	b.mu.Unlock()
	if b.store != nil {
		_ = b.store.upsertCaptain(existing)
	}
	slog.Info("fleet: captain connected", "client_key", clientKey, "port", port)
	return clientKey, true, nil
}

// handleCaptains lists all captains the fleet knows about, intersected with
// the set currently connected (so stale entries from prior runs don't show
// up). When a store is configured, disconnected captains still surface from
// the store so an operator can replay their history.
func (b *Server) handleCaptains(w http.ResponseWriter, r *http.Request) {
	connected := map[string]bool{}
	for _, k := range b.dialer.ListClients() {
		connected[k] = true
	}
	type wire struct {
		Captain
		Connected bool `json:"connected"`
	}
	b.mu.Lock()
	out := make([]wire, 0, len(b.captains))
	for k, l := range b.captains {
		out = append(out, wire{Captain: *l, Connected: connected[k]})
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
	// ServeMux hands back a percent-DECODED segment, so "%0d%0aGET /shutdown"
	// arrives here as real CRLF. Reject before it can reach the request line.
	if !personaname.Valid(persona) {
		http.Error(w, "bad persona", http.StatusBadRequest)
		return
	}
	bodyBytes, _ := io.ReadAll(r.Body)
	body, status, err := b.proxy(r.Context(), key, "POST", "/tell/"+url.PathEscape(persona), bodyBytes)
	writeProxied(w, status, body, err)
}

func (b *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	id := r.PathValue("id")
	// Pending-request ids are an 8-char UUID prefix; beadIDOK's alphabet
	// covers them and rejects anything that could reframe the request.
	if !beadIDOK(id) {
		http.Error(w, "bad request id", http.StatusBadRequest)
		return
	}
	bodyBytes, _ := io.ReadAll(r.Body)
	body, status, err := b.proxy(r.Context(), key, "POST", "/resolve/"+url.PathEscape(id), bodyBytes)
	writeProxied(w, status, body, err)
}

// handleAggregatePending fans out to every connected captain's /pending.json,
// flattens the results, and returns one array tagged with client_key. Lets the
// UI poll once per tick instead of (N leads) calls.
func (b *Server) handleAggregatePending(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		ID        string `json:"id"`
		Persona   string `json:"persona"`
		Tool      string `json:"tool"`
	}
	clients := b.dialer.ListClients()
	results := make(chan []entry, len(clients))
	for _, key := range clients {
		go func(key string) {
			body, status, err := b.proxy(r.Context(), key, "GET", "/pending.json", nil)
			if err != nil || status >= 300 {
				results <- nil
				return
			}
			var raw []struct {
				ID      string `json:"id"`
				Persona string `json:"persona"`
				Tool    string `json:"tool"`
			}
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
			out := make([]entry, 0, len(raw))
			for _, p := range raw {
				out = append(out, entry{ClientKey: key, Repo: repo, ID: p.ID, Persona: p.Persona, Tool: p.Tool})
			}
			results <- out
		}(key)
	}
	all := make([]entry, 0)
	for range clients {
		all = append(all, <-results...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// handleStream is an SSE endpoint that polls the captain's /events through the
// tunnel and pushes new events to the connected browser. The connection lives
// for as long as the client holds it open; closing the EventSource (e.g. the
// operator navigates away from the tab) ends the goroutine and drops history,
// matching the "ephemeral live stream" model we picked for v1.
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
	// Index watermark, not timestamp: the captain's event log is append-only, so
	// "everything past what we already sent" is exact. A timestamp watermark
	// silently dropped all-but-the-first of any same-second batch — which is
	// precisely what a broadcast tell produces.
	sent := 0

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
		body, status, err := b.proxy(r.Context(), key, "GET", "/events", nil)
		if err != nil || status >= 300 {
			continue
		}
		// Raw passthrough: the captain's event schema grows (timeline stats,
		// permission fields) and re-marshaling a named subset here silently
		// stripped every field this proxy didn't know about.
		var events []json.RawMessage
		if err := json.Unmarshal(body, &events); err != nil {
			continue
		}
		if len(events) < sent {
			sent = 0 // captain restarted and its log reset; replay from the top
		}
		if len(events) == sent {
			continue
		}
		for _, e := range events[sent:] {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", e); err != nil {
				return
			}
		}
		sent = len(events)
		flusher.Flush()
	}
}

// proxy opens a tunneled TCP connection to the named captain's local server,
// writes a single HTTP request, and returns the response body. We use
// http.NewRequestWithContext + http.ReadResponse on a hand-rolled connection
// because the standard http.Transport doesn't accept an arbitrary net.Conn.
func (b *Server) proxy(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error) {
	if err := checkProxyPath(path); err != nil {
		return nil, http.StatusBadRequest, err
	}
	b.mu.Lock()
	captain, ok := b.captains[clientKey]
	b.mu.Unlock()
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no such captain: %s", clientKey)
	}
	if !b.dialer.HasSession(clientKey) {
		return nil, http.StatusGatewayTimeout, fmt.Errorf("captain %s not currently connected", clientKey)
	}
	dial := b.dialer.Dialer(clientKey)
	addr := fmt.Sprintf("127.0.0.1:%d", captain.Port)
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("dial captain: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: captain\r\n%sContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		method, path, authHeaderLine(captain.APIToken), len(body))
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, http.StatusBadGateway, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return nil, http.StatusBadGateway, err
		}
	}
	resp, err := readHTTPResponse(conn)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	return resp.Body, resp.Status, nil
}

type proxyResp struct {
	Status int
	Body   []byte
}

// checkProxyPath is the last line of defence before a request-target is
// pasted into a hand-rolled HTTP/1.1 request line. Callers are expected to
// validate their identifiers and url.PathEscape every interpolated segment,
// but this proxy is a public seam inside the package and a future caller will
// forget: a single space splits the request target, and a CR or LF ends the
// request line outright, letting one proxied call become several — enough to
// promote a caller from the curated /api/* subset to any endpoint the
// (unauthenticated) captain-local server exposes.
//
// We reject rather than strip: silently rewriting a hostile path hides the
// attempt, and no legitimate caller in this package ever builds one.
func checkProxyPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("proxy: path %q must start with /", path)
	}
	for i := 0; i < len(path); i++ {
		if c := path[i]; c <= ' ' || c == 0x7f {
			return fmt.Errorf("proxy: refusing path with control character %q at offset %d", c, i)
		}
	}
	return nil
}

// readHTTPResponse reads one HTTP/1.1 response off a net.Conn via the stdlib
// parser, which decodes Transfer-Encoding: chunked. The previous hand-rolled
// header/body split did not — Go servers only chunk responses that outgrow
// the 2KB write buffer, so small payloads worked while anything bigger (a
// grown /events history) returned chunk-size framing glued into the body and
// broke every JSON consumer downstream.
func readHTTPResponse(conn net.Conn) (*proxyResp, error) {
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &proxyResp{Status: resp.StatusCode, Body: body}, nil
}

func writeProxied(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// mirrorLoop polls each connected captain's /events endpoint at a fixed interval
// and persists any new events to SQLite. The captain-side feed is monotonically
// appended, so we dedupe by tracking the highest (Time, Persona, Type, Text)
// composite per client_key.
func (b *Server) mirrorLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastTS := map[string]string{} // clientKey -> max Time string seen
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, key := range b.dialer.ListClients() {
			body, status, err := b.proxy(ctx, key, "GET", "/events", nil)
			if err != nil || status >= 300 {
				continue
			}
			var events []struct {
				Time    string `json:"time"`
				Persona string `json:"persona"`
				Type    string `json:"type"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(body, &events); err != nil {
				continue
			}
			high := lastTS[key]
			for _, e := range events {
				if e.Time <= high {
					continue
				}
				_ = b.store.insertEvent(key, e.Time, e.Persona, e.Type, e.Text)
			}
			if len(events) > 0 {
				if t := events[len(events)-1].Time; t > high {
					lastTS[key] = t
				}
			}
		}
	}
}

// store is the optional SQLite mirror.
type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	const schema = `
	CREATE TABLE IF NOT EXISTS captains (
		client_key TEXT PRIMARY KEY,
		repo TEXT, install_id TEXT, persona TEXT,
		first_seen INTEGER, last_seen INTEGER
	);
	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		client_key TEXT, ts TEXT, persona TEXT, kind TEXT, text TEXT
	);
	CREATE INDEX IF NOT EXISTS events_client_ts ON events(client_key, ts);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) upsertCaptain(l *Captain) error {
	_, err := s.db.Exec(`
		INSERT INTO captains (client_key, repo, install_id, persona, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_key) DO UPDATE SET
			repo=excluded.repo,
			install_id=excluded.install_id,
			persona=excluded.persona,
			last_seen=excluded.last_seen
	`, l.ClientKey, l.Repo, l.InstallID, l.Persona, l.FirstSeen.Unix(), l.LastSeen.Unix())
	return err
}

func (s *store) insertEvent(clientKey, ts, persona, kind, text string) error {
	_, err := s.db.Exec(`INSERT INTO events (client_key, ts, persona, kind, text) VALUES (?, ?, ?, ?, ?)`,
		clientKey, ts, persona, kind, text)
	return err
}

// Close releases the SQLite store, if any.
func (b *Server) Close() error {
	if b.store != nil {
		return b.store.db.Close()
	}
	return nil
}
