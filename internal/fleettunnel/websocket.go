package fleettunnel

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

// WebSocketHandler adapts the single authenticated ship tunnel endpoint to the
// transport-neutral server. TLS is owned by the enclosing production server;
// application ship authentication still occurs before any data frame.
func (s *Server) WebSocketHandler() http.Handler {
	u := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/fleet/v1/tunnel" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "not_found", http.StatusNotFound)
			return
		}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(maxWireBytes)
		_ = s.Serve(r.Context(), &websocketChannel{c: c, peer: "authenticated-transport"})
	})
}

// RunProductionLocal is the ship startup boundary: it loads the restricted
// local identity, inventories trusted project slots, opens an outbound WSS
// connection, and can publish only through RunLocal's slot-only schema.
func RunProductionLocal(ctx context.Context, projectRoot, identityDir string, states []fleetobserve.LocalPersonaState, resume Resume, events []fleetobserve.LocalEvent) (string, uint64, error) {
	return RunProductionLocalConnected(ctx, projectRoot, identityDir, states, resume, events, nil)
}

func RunProductionLocalConnected(ctx context.Context, projectRoot, identityDir string, states []fleetobserve.LocalPersonaState, resume Resume, events []fleetobserve.LocalEvent, connected func(context.Context, fleetidentity.ShipState, uint64) (func(), error)) (string, uint64, error) {
	return RunProductionLocalConnectedUpdates(ctx, projectRoot, identityDir, states, resume, events, nil, connected)
}

func RunProductionLocalConnectedUpdates(ctx context.Context, projectRoot, identityDir string, states []fleetobserve.LocalPersonaState, resume Resume, events []fleetobserve.LocalEvent, updates <-chan []fleetobserve.LocalPersonaState, connected func(context.Context, fleetidentity.ShipState, uint64) (func(), error)) (string, uint64, error) {
	identity, err := fleetidentity.LoadShipState(identityDir)
	if err != nil {
		return "", 0, err
	}
	adapter, err := fleetobserve.OpenLocalStateAdapter(projectRoot)
	if err != nil {
		return "", 0, err
	}
	u, err := url.Parse(identity.FleetDestination)
	loopbackHTTP := err == nil && u.Scheme == "http" && isLoopbackHost(u.Hostname())
	if err != nil || (u.Scheme != "https" && !loopbackHTTP) || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", 0, fail(InvalidHandshake)
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/api/fleet/v1/tunnel"
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return "", 0, fail(Backpressure)
	}
	conn.SetReadLimit(maxWireBytes)
	var hook func(context.Context, uint64) (func(), error)
	if connected != nil {
		hook = func(c context.Context, g uint64) (func(), error) { return connected(c, identity, g) }
	}
	client, err := NewClient(ClientConfig{FleetID: identity.FleetID, ServiceIdentity: identity.FleetServiceIdentity, CredentialID: identity.CredentialID, Secret: identity.CredentialSecret, ShipID: identity.ShipID, IOTimeout: 10 * time.Second, Connected: hook})
	if err != nil {
		_ = conn.Close()
		return "", 0, err
	}
	return client.RunLocalUpdates(ctx, &websocketChannel{c: conn, peer: identity.FleetServiceIdentity}, adapter, states, resume, events, updates)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type websocketChannel struct {
	c               *websocket.Conn
	peer            string
	readMu, writeMu sync.Mutex
	once            sync.Once
}

func (c *websocketChannel) Send(ctx context.Context, b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		_ = c.c.SetWriteDeadline(d)
	}
	return c.c.WriteMessage(websocket.TextMessage, b)
}
func (c *websocketChannel) Receive(ctx context.Context) ([]byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		_ = c.c.SetReadDeadline(d)
	}
	t, b, err := c.c.ReadMessage()
	if err != nil {
		return nil, err
	}
	if t != websocket.TextMessage {
		return nil, fail(ProtocolViolation)
	}
	return b, nil
}
func (c *websocketChannel) PeerServiceIdentity() string { return c.peer }
func (c *websocketChannel) Close() error {
	var err error
	c.once.Do(func() { err = c.c.Close() })
	return err
}
