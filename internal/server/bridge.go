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

// bridgeReconnectBackoff is how long to wait between failed dial attempts to
// the bridge. Kept short so a transient bridge restart heals quickly.
const bridgeReconnectBackoff = 5 * time.Second

// startBridge opens (and keeps open) an outbound websocket from the lead to a
// central `shipmates bridge serve` instance, when one is configured. The bridge
// can then dial back through the tunnel to reach this server's local API. No-op
// when no bridge URL is configured. Returns immediately; the connect loop runs
// until ctx is cancelled or the parent stopCh closes.
func (s *Server) startBridge(ctx context.Context, conf *project.Config) {
	if conf == nil || strings.TrimSpace(conf.Bridge.URL) == "" {
		return
	}

	name := strings.TrimSpace(conf.Bridge.Name)
	if name == "" {
		name = project.RepoName()
	}
	clientKey := fmt.Sprintf("%s:%s", name, leadPersona())
	// Install id is no longer part of the clientKey (kept human-readable), but
	// we still surface it in the X-Shipmates-Install-ID header for audit and
	// to let the bridge correlate sessions across reconnects when a lead's
	// configured name is ambiguous.
	installID, _ := project.InstallID()

	wsURL, err := toWebsocketURL(conf.Bridge.URL)
	if err != nil {
		slog.Warn("bridge disabled: bad url", "url", conf.Bridge.URL, "err", err)
		return
	}

	headers := http.Header{}
	headers.Set("X-Shipmates-Identity", clientKey)
	headers.Set("X-Shipmates-Repo", project.RepoName())
	headers.Set("X-Shipmates-Install-ID", installID)
	headers.Set("X-Shipmates-Persona", leadPersona())
	headers.Set("X-Shipmates-Port", fmt.Sprintf("%d", s.port))
	if t := conf.Bridge.Token(); t != "" {
		headers.Set("Authorization", "Bearer "+t)
	}

	go s.bridgeLoop(ctx, wsURL, headers, clientKey)
}

// bridgeLoop reconnects to the bridge with a small backoff until the context is
// cancelled or the server stops.
func (s *Server) bridgeLoop(ctx context.Context, wsURL string, headers http.Header, clientKey string) {
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-dialCtx.Done():
		}
	}()

	auth := s.bridgeAuthorizer()
	for {
		if dialCtx.Err() != nil {
			return
		}
		slog.Info("bridge: connecting", "url", wsURL, "client_key", clientKey)
		err := remotedialer.ClientConnect(dialCtx, wsURL, headers, nil, auth, nil)
		if dialCtx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("bridge: disconnected", "err", err)
		}
		select {
		case <-dialCtx.Done():
			return
		case <-time.After(bridgeReconnectBackoff):
		}
	}
}

// bridgeAuthorizer is the lead-side ConnectAuthorizer: it gates which inbound
// dial requests (initiated by the bridge through the tunnel) the lead will
// honor. We only allow dialing back into our own local server on its port.
func (s *Server) bridgeAuthorizer() remotedialer.ConnectAuthorizer {
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

// toWebsocketURL converts the user's bridge URL to a ws:// or wss:// connect
// URL. We accept http(s):// for convenience and rewrite the scheme. The path
// defaults to /connect to match `shipmates bridge serve`.
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

// leadPersona returns the configured lead persona name. Defaults to "lead".
// (Encoded in the clientKey so the bridge can label connected leads.)
func leadPersona() string {
	// Currently the lead persona is conventionally named "lead". If we add a
	// config knob later (e.g. config.LeadPersona), read it here.
	return "lead"
}
