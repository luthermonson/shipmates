package fleetsteer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/livesession"
)

const (
	// maxReverseWireBytes bounds one reverse frame on both ends. gorilla's
	// default read limit is unlimited, and this tunnel carries strictly more
	// authority than the observation tunnel, which already caps reads.
	maxReverseWireBytes = 256 << 10
	// reverseIdleTimeout is the read deadline refreshed by traffic and by the
	// keepalive below. Without it a hung peer holds the tunnel and its
	// registered generation open forever.
	reverseIdleTimeout   = 60 * time.Second
	reverseKeepalivePing = 25 * time.Second
)

// strictReverseRead reads exactly one bounded text frame and decodes it with no
// unknown fields and no trailing data. gorilla's ReadJSON accepts both.
func strictReverseRead(c *websocket.Conn, dst any) error {
	kind, data, err := c.ReadMessage()
	if err != nil {
		return err
	}
	if err := c.SetReadDeadline(time.Now().Add(reverseIdleTimeout)); err != nil {
		return err
	}
	if kind != websocket.TextMessage || len(data) == 0 || len(data) > maxReverseWireBytes {
		return errors.New("protocol_violation")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return errors.New("protocol_violation")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("protocol_violation")
	}
	return nil
}

// hardenReverseConn applies the read bound and the idle read deadline that the
// reverse tunnel previously lacked entirely.
func hardenReverseConn(c *websocket.Conn) error {
	c.SetReadLimit(maxReverseWireBytes)
	refresh := func() error { return c.SetReadDeadline(time.Now().Add(reverseIdleTimeout)) }
	c.SetPongHandler(func(string) error { return refresh() })
	previousPing := c.PingHandler()
	c.SetPingHandler(func(appData string) error {
		if err := refresh(); err != nil {
			return err
		}
		return previousPing(appData)
	})
	return refresh()
}

type reverseRequest struct {
	Type          string                          `json:"type"`
	TargetQueryID string                          `json:"target_query_id,omitempty"`
	Delivery      DeliveryV1                      `json:"delivery,omitempty"`
	Interrupt     *fleetinterrupt.DeliveryV1      `json:"interrupt,omitempty"`
	TargetInstall *fleetinterrupt.TargetInstallV1 `json:"target_install,omitempty"`
}
type reverseResult struct {
	Type          string                             `json:"type"`
	OperationID   string                             `json:"operation_id"`
	Result        livesession.RemoteSteerResult      `json:"result"`
	Interrupt     *livesession.RemoteInterruptResult `json:"interrupt,omitempty"`
	Installed     bool                               `json:"installed,omitempty"`
	TargetQueryID string                             `json:"target_query_id,omitempty"`
	Targets       []SteerTargetV1                    `json:"targets,omitempty"`
}
type reverseReady struct {
	Type                string `json:"type"`
	SteerRegistered     bool   `json:"steer_registered"`
	InterruptRegistered bool   `json:"interrupt_registered"`
}

type websocketJSONWriter interface {
	SetWriteDeadline(time.Time) error
	WriteJSON(any) error
}

// reverseWriter is the sole server-side write path for a reverse connection.
// Gorilla permits one concurrent reader and one concurrent writer, but it does
// not permit readiness/control and delivery goroutines to write concurrently.
type reverseWriter struct {
	mu   sync.Mutex
	conn websocketJSONWriter
}

// write never installs a zero deadline. gorilla treats the zero time as "clear
// the deadline", so a peer whose receive window is full would block here
// forever while holding mu, and the delivery goroutine abandoned by Submit
// after DeliveryDeadline would never exit.
func (w *reverseWriter) write(deadline time.Time, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if deadline.IsZero() {
		deadline = time.Now().Add(DeliveryDeadline)
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return w.conn.WriteJSON(value)
}

var reverseDialContext = func(ctx context.Context, urlStr string, requestHeader http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.DialContext(ctx, urlStr, requestHeader)
}

type reverseEndpoint struct {
	stateMu       sync.Mutex
	writer        *reverseWriter
	wait          map[string]chan livesession.RemoteSteerResult
	waitInterrupt map[string]chan livesession.RemoteInterruptResult
	waitInstall   map[string]chan bool
	waitTargets   map[string]chan []SteerTargetV1
	nextQuery     uint64
	registry      *fleetidentity.Registry
	shipID        string
	clock         Clock
}

func (e *reverseEndpoint) PublicTargets(ctx context.Context) ([]SteerTargetV1, error) {
	e.stateMu.Lock()
	e.nextQuery++
	id := "q_" + strconv.FormatUint(e.nextQuery, 10)
	ch := make(chan []SteerTargetV1, 1)
	e.waitTargets[id] = ch
	e.stateMu.Unlock()
	if e.writer.write(time.Now().Add(DeliveryDeadline), reverseRequest{Type: "steer_target_query", TargetQueryID: id}) != nil {
		e.stateMu.Lock()
		delete(e.waitTargets, id)
		e.stateMu.Unlock()
		return nil, errors.New("target_query_failed")
	}
	select {
	case targets := <-ch:
		return targets, nil
	case <-ctx.Done():
		e.stateMu.Lock()
		delete(e.waitTargets, id)
		e.stateMu.Unlock()
		return nil, ctx.Err()
	}
}

func (e *reverseEndpoint) InstallInterruptTarget(ctx context.Context, t fleetinterrupt.TargetInstallV1) error {
	key := t.InterruptTargetRef
	e.stateMu.Lock()
	ch := make(chan bool, 1)
	e.waitInstall[key] = ch
	e.stateMu.Unlock()
	deadline, _ := ctx.Deadline()
	if e.writer.write(deadline, reverseRequest{Type: "interrupt_target_install", TargetInstall: &t}) != nil {
		e.stateMu.Lock()
		delete(e.waitInstall, key)
		e.stateMu.Unlock()
		return errors.New("target_install_failed")
	}
	select {
	case ok := <-ch:
		if !ok {
			return errors.New("target_install_failed")
		}
		return nil
	case <-ctx.Done():
		e.stateMu.Lock()
		delete(e.waitInstall, key)
		e.stateMu.Unlock()
		return ctx.Err()
	}
}

func (e *reverseEndpoint) DeliverInterrupt(ctx context.Context, d fleetinterrupt.DeliveryV1) livesession.RemoteInterruptResult {
	rec, err := e.registry.InspectOperator(d.Operator.CredentialID, d.Operator.CredentialGeneration)
	if err != nil || rec.Revoked || rec.SubjectID != d.Operator.SubjectID || rec.Capability != fleetidentity.InterruptTurnCapability || !e.clock.Now().Before(rec.ExpiresAt) || !contains(rec.ShipIDs, e.shipID) {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: d.Request.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "credential_revoked", RetryDisposition: livesession.RemoteInterruptNoRetry}
	}
	e.stateMu.Lock()
	ch := make(chan livesession.RemoteInterruptResult, 1)
	e.waitInterrupt[d.Request.OperationID] = ch
	e.stateMu.Unlock()
	deadline, _ := ctx.Deadline()
	if e.writer.write(deadline, reverseRequest{Type: "turn_interrupt", Interrupt: &d}) != nil {
		e.stateMu.Lock()
		delete(e.waitInterrupt, d.Request.OperationID)
		e.stateMu.Unlock()
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: d.Request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "delivery_unknown", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
	select {
	case out := <-ch:
		return out
	case <-ctx.Done():
		e.stateMu.Lock()
		delete(e.waitInterrupt, d.Request.OperationID)
		e.stateMu.Unlock()
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: d.Request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "delivery_unknown", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
}

func (e *reverseEndpoint) Deliver(ctx context.Context, d DeliveryV1) livesession.RemoteSteerResult {
	rec, err := e.registry.InspectOperator(d.Operator.CredentialID, d.Operator.CredentialGeneration)
	if err != nil || rec.Revoked || rec.SubjectID != d.Operator.SubjectID || rec.FleetID != d.Operator.FleetID || rec.Capability != livesession.RemoteSteerCapability || !e.clock.Now().Before(rec.ExpiresAt) || !contains(rec.ShipIDs, e.shipID) {
		return result(d.Request.OperationID, "refused", "credential_revoked")
	}
	e.stateMu.Lock()
	ch := make(chan livesession.RemoteSteerResult, 1)
	e.wait[d.Request.OperationID] = ch
	e.stateMu.Unlock()
	deadline, _ := ctx.Deadline()
	if e.writer.write(deadline, reverseRequest{Type: "turn_steer", Delivery: d}) != nil {
		e.stateMu.Lock()
		delete(e.wait, d.Request.OperationID)
		e.stateMu.Unlock()
		return result(d.Request.OperationID, "indeterminate", "delivery_unknown")
	}
	select {
	case out := <-ch:
		return out
	case <-ctx.Done():
		e.stateMu.Lock()
		delete(e.wait, d.Request.OperationID)
		e.stateMu.Unlock()
		return result(d.Request.OperationID, "indeterminate", "delivery_unknown")
	}
}

// ReverseHandler registers a concrete endpoint only for a ship credential
// whose observation connection currently owns the supplied generation.
func ReverseHandler(service *Service, registry *fleetidentity.Registry, interrupts ...*fleetinterrupt.Service) http.Handler {
	u := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil || registry == nil || r.Method != http.MethodGet || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		id, secret, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		p, err := registry.AuthenticateShip(id, secret)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		gen, err := strconv.ParseUint(r.Header.Get("X-Shipmates-Connection-Generation"), 10, 64)
		if err != nil || gen == 0 || registry.ConnectionGeneration(p.ShipID) != gen {
			http.Error(w, "stale_generation", http.StatusConflict)
			return
		}
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Registered immediately: every later failure path must close the
		// hijacked connection, and the interrupt registration failure below
		// previously returned before any close was in place.
		defer c.Close()
		if err = hardenReverseConn(c); err != nil {
			return
		}
		ep := &reverseEndpoint{writer: &reverseWriter{conn: c}, wait: map[string]chan livesession.RemoteSteerResult{}, waitInterrupt: map[string]chan livesession.RemoteInterruptResult{}, waitInstall: map[string]chan bool{}, waitTargets: map[string]chan []SteerTargetV1{}, registry: registry, shipID: p.ShipID, clock: service.clock}
		disconnect, err := service.Connect(p.ShipID, gen, ep)
		if err != nil {
			return
		}
		defer disconnect()
		var disconnectInterrupt func()
		if len(interrupts) != 0 && interrupts[0] != nil {
			disconnectInterrupt, err = interrupts[0].Connect(p.ShipID, gen, ep)
			if err != nil {
				return
			}
			defer disconnectInterrupt()
		}
		stopPing := make(chan struct{})
		defer close(stopPing)
		go reverseKeepalive(c, stopPing)
		if err = ep.writer.write(time.Now().Add(DeliveryDeadline), reverseReady{Type: "reverse_ready", SteerRegistered: true, InterruptRegistered: disconnectInterrupt != nil}); err != nil {
			return
		}
		for {
			var out reverseResult
			if err = strictReverseRead(c, &out); err != nil {
				return
			}
			if out.OperationID == "" {
				return
			}
			ep.stateMu.Lock()
			if out.Type == "interrupt_target_installed" {
				ch := ep.waitInstall[out.OperationID]
				delete(ep.waitInstall, out.OperationID)
				ep.stateMu.Unlock()
				if ch == nil {
					return
				}
				ch <- out.Installed
				continue
			}
			if out.Type == "turn_interrupt_result" && out.Interrupt != nil && out.Interrupt.OperationID == out.OperationID {
				ch := ep.waitInterrupt[out.OperationID]
				delete(ep.waitInterrupt, out.OperationID)
				ep.stateMu.Unlock()
				if ch == nil {
					return
				}
				ch <- *out.Interrupt
				continue
			}
			if out.Type == "steer_target_page" && out.TargetQueryID != "" && out.TargetQueryID == out.OperationID {
				ch := ep.waitTargets[out.TargetQueryID]
				delete(ep.waitTargets, out.TargetQueryID)
				ep.stateMu.Unlock()
				if ch == nil {
					return
				}
				ch <- out.Targets
				continue
			}
			if out.Type != "turn_steer_result" || out.Result.OperationID != out.OperationID {
				ep.stateMu.Unlock()
				return
			}
			ch := ep.wait[out.OperationID]
			delete(ep.wait, out.OperationID)
			ep.stateMu.Unlock()
			if ch == nil {
				return
			}
			ch <- out.Result
		}
	})
}

// reverseKeepalive proves the peer is alive. gorilla's WriteControl is safe to
// call concurrently with the serialized JSON writer, so the keepalive never
// takes the delivery write lock.
func reverseKeepalive(c *websocket.Conn, stop <-chan struct{}) {
	t := time.NewTicker(reverseKeepalivePing)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if c.WriteControl(websocket.PingMessage, nil, time.Now().Add(DeliveryDeadline)) != nil {
				return
			}
		}
	}
}

// RunReverseClient serves the one closed steer operation over an outbound
// ship-initiated websocket until disconnect or cancellation.
func RunReverseClient(ctx context.Context, destination string, identity fleetidentity.ShipState, generation uint64, endpoint Endpoint) error {
	return runReverseClient(ctx, destination, identity, generation, endpoint, nil, false)
}

func RunReverseClientConnected(ctx context.Context, destination string, identity fleetidentity.ShipState, generation uint64, endpoint Endpoint, connected func()) error {
	return runReverseClient(ctx, destination, identity, generation, endpoint, connected, false)
}

// RunReverseClientInterruptReady invokes connected only after Fleet confirms
// both steer and interrupt registration for the exact generation.
func RunReverseClientInterruptReady(ctx context.Context, destination string, identity fleetidentity.ShipState, generation uint64, endpoint Endpoint, connected func()) error {
	return runReverseClient(ctx, destination, identity, generation, endpoint, connected, true)
}

func runReverseClient(ctx context.Context, destination string, identity fleetidentity.ShipState, generation uint64, endpoint Endpoint, connected func(), requireInterrupt bool) error {
	u, err := url.Parse(destination)
	if err != nil || u.Host == "" {
		return errors.New("invalid_fleet_destination")
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else if u.Scheme == "http" && reverseLoopbackHost(u.Hostname()) {
		u.Scheme = "ws"
	} else {
		return errors.New("invalid_fleet_destination")
	}
	u.Path = "/api/fleet/v1/steer-tunnel"
	u.RawQuery = ""
	u.Fragment = ""
	h := http.Header{}
	h.Set("Authorization", "Basic "+basicAuth(identity.CredentialID, identity.CredentialSecret))
	h.Set("X-Shipmates-Connection-Generation", strconv.FormatUint(generation, 10))
	c, _, err := reverseDialContext(ctx, u.String(), h)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := hardenReverseConn(c); err != nil {
		return err
	}
	var ready reverseReady
	if err := strictReverseRead(c, &ready); err != nil || ready.Type != "reverse_ready" || !ready.SteerRegistered || (requireInterrupt && !ready.InterruptRegistered) {
		return errors.New("reverse_handshake_failed")
	}
	if connected != nil {
		connected()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	for {
		var in reverseRequest
		if strictReverseRead(c, &in) != nil {
			return nil
		}
		if in.Type == "turn_interrupt" && in.Interrupt != nil {
			ie, ok := any(endpoint).(fleetinterrupt.Endpoint)
			if !ok {
				return errors.New("protocol_violation")
			}
			dctx, cancel := context.WithTimeout(ctx, fleetinterrupt.DeliveryDeadline)
			out := ie.DeliverInterrupt(dctx, *in.Interrupt)
			cancel()
			_ = c.SetWriteDeadline(time.Now().Add(DeliveryDeadline))
			if c.WriteJSON(reverseResult{Type: "turn_interrupt_result", OperationID: in.Interrupt.Request.OperationID, Interrupt: &out}) != nil {
				return nil
			}
			continue
		}
		if in.Type == "interrupt_target_install" && in.TargetInstall != nil {
			installer, ok := any(endpoint).(fleetinterrupt.TargetInstaller)
			installed := ok && installer.InstallInterruptTarget(ctx, *in.TargetInstall) == nil
			_ = c.SetWriteDeadline(time.Now().Add(DeliveryDeadline))
			if c.WriteJSON(reverseResult{Type: "interrupt_target_installed", OperationID: in.TargetInstall.InterruptTargetRef, Installed: installed}) != nil {
				return nil
			}
			continue
		}
		if in.Type == "steer_target_query" && in.TargetQueryID != "" {
			provider, ok := any(endpoint).(TargetProvider)
			var targets []SteerTargetV1
			if ok {
				targets, _ = provider.PublicTargets(ctx)
			}
			_ = c.SetWriteDeadline(time.Now().Add(DeliveryDeadline))
			if c.WriteJSON(reverseResult{Type: "steer_target_page", OperationID: in.TargetQueryID, TargetQueryID: in.TargetQueryID, Targets: targets}) != nil {
				return nil
			}
			continue
		}
		if in.Type != "turn_steer" {
			return errors.New("protocol_violation")
		}
		dctx, cancel := context.WithTimeout(ctx, DeliveryDeadline)
		out := endpoint.Deliver(dctx, in.Delivery)
		cancel()
		_ = c.SetWriteDeadline(time.Now().Add(DeliveryDeadline))
		if c.WriteJSON(reverseResult{Type: "turn_steer_result", OperationID: in.Delivery.Request.OperationID, Result: out}) != nil {
			return nil
		}
	}
}

func reverseLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func basicAuth(user, pass string) string {
	r := http.Request{Header: make(http.Header)}
	r.SetBasicAuth(user, pass)
	return r.Header.Get("Authorization")[6:]
}
