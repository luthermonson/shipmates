//go:build unix

package fleettunnel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/fleetconfig"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

// BuildQualifierTLSConfig binds the administrator-loaded trust identity to
// the TLS connection. The SPKI pin is checked in addition to normal hostname,
// chain, and certificate validity verification.
func BuildQualifierTLSConfig(loaded fleetconfig.Loaded) (*tls.Config, error) {
	if err := loaded.Config.Validate(); err != nil {
		return nil, errors.New("qualifier_config_invalid")
	}
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(loaded.Trust); !ok {
		return nil, errors.New("qualifier_trust_invalid")
	}
	pin, err := hex.DecodeString(loaded.Config.SPKISHA256)
	if err != nil || len(pin) != sha256.Size {
		return nil, errors.New("qualifier_spki_invalid")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: loaded.Config.TLSServerName,
		NextProtos: []string{"http/1.1"},
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return errors.New("qualifier_certificate_unverified")
			}
			sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if !equalBytes(sum[:], pin) {
				return errors.New("qualifier_spki_mismatch")
			}
			return nil
		},
	}, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// QualifyHandshake consumes the same production Client handshake with a
// caller-provided channel. Tests use this seam without listeners; production
// callers use QualifyProduction below.
func QualifyHandshake(ctx context.Context, loaded fleetconfig.Loaded, ch Channel) error {
	var result error
	result = (&loaded).UseForShipProof(func(fleetID, shipID, credentialID string, secret []byte) error {
		client, err := NewClient(ClientConfig{FleetID: fleetID, ServiceIdentity: loaded.Config.Service, CredentialID: credentialID, Secret: string(secret), ShipID: shipID, IOTimeout: 10 * time.Second})
		if err != nil {
			return errors.New("qualifier_client_invalid")
		}
		_, err = client.Qualify(ctx, ch)
		if err != nil {
			return errors.New("qualifier_handshake_failed")
		}
		return nil
	})
	return result
}

// QualifyProfile requires the protected ship and Commander scope records in
// addition to the public config/trust/ship-proof binding.
func QualifyProfile(ctx context.Context, profile fleetconfig.Profile) error {
	if profile.Ship.FleetID != profile.Config.FleetID || profile.Ship.ShipID != profile.Config.ShipID || profile.Commander.FleetID != profile.Config.FleetID || profile.Commander.ShipID != profile.Config.ShipID || profile.Commander.Capability != fleetidentity.CommanderDelegateCapability {
		return errors.New("qualifier_profile_mismatch")
	}
	return QualifyProduction(ctx, profile.Loaded)
}

// QualifyProduction uses the real protected binding, TLS dialer, WebSocket
// transport, and Client handshake. It never enters M7/M3 lifecycle work.
func QualifyProduction(ctx context.Context, loaded fleetconfig.Loaded) error {
	tlsConfig, err := BuildQualifierTLSConfig(loaded)
	if err != nil {
		return err
	}
	u, err := url.Parse(loaded.Config.FleetURL)
	if err != nil || u.Scheme != "wss" || u.Path != "/api/fleet/v1/tunnel" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() != loaded.Config.FleetDNS {
		return errors.New("qualifier_endpoint_invalid")
	}
	dialer := websocket.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: 10 * time.Second, EnableCompression: false}
	return (&loaded).UseForShipProof(func(fleetID, shipID, credentialID string, secret []byte) error {
		conn, _, dialErr := dialer.DialContext(ctx, u.String(), nil)
		if dialErr != nil {
			return errors.New("qualifier_tls_or_endpoint_failed")
		}
		defer conn.Close()
		client, clientErr := NewClient(ClientConfig{FleetID: fleetID, ServiceIdentity: loaded.Config.Service, CredentialID: credentialID, Secret: string(secret), ShipID: shipID, IOTimeout: 10 * time.Second})
		if clientErr != nil {
			return errors.New("qualifier_client_invalid")
		}
		if _, clientErr = client.Qualify(ctx, &websocketChannel{c: conn, peer: loaded.Config.Service}); clientErr != nil {
			return errors.New("qualifier_handshake_failed")
		}
		return nil
	})
}

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
	return runProductionLocalConnectedUpdates(ctx, projectRoot, identityDir, states, resume, events, updates, connected, nil)
}

// RunProductionLocalConnectedUpdatesCommander is the additive production
// outbound seam for the negotiated M3 carrier. The callback runs only after
// the existing M7 authentication and capability acceptance; it cannot create
// a listener or alter M7 observation frames.
func RunProductionLocalConnectedUpdatesCommander(ctx context.Context, projectRoot, identityDir string, states []fleetobserve.LocalPersonaState, resume Resume, events []fleetobserve.LocalEvent, updates <-chan []fleetobserve.LocalPersonaState, connected func(context.Context, fleetidentity.ShipState, uint64) (func(), error), commander func(context.Context, Channel, uint64) (CommanderStep, error)) (string, uint64, error) {
	return runProductionLocalConnectedUpdates(ctx, projectRoot, identityDir, states, resume, events, updates, connected, commander)
}

func runProductionLocalConnectedUpdates(ctx context.Context, projectRoot, identityDir string, states []fleetobserve.LocalPersonaState, resume Resume, events []fleetobserve.LocalEvent, updates <-chan []fleetobserve.LocalPersonaState, connected func(context.Context, fleetidentity.ShipState, uint64) (func(), error), commander func(context.Context, Channel, uint64) (CommanderStep, error)) (string, uint64, error) {
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
	client, err := NewClient(ClientConfig{FleetID: identity.FleetID, ServiceIdentity: identity.FleetServiceIdentity, CredentialID: identity.CredentialID, Secret: identity.CredentialSecret, ShipID: identity.ShipID, IOTimeout: 10 * time.Second, Connected: hook, CommanderStep: commander})
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
