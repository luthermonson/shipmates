package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/rancher/remotedialer"
)

// fleetReconnectBackoff is how long to wait between failed dial attempts to
// the fleet. Kept short so a transient fleet restart heals quickly.
const fleetReconnectBackoff = 5 * time.Second

// startFleet opens (and keeps open) an outbound websocket from the captain to a
// central `shipmates fleet serve` instance, when one is configured. The fleet
// can then dial back through the tunnel to reach this server's local API. No-op
// when no fleet URL is configured. Returns immediately; the connect loop runs
// until ctx is cancelled or the parent stopCh closes.
func (s *Server) startFleet(ctx context.Context, conf *project.Config) {
	if conf == nil || strings.TrimSpace(conf.Fleet.URL) == "" {
		return
	}

	name := strings.TrimSpace(conf.Fleet.Name)
	if name == "" {
		name = project.RepoName()
	}
	clientKey := fmt.Sprintf("%s:%s", name, captainPersona())
	// Install id is no longer part of the clientKey (kept human-readable), but
	// we still surface it in the X-Shipmates-Install-ID header for audit and
	// to let the fleet correlate sessions across reconnects when a captain's
	// configured name is ambiguous.
	installID, _ := project.InstallID()

	wsURL, err := toWebsocketURL(conf.Fleet.URL)
	if err != nil {
		slog.Warn("fleet disabled: bad url", "url", conf.Fleet.URL, "err", err)
		return
	}

	// identity for captain→fleet callbacks (the beads nudge)
	s.mu.Lock()
	s.fleetURL = strings.TrimSpace(conf.Fleet.URL)
	s.fleetToken = conf.Fleet.Token()
	s.fleetKey = clientKey
	s.mu.Unlock()

	headers := http.Header{}
	headers.Set("X-Shipmates-Identity", clientKey)
	headers.Set("X-Shipmates-Repo", project.RepoName())
	if u := project.RepoWebURL(); u != "" {
		headers.Set("X-Shipmates-Repo-URL", u)
	}
	headers.Set("X-Shipmates-Install-ID", installID)
	headers.Set("X-Shipmates-Persona", captainPersona())
	headers.Set("X-Shipmates-Port", fmt.Sprintf("%d", s.port))
	if t := conf.Fleet.Token(); t != "" {
		headers.Set("Authorization", "Bearer "+t)
	}

	go s.fleetLoop(ctx, wsURL, headers, clientKey)
}

// fleetLoop reconnects to the fleet with a small backoff until the context is
// cancelled or the server stops.
func (s *Server) fleetLoop(ctx context.Context, wsURL string, headers http.Header, clientKey string) {
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-dialCtx.Done():
		}
	}()

	auth := s.fleetAuthorizer()
	for {
		if dialCtx.Err() != nil {
			return
		}
		slog.Info("fleet: connecting", "url", wsURL, "client_key", clientKey)
		err := remotedialer.ClientConnect(dialCtx, wsURL, headers, nil, auth, nil)
		if dialCtx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("fleet: disconnected", "err", err)
		}
		select {
		case <-dialCtx.Done():
			return
		case <-time.After(fleetReconnectBackoff):
		}
	}
}

// fleetAuthorizer is the captain-side ConnectAuthorizer: it gates which inbound
// dial requests (initiated by the fleet through the tunnel) the captain will
// honor. We only allow dialing back into our own local server on its port.
func (s *Server) fleetAuthorizer() remotedialer.ConnectAuthorizer {
	return func(proto, address string) bool {
		if proto != "tcp" {
			return false
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return false
		}
		if host != "127.0.0.1" && host != "localhost" {
			return false
		}
		return port == fmt.Sprintf("%d", s.port)
	}
}

// toWebsocketURL converts the user's fleet URL to a ws:// or wss:// connect
// URL. We accept http(s):// for convenience and rewrite the scheme. The path
// defaults to /connect to match `shipmates fleet serve`.
func toWebsocketURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already correct
	default:
		return "", fmt.Errorf("unsupported scheme %q (use http, https, ws, or wss)", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/connect"
	}
	return u.String(), nil
}

// captainPersona returns the configured captain persona name (shipmates.yaml
// `captainPersona:`), defaulting to "captain". Encoded in the clientKey so the
// fleet can label connected captains and open the right front-door persona.
func captainPersona() string {
	if c, err := project.LoadConfig(); err == nil {
		if p := strings.TrimSpace(c.CaptainPersona); p != "" {
			return p
		}
	}
	return "captain"
}
