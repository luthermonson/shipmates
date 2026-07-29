// Package server owns the project-local Codex live-session and exact-turn
// control boundary. Legacy hook, permission, upload, PTY, graph, and Fleet
// authority are intentionally absent.
package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime/env"
)

type Server struct {
	projectRoot     string
	projectScope    string
	liveSessions    *livesession.Manager
	remoteSteer     *livesession.RemoteSteerCoordinator
	remoteInterrupt *livesession.RemoteInterruptCoordinator
	controlToken    string
	stopOnce        sync.Once
	stopCh          chan struct{}
	ready           chan struct{}
}

type discoveryRecord struct {
	SchemaVersion uint64 `json:"schema_version"`
	ProjectRoot   string `json:"project_root"`
	ProjectScope  string `json:"project_scope"`
	Address       string `json:"address"`
	PID           int    `json:"pid"`
	ControlToken  string `json:"control_token"`
}

func New() *Server { return NewWithCodexOptions(codexapp.StartOptions{}) }

// NewWithCodexOptions builds the project-local live-session server. Live
// sessions run on whichever runtime the operator selected: the manager is
// given the production runtime.Selector, so SHIPMATES_RUNTIME and
// .shipmates/config.yaml are honored here exactly as they are by `ask`.
// The server is a separate process, so the client-side `--runtime` flag
// does not reach it — config or the environment variable is how you steer
// the server's runtime.
func NewWithCodexOptions(options codexapp.StartOptions) *Server {
	root, _ := os.Getwd()
	if canonical, err := project.CanonicalRoot(root); err == nil {
		root = canonical
	}
	scope, _ := project.ScopeID(root)
	manager := livesession.NewWithRuntime(root, env.New(), "", nil, options)
	return &Server{projectRoot: root, projectScope: scope, liveSessions: manager, stopCh: make(chan struct{}), ready: make(chan struct{})}
}

func (s *Server) Ready() <-chan struct{} { return s.ready }

// route is one registered endpoint. authenticated routes are wrapped in
// localControlOnly — which checks the project scope AND does a constant-time
// compare of the control token — when the mux is built, so a route cannot be
// registered with the wrong gate by forgetting to wrap it by hand.
type route struct {
	pattern       string
	handler       http.HandlerFunc
	authenticated bool
}

// routes is the single source of truth for the server's surface: what exists
// and what it requires. route_auth_test.go asserts every entry here is
// covered by an auth test, so a new endpoint cannot ship unauthenticated by
// omission — which is exactly how the whole /api/live surface came to be
// gated on nothing but the project scope, a SHA of the project path that is
// documented non-secret. Loopback is not a boundary between local user
// accounts; the token in the 0600 discovery record is.
func (s *Server) routes() []route {
	return []route{
		// The liveness probe callers use before they trust anything else, so
		// it carries no token — but it must not answer WITH the project scope
		// until the caller has proved it already knows it.
		{"GET /health", s.handleHealth, false},

		{"POST /shutdown", s.handleShutdown, true},

		{"POST /api/live/{persona}", s.handleCodexLive, true},
		{"POST /api/live/{persona}/attach", s.handleCodexAttach, true},
		{"POST /api/live/{persona}/release", s.handleCodexRelease, true},
		{"POST /api/live/{persona}/heartbeat", s.handleCodexHeartbeat, true},
		{"POST /api/live/{persona}/sync", s.handleCodexSync, true},
		{"POST /api/live/{persona}/action", s.handleCodexControllerAction, true},
		{"POST /api/live/{persona}/approval", s.handleCodexApproval, true},
		{"GET /api/live/{persona}/feed", s.handleCodexFeed, true},
		{"POST /api/live/{persona}/tell", s.handleCodexTell, true},
		{"POST /api/live/{persona}/show", s.handleLiveShow, true},
		{"POST /api/live/{persona}/interrupt", s.handleCodexInterrupt, true},

		{"GET /api/local/v1/steer-targets", s.handleLocalSteerTargets, true},
		{"POST /api/local/v1/steer-exact", s.handleLocalSteerExact, true},
		{"POST /api/local/v1/interrupt-exact", s.handleLocalInterruptExact, true},

		// 204 stubs retained for legacy clients; they touch no state.
		{"POST /register", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, false},
		{"POST /deregister", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }, false},
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Shipmates-Project") != s.projectScope {
		http.Error(w, "project mismatch", http.StatusConflict)
		return
	}
	w.Header().Set("X-Shipmates-Project", s.projectScope)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		if rt.authenticated {
			mux.Handle(rt.pattern, s.localControlOnly(rt.handler))
			continue
		}
		mux.Handle(rt.pattern, rt.handler)
	}
	return mux
}

// registeredRoutePatterns lists every pattern handler() registers. It exists
// for route_auth_test.go: net/http.ServeMux cannot be enumerated, so without
// this the coverage test would have to scrape source text, which proves
// formatting rather than behavior.
func (s *Server) registeredRoutePatterns() []string {
	out := make([]string, 0, len(s.routes()))
	for _, rt := range s.routes() {
		out = append(out, rt.pattern)
	}
	return out
}

func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	canonical, err := project.CanonicalRoot(s.projectRoot)
	if err != nil || canonical != s.projectRoot {
		return errors.New("invalid project root")
	}
	scope, err := project.ScopeID(canonical)
	if err != nil || scope != s.projectScope {
		return errors.New("invalid project scope")
	}
	if err := project.EnsureServerStateDirectory(s.projectRoot); err != nil {
		return err
	}
	steer, err := livesession.OpenRemoteSteerCoordinator(s.liveSessions, time.Now, filepath.Join(s.projectRoot, project.Dir, "remote-steer"))
	if err != nil {
		return err
	}
	s.remoteSteer = steer
	if err := s.openProductionRemoteInterrupt(); err != nil {
		return err
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	s.controlToken = base64.RawURLEncoding.EncodeToString(token)
	record := discoveryRecord{SchemaVersion: 1, ProjectRoot: s.projectRoot, ProjectScope: s.projectScope, Address: ln.Addr().String(), PID: os.Getpid(), ControlToken: s.controlToken}
	recordRaw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ownedRecord := append(recordRaw, '\n')
	if err := project.WriteServerStateFile(s.projectRoot, "server.json", ownedRecord); err != nil {
		return err
	}
	defer project.RemoveOwnedServerStateFile(s.projectRoot, "server.json", ownedRecord)
	httpServer := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	close(s.ready)
	go func() {
		select {
		case <-ctx.Done():
			s.stopOnce.Do(func() { close(s.stopCh) })
		case <-s.stopCh:
		}
	}()
	go func() {
		<-s.stopCh
		s.closeCodexLive()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *Server) openProductionRemoteInterrupt() error {
	c, err := livesession.OpenRemoteInterruptCoordinator(s.liveSessions, time.Now, filepath.Join(s.projectRoot, project.Dir, "remote-interrupt"), nil)
	if err != nil {
		return err
	}
	s.remoteInterrupt = c
	return nil
}
