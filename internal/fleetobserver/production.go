package fleetobserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/fleetsteer"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
)

// ProductionConfig is the single Fleet startup boundary. AuthorityStore must
// be outside a project checkout and is always opened durably.
type ProductionConfig struct {
	AuthorityStore, FleetID, FleetEpoch, ServiceIdentity string
	IdentityClock                                        fleetidentity.Clock
	ObserveClock                                         fleetobserve.Clock
	TunnelClock                                          fleettunnel.Clock
	Random                                               io.Reader
	SteerEpoch                                           uint64
	InterruptClock                                       fleetinterrupt.Clock
	InterruptAdmissionClock                              fleetinterrupt.Clock
	InterruptTargetLifetime                              time.Duration
	InterruptObservationFreshness                        time.Duration
	InterruptSubjectAdmissionWindow                      time.Duration
	InterruptShipAdmissionWindow                         time.Duration
	InterruptTargetAdmissionWindow                       time.Duration
}

func (p *Production) Handler() http.Handler {
	m := http.NewServeMux()
	m.Handle("/api/fleet/v1/tunnel", p.Tunnel.WebSocketHandler())
	if p.Steer != nil {
		m.Handle("/api/fleet/v1/steer-tunnel", fleetsteer.ReverseHandler(p.Steer, p.Registry, p.Interrupt))
		m.Handle("/api/fleet/v1/steer-targets", fleetsteer.HTTPHandler{Service: p.Steer})
		m.Handle("/api/fleet/v1/turn-steers", fleetsteer.HTTPHandler{Service: p.Steer})
		m.Handle("/steer/", fleetsteer.ProductHandler{API: fleetsteer.HTTPHandler{Service: p.Steer}})
	}
	if p.Interrupt != nil {
		m.Handle("/api/fleet/v1/interrupt-targets", fleetinterrupt.HTTPHandler{Service: p.Interrupt})
		m.Handle("/api/fleet/v1/turn-interrupts", fleetinterrupt.HTTPHandler{Service: p.Interrupt})
		m.Handle("/api/fleet/v1/turn-interrupts/", fleetinterrupt.HTTPHandler{Service: p.Interrupt})
		m.Handle("/interrupt/", fleetinterrupt.ProductHandler{API: fleetinterrupt.HTTPHandler{Service: p.Interrupt}})
	}
	m.Handle("/", p.Observer.ProductHandler())
	return m
}

// Serve owns the production listener lifecycle and performs bounded graceful
// shutdown when the parent context is cancelled.
func (p *Production) Serve(ctx context.Context, server *http.Server, certFile, keyFile string) error {
	if p != nil && p.Tunnel != nil {
		defer p.Tunnel.CloseAll()
	}
	if p == nil || server == nil || certFile == "" || keyFile == "" {
		return errors.New("invalid_input")
	}
	server.Handler = p.Handler()
	return serveProduction(ctx, func() error { return server.ListenAndServeTLS(certFile, keyFile) }, server.Shutdown)
}

func serveProduction(ctx context.Context, listen func() error, shutdown func(context.Context) error) error {
	done := make(chan error, 1)
	go func() { done <- listen() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Production holds the three observer-only services that must share one
// durable authority registry. Keeping construction here prevents production
// startup from accidentally authenticating with an ephemeral registry.
type Production struct {
	Registry   *fleetidentity.Registry
	Projection *fleetobserve.Projection
	Tunnel     *fleettunnel.Server
	Observer   *Service
	Steer      *fleetsteer.Service
	Interrupt  *fleetinterrupt.Service
}

// ConnectSteerEndpoint is the typed production composition seam used by the
// authenticated ship transport. It cannot accept an observer projection or a
// generic tunnel channel.
func (p *Production) ConnectSteerEndpoint(shipID string, generation uint64, endpoint fleetsteer.Endpoint) (func(), error) {
	if p == nil || p.Steer == nil {
		return nil, errors.New("steer_unavailable")
	}
	return p.Steer.Connect(shipID, generation, endpoint)
}

func OpenProduction(c ProductionConfig) (*Production, error) {
	r, err := fleetidentity.OpenRegistry(c.AuthorityStore, c.FleetID, c.IdentityClock, c.Random)
	if err != nil {
		return nil, err
	}
	p, err := fleetobserve.New(fleetobserve.Config{FleetID: c.FleetID, FleetEpoch: c.FleetEpoch, MaxShips: 4096, MaxPersonas: 256, MaxSnapshotBytes: 1 << 20, MaxEventBytes: 16 << 10, PerShipIngress: 256, ReplayCapacity: 8192, MaxSubscribers: 128, MaxPageSize: 256, MaxTerminalMetadata: 256, LeaseDuration: time.Minute, StaleRetention: 24 * time.Hour, Clock: c.ObserveClock})
	if err != nil {
		return nil, err
	}
	t, err := fleettunnel.NewServer(fleettunnel.ServerConfig{FleetID: c.FleetID, ServiceIdentity: c.ServiceIdentity, HandshakeTTL: 30 * time.Second, LeaseDuration: time.Minute, IOTimeout: 10 * time.Second, Clock: c.TunnelClock, Random: c.Random}, r, p)
	if err != nil {
		return nil, err
	}
	o, err := New(r, p)
	if err != nil {
		return nil, err
	}
	var steer *fleetsteer.Service
	var interrupt *fleetinterrupt.Service
	if c.SteerEpoch != 0 {
		steer, err = fleetsteer.NewService(c.FleetID, r, nil, c.Random)
		if err != nil {
			return nil, err
		}
		if err = steer.BindProjection(p, c.SteerEpoch); err != nil {
			return nil, err
		}
		interrupt, err = fleetinterrupt.OpenServiceWithConfig(c.FleetID, r, c.Random, filepath.Join(c.AuthorityStore, "interrupt"), fleetinterrupt.ServiceConfig{Clock: c.InterruptClock, AdmissionClock: c.InterruptAdmissionClock, TargetLifetime: c.InterruptTargetLifetime, ObservationFreshness: c.InterruptObservationFreshness, SubjectAdmissionWindow: c.InterruptSubjectAdmissionWindow, ShipAdmissionWindow: c.InterruptShipAdmissionWindow, TargetAdmissionWindow: c.InterruptTargetAdmissionWindow})
		if err != nil {
			return nil, err
		}
		if err = interrupt.BindProjection(p, c.SteerEpoch); err != nil {
			return nil, err
		}
	}
	return &Production{Registry: r, Projection: p, Tunnel: t, Observer: o, Steer: steer, Interrupt: interrupt}, nil
}
