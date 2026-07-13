package fleetobserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
)

type lifecycleChannel struct {
	in, out     chan []byte
	closed      chan struct{}
	closeOnce   *sync.Once
	blockClose  bool
	closeIntent chan struct{}
	intentOnce  sync.Once
}

func lifecyclePair() (*lifecycleChannel, *lifecycleChannel) {
	a, b := make(chan []byte, 8), make(chan []byte, 8)
	closed := make(chan struct{})
	once := new(sync.Once)
	intent := make(chan struct{})
	return &lifecycleChannel{in: b, out: a, closed: closed, closeOnce: once, blockClose: true, closeIntent: intent}, &lifecycleChannel{in: a, out: b, closed: closed, closeOnce: once}
}

func (c *lifecycleChannel) Send(ctx context.Context, b []byte) error {
	if c.blockClose && bytes.Contains(b, []byte(`"type":"close"`)) {
		c.intentOnce.Do(func() { close(c.closeIntent) })
		select {
		case <-c.closed:
			return context.Canceled
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case c.out <- append([]byte(nil), b...):
		return nil
	case <-c.closed:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *lifecycleChannel) Receive(ctx context.Context) ([]byte, error) {
	select {
	case b := <-c.in:
		return append([]byte(nil), b...), nil
	case <-c.closed:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *lifecycleChannel) PeerServiceIdentity() string { return "fleet-service" }
func (c *lifecycleChannel) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestProductionRestartUsesOneDurableRegistryForObserverAndTunnel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	clock := &fakeClock{now: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	open := func(seed byte) *Production {
		p, err := OpenProduction(ProductionConfig{AuthorityStore: dir, FleetID: "flt_0123456789abcdef", FleetEpoch: "epc_0123456789abcdef", ServiceIdentity: "fleet-service", IdentityClock: clock, ObserveClock: clock, TunnelClock: clock, Random: bytes.NewReader(bytes.Repeat([]byte{seed}, 4096))})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := open(1)
	artifact, err := a.Registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := a.Registry.Enroll(artifact.ArtifactID, artifact.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	observer, err := a.Registry.IssueObserver([]string{ship.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	b := open(2)
	if b.Observer.identities != b.Registry || b.Tunnel == nil {
		t.Fatal("production services do not share durable startup")
	}
	if _, err = b.Registry.AuthenticateObserver(observer.CredentialID, observer.Secret); err != nil {
		t.Fatalf("observer authority lost on restart: %v", err)
	}
	if _, err = b.Registry.AuthenticateShip(ship.Credential.CredentialID, ship.Credential.Secret); err != nil {
		t.Fatalf("tunnel authority lost on restart: %v", err)
	}
}

func TestProductionServeClosesTunnelsOnEveryReturnPath(t *testing.T) {
	listenErr := errors.New("listen failed")
	shutdownErr := errors.New("shutdown failed")
	tests := []struct {
		name     string
		listen   func(context.Context) func() error
		shutdown func(context.Context) error
		want     error
	}{
		{"listen error", func(context.Context) func() error { return func() error { return listenErr } }, func(context.Context) error { return nil }, listenErr},
		{"server closed", func(context.Context) func() error { return func() error { return http.ErrServerClosed } }, func(context.Context) error { return nil }, nil},
		{"context cancellation", func(ctx context.Context) func() error {
			return func() error { <-ctx.Done(); return http.ErrServerClosed }
		}, func(context.Context) error { return nil }, nil},
		{"shutdown error", func(ctx context.Context) func() error {
			return func() error { <-ctx.Done(); return http.ErrServerClosed }
		}, func(context.Context) error { return shutdownErr }, shutdownErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tt.name == "context cancellation" || tt.name == "shutdown error" {
				cancel()
			} else {
				defer cancel()
			}
			err := serveProduction(ctx, tt.listen(ctx), tt.shutdown)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestProductionServeValidationReturnsClosePreExistingTunnels(t *testing.T) {
	tests := []struct {
		name     string
		server   *http.Server
		certFile string
		keyFile  string
	}{
		{name: "nil server", certFile: "cert.pem", keyFile: "key.pem"},
		{name: "missing certificate", server: &http.Server{}, keyFile: "key.pem"},
		{name: "missing key", server: &http.Server{}, certFile: "cert.pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, client, adapter, clientSide, serverSide := productionWithTunnel(t)
			serverDone := make(chan error, 1)
			clientDone := make(chan error, 1)
			go func() { serverDone <- p.Tunnel.Serve(context.Background(), serverSide) }()
			go func() {
				_, _, err := client.RunLocal(context.Background(), clientSide, adapter, []fleetobserve.LocalPersonaState{{Session: fleetobserve.SessionIdle, Turn: fleetobserve.TurnNone, Activity: fleetobserve.ActivityIdle}}, fleettunnel.Resume{}, nil)
				clientDone <- err
			}()
			select {
			case <-clientSide.closeIntent:
			case <-time.After(2 * time.Second):
				t.Fatal("tunnel did not become active")
			}
			if err := p.Serve(context.Background(), tt.server, tt.certFile, tt.keyFile); err == nil {
				t.Fatal("validation unexpectedly succeeded")
			}
			select {
			case <-serverDone:
			case <-time.After(2 * time.Second):
				t.Fatal("server tunnel remained active")
			}
			select {
			case <-clientDone:
			case <-time.After(2 * time.Second):
				t.Fatal("client tunnel remained active")
			}
		})
	}
	var nilProduction *Production
	if err := nilProduction.Serve(context.Background(), &http.Server{}, "cert.pem", "key.pem"); err == nil {
		t.Fatal("nil production unexpectedly succeeded")
	}
}

func productionWithTunnel(t *testing.T) (*Production, *fleettunnel.Client, *fleetobserve.LocalStateAdapter, *lifecycleChannel, *lifecycleChannel) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	p, err := OpenProduction(ProductionConfig{AuthorityStore: filepath.Join(t.TempDir(), "authority"), FleetID: "flt_0123456789abcdef", FleetEpoch: "epc_0123456789abcdef", ServiceIdentity: "fleet-service", IdentityClock: clock, ObserveClock: clock, TunnelClock: clock, Random: bytes.NewReader(bytes.Repeat([]byte{7}, 4096))})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := p.Registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := p.Registry.Enroll(artifact.ArtifactID, artifact.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	client, err := fleettunnel.NewClient(fleettunnel.ClientConfig{FleetID: enrolled.FleetID, ServiceIdentity: "fleet-service", CredentialID: enrolled.Credential.CredentialID, Secret: enrolled.Credential.Secret, ShipID: enrolled.ShipID, IOTimeout: time.Second, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.MkdirAll(filepath.Join(root, ".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".codex", "agents", "backend.toml"), []byte("name='backend'\ndeveloper_instructions='Test backend.'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter, err := fleetobserve.OpenLocalStateAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := lifecyclePair()
	return p, client, adapter, clientSide, serverSide
}
