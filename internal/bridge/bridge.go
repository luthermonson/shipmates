// Package bridge is the central rendezvous server a power user can run when
// they want a single pane across many shipmates leads. Leads dial out to the
// bridge over a websocket (rancher/remotedialer); the bridge then proxies
// operator-facing /api/* calls back through the tunnel to each lead's local
// 127.0.0.1 server. Optional SQLite persistence mirrors lead events for replay.
//
// The bridge is *not* a UI host. It exposes a JSON API consumed by the
// shipmates CLI (`shipmates bridge ls / tail / tell / pending / resolve`). A
// browser frontend could be glued on later via the same API.
package bridge

import (
	"bytes"
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
	"strings"
	"sync"
	"time"

	"github.com/rancher/remotedialer"
	_ "modernc.org/sqlite"
)

//go:embed all:ui
var uiFS embed.FS

// Server is the bridge: it embeds a remotedialer.Server (which accepts inbound
// websocket connections from leads) and adds a small /api/* surface that the
// shipmates CLI hits to list leads, tail their event feeds, and proxy commands
// back through the tunnels.
type Server struct {
	dialer *remotedialer.Server
	token  string

	mu    sync.Mutex
	leads map[string]*Lead // keyed by clientKey

	store *store // nil when --store wasn't passed
}

// Lead is the bridge's record of one connected shipmates lead.
type Lead struct {
	ClientKey   string    `json:"client_key"`
	Repo        string    `json:"repo"`
	InstallID   string    `json:"install_id"`
	Persona     string    `json:"persona"`
	Port        int       `json:"port"` // lead's local server port (for tunnel dial)
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
}

// Options configures the bridge.
type Options struct {
	Addr  string // listen address (e.g. ":8443")
	Token string // shared secret; if empty, auth is disabled (dev only)
	Store string // optional SQLite path; empty = ephemeral
}

// New constructs the bridge. The returned Server is ready to Run.
func New(opts Options) (*Server, error) {
	b := &Server{
		token: strings.TrimSpace(opts.Token),
		leads: map[string]*Lead{},
	}
	if opts.Store != "" {
		s, err := openStore(opts.Store)
		if err != nil {
			return nil, fmt.Errorf("open store %s: %w", opts.Store, err)
		}
		b.store = s
	}
	b.dialer = remotedialer.New(b.authorize, remotedialer.DefaultErrorWriter)
	return b, nil
}

// Run binds the bridge HTTP listener and serves until ctx is cancelled.
func (b *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/connect", b.dialer)
	mux.HandleFunc("GET /login", b.handleLogin)
	mux.HandleFunc("POST /login", b.handleLogin)
	mux.HandleFunc("POST /logout", b.handleLogout)
	mux.HandleFunc("GET /api/leads", b.handleLeads)
	mux.HandleFunc("GET /api/pending", b.handleAggregatePending)
	mux.HandleFunc("GET /api/lead/{key}/feed", b.proxyGet("/feed"))
	mux.HandleFunc("GET /api/lead/{key}/events", b.proxyGet("/events"))
	mux.HandleFunc("GET /api/lead/{key}/pending", b.proxyGet("/pending"))
	mux.HandleFunc("GET /api/lead/{key}/status", b.proxyGet("/status.json"))
	mux.HandleFunc("GET /api/status", b.handleAggregateStatus)
	mux.HandleFunc("POST /api/lead/{key}/pty/{persona}/start", b.proxyPTYPost("/pty/%s/start"))
	mux.HandleFunc("POST /api/lead/{key}/pty/{persona}/input", b.proxyPTYPost("/pty/%s/input"))
	mux.HandleFunc("POST /api/lead/{key}/pty/{persona}/resize", b.proxyPTYPost("/pty/%s/resize"))
	mux.HandleFunc("GET /api/lead/{key}/pty/{persona}/snapshot", b.proxyGet2("/pty/%s/snapshot"))
	mux.HandleFunc("GET /api/lead/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	mux.HandleFunc("GET /api/lead/{key}/stream", b.handleStream)
	mux.HandleFunc("POST /api/lead/{key}/tell/{persona}", b.handleTell)
	mux.HandleFunc("POST /api/lead/{key}/resolve/{id}", b.handleResolve)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })

	// Mount the embedded UI at /. fs.Sub drops the "ui" prefix so paths like
	// /style.css and /app.js resolve to ui/style.css and ui/app.js. The
	// individual UI files (style.css, app.js) need to be readable without
	// auth so the /login page can style itself; auth-gating is enforced by
	// authGate based on path, and the login.html / style.css / app.js assets
	// are harmless on their own.
	if sub, err := fs.Sub(uiFS, "ui"); err == nil {
		mux.Handle("/", http.FileServer(http.FS(sub)))
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

	slog.Info("bridge listening", "addr", addr, "auth", b.token != "", "store", b.store != nil)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// authorize is the remotedialer Authorizer: validates the lead's bearer token,
// extracts identity headers, and records the lead in the registry. Returning
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
	existing, ok := b.leads[clientKey]
	if !ok {
		existing = &Lead{ClientKey: clientKey, FirstSeen: now}
		b.leads[clientKey] = existing
	}
	existing.Repo = req.Header.Get("X-Shipmates-Repo")
	existing.InstallID = req.Header.Get("X-Shipmates-Install-ID")
	existing.Persona = req.Header.Get("X-Shipmates-Persona")
	existing.Port = port
	existing.LastSeen = now
	b.mu.Unlock()
	if b.store != nil {
		_ = b.store.upsertLead(existing)
	}
	slog.Info("bridge: lead connected", "client_key", clientKey, "port", port)
	return clientKey, true, nil
}

// handleLeads lists all leads the bridge knows about, intersected with the set
// currently connected (so stale entries from prior runs don't show up). When a
// store is configured, disconnected leads still surface from the store so an
// operator can replay their history.
func (b *Server) handleLeads(w http.ResponseWriter, r *http.Request) {
	connected := map[string]bool{}
	for _, k := range b.dialer.ListClients() {
		connected[k] = true
	}
	type wire struct {
		Lead
		Connected bool `json:"connected"`
	}
	b.mu.Lock()
	out := make([]wire, 0, len(b.leads))
	for k, l := range b.leads {
		out = append(out, wire{Lead: *l, Connected: connected[k]})
	}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// proxyGet returns a handler that reverse-proxies an inbound GET to the named
// path on a specific lead's local server, via the remotedialer tunnel.
func (b *Server) proxyGet(leadPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		body, status, err := b.proxy(r.Context(), key, "GET", leadPath, nil)
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

// handleAggregatePending fans out to every connected lead's /pending.json,
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
			lead, _ := b.leads[key]
			b.mu.Unlock()
			repo := ""
			if lead != nil {
				repo = lead.Repo
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

// handleStream is an SSE endpoint that polls the lead's /events through the
// tunnel and pushes new events to the connected browser. The connection lives
// for as long as the client holds it open; closing the EventSource (e.g. the
// operator navigates away from the tab) ends the goroutine and drops history,
// matching the "ephemeral live stream" model we picked for v1.
func (b *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	b.mu.Lock()
	_, known := b.leads[key]
	b.mu.Unlock()
	if !known {
		http.Error(w, "no such lead", http.StatusNotFound)
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
	high := ""

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
		var events []struct {
			Time    string `json:"time"`
			Persona string `json:"persona"`
			Type    string `json:"type"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(body, &events); err != nil {
			continue
		}
		for _, e := range events {
			if e.Time <= high {
				continue
			}
			data, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			high = e.Time
		}
		flusher.Flush()
	}
}

// proxy opens a tunneled TCP connection to the named lead's local server,
// writes a single HTTP request, and returns the response body. We use
// http.NewRequestWithContext + http.ReadResponse on a hand-rolled connection
// because the standard http.Transport doesn't accept an arbitrary net.Conn.
func (b *Server) proxy(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error) {
	b.mu.Lock()
	lead, ok := b.leads[clientKey]
	b.mu.Unlock()
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no such lead: %s", clientKey)
	}
	if !b.dialer.HasSession(clientKey) {
		return nil, http.StatusGatewayTimeout, fmt.Errorf("lead %s not currently connected", clientKey)
	}
	dial := b.dialer.Dialer(clientKey)
	addr := fmt.Sprintf("127.0.0.1:%d", lead.Port)
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("dial lead: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: lead\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", method, path, len(body))
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

// readHTTPResponse reads a minimal HTTP/1.1 response off a net.Conn. We can't
// use http.ReadResponse directly without a *bufio.Reader, which is exactly what
// it wants — wrap accordingly.
func readHTTPResponse(conn net.Conn) (*proxyResp, error) {
	all, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	// Split headers from body on the first \r\n\r\n.
	sep := []byte("\r\n\r\n")
	idx := bytes.Index(all, sep)
	if idx < 0 {
		return nil, fmt.Errorf("malformed response: no header/body separator")
	}
	headerBlock := string(all[:idx])
	body := all[idx+len(sep):]

	// First line: "HTTP/1.1 <code> <text>"
	firstEnd := strings.Index(headerBlock, "\r\n")
	statusLine := headerBlock
	if firstEnd > 0 {
		statusLine = headerBlock[:firstEnd]
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed status line: %q", statusLine)
	}
	var code int
	if _, err := fmt.Sscanf(parts[1], "%d", &code); err != nil {
		return nil, fmt.Errorf("bad status code: %w", err)
	}
	return &proxyResp{Status: code, Body: body}, nil
}

func writeProxied(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// mirrorLoop polls each connected lead's /events endpoint at a fixed interval
// and persists any new events to SQLite. The lead-side feed is monotonically
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
	CREATE TABLE IF NOT EXISTS leads (
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

func (s *store) upsertLead(l *Lead) error {
	_, err := s.db.Exec(`
		INSERT INTO leads (client_key, repo, install_id, persona, first_seen, last_seen)
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
