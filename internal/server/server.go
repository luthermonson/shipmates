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

func NewWithCodexOptions(options codexapp.StartOptions) *Server {
	root, _ := os.Getwd()
	if canonical, err := project.CanonicalRoot(root); err == nil {
		root = canonical
	}
	scope, _ := project.ScopeID(root)
	return &Server{projectRoot: root, projectScope: scope, liveSessions: livesession.NewAt(root, nil, options), stopCh: make(chan struct{}), ready: make(chan struct{})}
}

func (s *Server) Ready() <-chan struct{} { return s.ready }

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Shipmates-Project", s.projectScope)
		if r.Header.Get("X-Shipmates-Project") != s.projectScope {
			http.Error(w, "project mismatch", http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("POST /shutdown", s.localControlOnly(http.HandlerFunc(s.handleShutdown)))
	mux.Handle("POST /api/live/{persona}", s.projectOnly(http.HandlerFunc(s.handleCodexLive)))
	mux.Handle("POST /api/live/{persona}/attach", s.projectOnly(http.HandlerFunc(s.handleCodexAttach)))
	mux.Handle("POST /api/live/{persona}/release", s.projectOnly(http.HandlerFunc(s.handleCodexRelease)))
	mux.Handle("POST /api/live/{persona}/heartbeat", s.projectOnly(http.HandlerFunc(s.handleCodexHeartbeat)))
	mux.Handle("POST /api/live/{persona}/sync", s.projectOnly(http.HandlerFunc(s.handleCodexSync)))
	mux.Handle("POST /api/live/{persona}/action", s.projectOnly(http.HandlerFunc(s.handleCodexControllerAction)))
	mux.Handle("POST /api/live/{persona}/approval", s.projectOnly(http.HandlerFunc(s.handleCodexApproval)))
	mux.Handle("GET /api/live/{persona}/feed", s.projectOnly(http.HandlerFunc(s.handleCodexFeed)))
	mux.Handle("POST /api/live/{persona}/tell", s.projectOnly(http.HandlerFunc(s.handleCodexTell)))
	mux.Handle("POST /api/live/{persona}/interrupt", s.projectOnly(http.HandlerFunc(s.handleCodexInterrupt)))
	mux.Handle("GET /api/local/v1/steer-targets", s.localControlOnly(http.HandlerFunc(s.handleLocalSteerTargets)))
	mux.Handle("POST /api/local/v1/steer-exact", s.localControlOnly(http.HandlerFunc(s.handleLocalSteerExact)))
	mux.Handle("POST /api/local/v1/interrupt-exact", s.localControlOnly(http.HandlerFunc(s.handleLocalInterruptExact)))
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /deregister", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
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
