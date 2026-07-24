//go:build unix

package commands

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/fleetobserver"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/server"
)

func TestM9FakeCodexAppServer(t *testing.T) {
	// The environment bit is deliberately insufficient: an inherited or
	// externally-set value must never turn the parent test process into the
	// helper and give this fixture authority to terminate it.
	if os.Getenv("SHIPMATES_M9_FAKE") != "1" || !m9HelperProcessArg(os.Args) {
		return
	}
	s := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, value any) {
		b, _ := json.Marshal(map[string]any{"id": json.RawMessage(id), "result": value})
		fmt.Println(string(b))
	}
	protocolError := func(id json.RawMessage, code int, message string) {
		b, _ := json.Marshal(map[string]any{"id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
		fmt.Println(string(b))
	}
	record := func(value string) error {
		f, err := os.OpenFile(os.Getenv("SHIPMATES_M9_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(f, value); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	for s.Scan() {
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result struct {
				Decision string `json:"decision"`
			} `json:"result"`
		}
		if err := json.Unmarshal(s.Bytes(), &q); err != nil {
			_ = record("unexpected:invalid_json")
			protocolError(json.RawMessage("null"), -32700, "parse error")
			continue
		}
		switch q.Method {
		case "initialize":
			reply(q.ID, map[string]any{"userAgent": "codex-cli 0.144.1"})
		case "initialized":
		case "thread/start":
			reply(q.ID, map[string]any{"thread": map[string]string{"id": "thread-m9-e2e"}})
		case "turn/start":
			reply(q.ID, map[string]any{"turn": map[string]string{"id": "turn-m9-e2e"}})
			trigger := os.Getenv("SHIPMATES_M9_APPROVAL_TRIGGER")
			deadline := time.Now().Add(5 * time.Second)
			for trigger != "" && time.Now().Before(deadline) {
				if _, err := os.Stat(trigger); err == nil {
					fmt.Println(`{"id":91,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-m9-e2e","turnId":"turn-m9-e2e","command":"printf M9_PENDING_APPROVAL"}}`)
					break
				}
				time.Sleep(time.Millisecond)
			}
		case "turn/interrupt":
			if err := record("interrupt"); err != nil {
				protocolError(q.ID, -32603, "fixture record unavailable")
				continue
			}
			reply(q.ID, map[string]any{"accepted": true})
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"thread-m9-e2e","turn":{"id":"turn-m9-e2e"}}}`)
		case "":
			if string(q.ID) == "91" && q.Result.Decision == "cancel" {
				_ = record("approval_cancel")
				continue
			}
			_ = record("unexpected:response")
		default:
			_ = record("unexpected:" + q.Method)
			protocolError(q.ID, -32601, "method not found")
		}
	}
}

func m9HelperProcessArg(args []string) bool {
	for _, arg := range args {
		if arg == "shipmates-m9-fake-app-server" {
			return true
		}
	}
	return false
}

func TestM9FakeCodexAppServerEnvironmentCannotActivateParent(t *testing.T) {
	t.Setenv("SHIPMATES_M9_FAKE", "1")
	if m9HelperProcessArg(os.Args) {
		t.Fatal("parent test process unexpectedly carries fake app-server marker")
	}
	// This must return without reading stdin or terminating the process.
	TestM9FakeCodexAppServer(t)
}

func TestProductionFleetShipRemoteInterruptVerticalSlice(t *testing.T) {
	m11InstallHostileRuntimeGuard(t)
	productionClock := &m9Clock{now: time.Now()}
	root := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	for _, d := range []string{".codex/agents", ".shipmates/policies", ".shipmates/sessions"} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRender("codex", "backend", renderCodex(frontmatter{Name: "backend", Description: "Production interrupt E2E fixture"}, "Exercise the production remote-interrupt lifecycle.")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".shipmates/policy.yaml", ".shipmates/policies/backend.yaml"} {
		if err := os.WriteFile(p, []byte(emptyStrictPolicy), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	authority := filepath.Join(t.TempDir(), "authority")
	registry, err := fleetidentity.OpenRegistry(authority, "flt_0123456789abcdef", nil, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := registry.Enroll(enrollment.ArtifactID, enrollment.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := registry.IssueOperatorCapability("sub_0123456789abcdef", []string{ship.ShipID}, fleetidentity.InterruptTurnCapability, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := registry.IssueObserver([]string{ship.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	steerOnly, err := registry.IssueOperator("sub_steeronly0123456", []string{ship.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	staleInterrupt, err := registry.IssueOperatorCapability("sub_stalegen0123456", []string{ship.ShipID}, fleetidentity.InterruptTurnCapability, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	staleSuccessor, err := registry.RotateOperator(staleInterrupt.Record.CredentialID, staleInterrupt.Record.CredentialGeneration, time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CommitOperatorRotation(staleSuccessor.Record.CredentialID, staleSuccessor.Record.CredentialGeneration); err != nil {
		t.Fatal(err)
	}
	if err := registry.RevokeOperatorCredential(staleSuccessor.Record.CredentialID, staleSuccessor.Record.CredentialGeneration); err != nil {
		t.Fatal(err)
	}
	identityDir := filepath.Join(t.TempDir(), "identity")
	if err := os.MkdirAll(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(t.TempDir(), "interrupt.credential")
	credential := operator.Record.CredentialID + "." + operator.Secret
	if err := os.WriteFile(credPath, []byte(credential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cert, key, roots := m9TLSFixture(t)
	t.Setenv("SSL_CERT_FILE", cert)
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.TLSClientConfig.RootCAs = roots
	oldHTTP := http.DefaultTransport
	http.DefaultTransport = baseTransport
	defer func() { http.DefaultTransport = oldHTTP }()
	oldDialer := websocket.DefaultDialer
	dialerCopy := *oldDialer
	dialerCopy.TLSClientConfig = baseTransport.TLSClientConfig.Clone()
	// Gorilla's websocket client performs an HTTP/1.1 Upgrade. Keep the
	// fixture's private CA verification while preventing ALPN from selecting
	// HTTP/2, which would send the upgrade request as a bogus connection
	// greeting to the HTTP/2 server.
	dialerCopy.TLSClientConfig.NextProtos = []string{"http/1.1"}
	websocket.DefaultDialer = &dialerCopy
	defer func() { websocket.DefaultDialer = oldDialer }()
	addr := m9FreeAddress(t)
	fleetURL := "https://" + addr
	if host, _, e := net.SplitHostPort(addr); e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("fleet TLS hostname=%q err=%v; certificate is pinned to 127.0.0.1", addr, e)
	}
	if err := fleetidentity.StoreShipState(identityDir, fleetidentity.ShipState{SchemaVersion: 1, FleetID: ship.FleetID, ShipID: ship.ShipID, FleetDestination: fleetURL, FleetServiceIdentity: "fleet-service", CredentialID: ship.Credential.CredentialID, CredentialSecret: ship.Credential.Secret}); err != nil {
		t.Fatal(err)
	}

	fleetCtx, stopFleet := context.WithCancel(context.Background())
	fleetDone := make(chan error, 1)
	startFleet := func(ctx context.Context, done chan<- error) {
		cmd := fleetWithProductionConfig(fleetobserver.ProductionConfig{InterruptClock: productionClock, InterruptAdmissionClock: m9WallClock{}, InterruptTargetLifetime: 500 * time.Millisecond})
		done <- cmd.Run(ctx, []string{"fleet", "serve-observer", "--addr", addr, "--authority-store", authority, "--fleet-id", ship.FleetID, "--fleet-epoch", "epc_0123456789abcdef", "--steer-epoch", "9", "--service-identity", "fleet-service", "--tls-cert", cert, "--tls-key", key, "--interrupt-ship-admission-window", "1ns"})
	}
	go startFleet(fleetCtx, fleetDone)
	m9WaitFleet(t, fleetURL, fleetDone)

	record := filepath.Join(t.TempDir(), "interrupts")
	approvalTrigger := filepath.Join(t.TempDir(), "release-approval")
	srv := server.NewWithCodexOptions(codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestM9FakeCodexAppServer$", "--", "shipmates-m9-fake-app-server"}, Environment: map[string]string{"SHIPMATES_M9_FAKE": "1", "SHIPMATES_M9_RECORD": record, "SHIPMATES_M9_APPROVAL_TRIGGER": approvalTrigger}, StartupTimeout: time.Second, ShutdownTimeout: time.Second})
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Run(serverCtx) }()
	defer func() {
		stopServer()
		select {
		case <-serverDone:
		case <-time.After(3 * time.Second):
			t.Error("local server cleanup timeout")
		}
	}()
	select {
	case <-srv.Ready():
	case e := <-serverDone:
		t.Fatalf("local server startup: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatal("local server readiness timeout")
	}
	var liveOut synchronizedBuffer
	live := Live()
	live.Writer = &liveOut
	live.ErrWriter = &synchronizedBuffer{}
	if err := live.Run(context.Background(), []string{"live", "backend", "work until interrupted"}); err != nil {
		t.Fatalf("public live: %v", err)
	}
	var active livesession.Snapshot
	if err := json.Unmarshal([]byte(liveOut.String()), &active); err != nil || active.TurnID != "turn-m9-e2e" {
		t.Fatalf("live snapshot=%q err=%v", liveOut.String(), err)
	}
	controller, err := dashboard.Connect(context.Background(), dashboard.HTTPTransport{}, "backend", dashboard.AttachRequest{SessionID: active.SessionID})
	if err != nil {
		t.Fatalf("public controller attach: %v", err)
	}
	defer func() { _ = controller.Close(context.Background()) }()
	if err = os.WriteFile(approvalTrigger, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pending livesession.Attach
	approvalDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(approvalDeadline) {
		pending, err = controller.Sync(context.Background(), 0)
		if err == nil && pending.PendingApproval != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pending.PendingApproval == nil || pending.PendingApproval.Authority.TurnID != active.TurnID || pending.PendingApproval.Authority.ThreadID != active.ThreadID {
		t.Fatalf("exact live-turn approval not pending: %+v err=%v", pending.PendingApproval, err)
	}

	observeCtx, stopObserve := context.WithCancel(context.Background())
	observeDone := make(chan error, 1)
	observeReady := make(chan struct{})
	shipCmd := shipWithProductionReadiness(productionClock, observeReady)
	shipCmd.Writer = &synchronizedBuffer{}
	shipCmd.ErrWriter = &synchronizedBuffer{}
	go func() {
		observeDone <- shipCmd.Run(observeCtx, []string{"ship", "observe", "--project", root, "--identity-store", identityDir, "--steer-epoch", "9"})
	}()
	select {
	case <-observeReady:
	case e := <-observeDone:
		t.Fatalf("ship observe exited before reverse readiness: %v", e)
	case <-time.After(5 * time.Second):
		t.Fatal("ship observe/reverse readiness timeout")
	}

	interruptClient := fleetinterrupt.Client{BaseURL: fleetURL, Credential: credential}
	observerClient := fleetobserver.Client{BaseURL: fleetURL, Credential: observer.CredentialID + "." + observer.Secret}
	var target fleetinterrupt.InterruptTargetV1
	var lastTargetsErr error
	var lastFleet fleetobserver.ShipsV1
	var lastFleetErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		targets, e := interruptClient.Targets(context.Background())
		lastTargetsErr = e
		if e == nil && len(targets.Targets) == 1 {
			target = targets.Targets[0]
			break
		}
		lastFleet, lastFleetErr = observerClient.Ships(context.Background())
		select {
		case e := <-observeDone:
			t.Fatalf("ship observe exited: %v", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if target.InterruptTargetRef == "" || target.Persona == "" || target.Persona == "backend" || target.ShipID != ship.ShipID {
		t.Fatalf("authorized target discovery=%+v last_targets_error=%v fleet_projection=%+v fleet_projection_error=%v", target, lastTargetsErr, lastFleet, lastFleetErr)
	}

	credentialCases := []struct {
		name       string
		credential string
	}{
		{name: "observer credential", credential: observer.CredentialID + "." + observer.Secret},
		{name: "ship credential", credential: ship.Credential.CredentialID + "." + ship.Credential.Secret},
		{name: "steer-only operator", credential: steerOnly.Record.CredentialID + "." + steerOnly.Secret},
		{name: "stale interrupt credential generation", credential: staleInterrupt.Record.CredentialID + "." + staleInterrupt.Secret},
		{name: "revoked predecessor with successor secret", credential: staleInterrupt.Record.CredentialID + "." + staleSuccessor.Secret},
		{name: "revoked successor with predecessor secret", credential: staleSuccessor.Record.CredentialID + "." + staleInterrupt.Secret},
		{name: "revoked successor", credential: staleSuccessor.Record.CredentialID + "." + staleSuccessor.Secret},
	}
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			m9AssertCannotDiscover(t, fleetURL, tc.credential, record)
		})
	}

	validSubmit := func(tgt fleetinterrupt.InterruptTargetV1) fleetinterrupt.SubmitV1 {
		t.Helper()
		op, e := fleetinterrupt.NewOperationID()
		if e != nil {
			t.Fatal(e)
		}
		return fleetinterrupt.SubmitV1{SchemaVersion: 1, FleetID: tgt.FleetID, FleetEpoch: tgt.FleetEpoch, ShipID: tgt.ShipID, ConnectionGeneration: tgt.ConnectionGeneration, Persona: tgt.Persona, InterruptTargetRef: tgt.InterruptTargetRef, OperationID: op}
	}
	freshTarget := func() fleetinterrupt.InterruptTargetV1 {
		t.Helper()
		fresh, e := interruptClient.Targets(context.Background())
		if e != nil || len(fresh.Targets) != 1 {
			t.Fatalf("fresh target discovery=%+v err=%v", fresh, e)
		}
		return fresh.Targets[0]
	}
	wrongShip := validSubmit(target)
	wrongShip.ShipID = "shp_ffffffffffffffff"
	m9AssertSubmitRefused(t, interruptClient, wrongShip, "unauthorized", record)
	target = freshTarget()
	wrongGeneration := validSubmit(target)
	wrongGeneration.ConnectionGeneration++
	m9AssertSubmitRefused(t, interruptClient, wrongGeneration, "stale_generation", record)
	target = freshTarget()
	wrongPersona := validSubmit(target)
	wrongPersona.Persona = fleetobserve.OpaquePersonaReference(ship.ShipID, 1)
	m9AssertSubmitRefused(t, interruptClient, wrongPersona, "stale_target", record)
	target = freshTarget()
	wrongTarget := validSubmit(target)
	wrongTarget.InterruptTargetRef, err = fleetinterrupt.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	m9AssertSubmitRefused(t, interruptClient, wrongTarget, "stale_target", record)
	productionClock.AdvanceTo(target.ExpiresAt.Add(time.Nanosecond))
	m9AssertSubmitRefused(t, interruptClient, validSubmit(target), "target_expired", record)

	// Expired projections are never reused for the happy path. Discovering a
	// fresh target also installs a fresh exact private tuple on the ship.
	target = freshTarget()

	var firstOut synchronizedBuffer
	firstCommand := Fleet()
	firstCommand.Writer = &firstOut
	firstCommand.ErrWriter = &synchronizedBuffer{}
	err = firstCommand.Run(context.Background(), []string{"fleet", "interrupt", "--fleet", fleetURL, "--credential-file", credPath, "--fleet-id", target.FleetID, "--fleet-epoch", fmt.Sprint(target.FleetEpoch), "--ship", target.ShipID, "--connection-generation", fmt.Sprint(target.ConnectionGeneration), "--persona", target.Persona, "--target", target.InterruptTargetRef, "--confirm", "--json"})
	var first livesession.RemoteInterruptResult
	if decodeErr := json.Unmarshal([]byte(firstOut.String()), &first); err != nil || decodeErr != nil || first.Outcome != livesession.RemoteInterruptInterrupted || first.OperationID == "" {
		t.Fatalf("public interrupt result=%q parsed=%+v err=%v decode=%v", firstOut.String(), first, err, decodeErr)
	}
	if got := m9InterruptCount(t, record); got != 1 {
		t.Fatalf("exact adapter interrupt calls=%d", got)
	}
	m9AssertApprovalCancelledBeforeInterrupt(t, record)
	resolved, err := controller.Sync(context.Background(), 0)
	if err != nil {
		t.Fatalf("public controller sync after interrupt: %v", err)
	}
	if resolved.PendingApproval != nil {
		t.Fatalf("approval remains pending after remote interrupt: %+v", resolved.PendingApproval)
	}
	foundResolution := false
	for _, event := range resolved.Events {
		if event.Kind == "approval.resolved" && event.TurnID == active.TurnID && event.Data["outcome"] == "denied" && event.Data["reason_code"] == "remote_interrupt" {
			foundResolution = true
		}
	}
	if !foundResolution {
		t.Fatalf("server-owned approval denial not recorded: %+v", resolved.Events)
	}

	// Exercise the public command only through the already-terminal replay. A
	// refusal remains visible in the direct assertion above and cannot invoke
	// the CLI ExitCoder handling in the parent go test process.
	var replayOut synchronizedBuffer
	replay := Fleet()
	replay.Writer = &replayOut
	replay.ErrWriter = &synchronizedBuffer{}
	if err := replay.Run(context.Background(), []string{"fleet", "interrupt", "--fleet", fleetURL, "--credential-file", credPath, "--retry-operation", first.OperationID, "--json"}); err != nil {
		t.Fatalf("operation replay query: %v", err)
	}
	var replayed livesession.RemoteInterruptResult
	if err := json.Unmarshal([]byte(replayOut.String()), &replayed); err != nil || replayed != first {
		t.Fatalf("replay=%q parsed=%+v want=%+v err=%v", replayOut.String(), replayed, first, err)
	}
	if got := m9InterruptCount(t, record); got != 1 {
		t.Fatalf("replay invoked adapter %d times", got)
	}

	stopObserve()
	select {
	case <-observeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ship observe cleanup timeout")
	}
	stopFleet()
	select {
	case e := <-fleetDone:
		if e != nil {
			t.Fatalf("fleet shutdown: %v", e)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("fleet shutdown timeout")
	}
	fleetCtx2, stopFleet2 := context.WithCancel(context.Background())
	fleetDone2 := make(chan error, 1)
	go startFleet(fleetCtx2, fleetDone2)
	m9WaitFleet(t, fleetURL, fleetDone2)
	defer func() {
		stopFleet2()
		select {
		case <-fleetDone2:
		case <-time.After(12 * time.Second):
			t.Error("restarted fleet cleanup timeout")
		}
	}()
	var restartOut synchronizedBuffer
	restart := Fleet()
	restart.Writer = &restartOut
	restart.ErrWriter = &synchronizedBuffer{}
	if err := restart.Run(context.Background(), []string{"fleet", "interrupt", "--fleet", fleetURL, "--credential-file", credPath, "--retry-operation", first.OperationID, "--json"}); err != nil {
		t.Fatalf("restart query: %v", err)
	}
	var durable livesession.RemoteInterruptResult
	if err := json.Unmarshal([]byte(restartOut.String()), &durable); err != nil || durable.Outcome != livesession.RemoteInterruptInterrupted || durable.ReasonCode != "interrupted" || durable.RetryDisposition != livesession.RemoteInterruptNoRetry {
		t.Fatalf("durable restart classification=%q parsed=%+v err=%v", restartOut.String(), durable, err)
	}
	if got := m9InterruptCount(t, record); got != 1 {
		t.Fatalf("restart query invoked adapter %d times", got)
	}
}

type m9Clock struct {
	mu  sync.Mutex
	now time.Time
}

type m9WallClock struct{}

func (m9WallClock) Now() time.Time { return time.Now() }

func (c *m9Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *m9Clock) AdvanceTo(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.After(c.now) {
		c.now = now
	}
}

func m9FreeAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}
func m9WaitFleet(t *testing.T, base string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, e := http.Get(base + "/")
		if e == nil {
			r.Body.Close()
			return
		}
		select {
		case e := <-done:
			t.Fatalf("fleet exited before readiness: %v", e)
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("fleet readiness timeout")
}
func m9InterruptCount(t *testing.T, path string) int {
	t.Helper()
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return 0
	}
	if e != nil {
		t.Fatal(e)
	}
	return strings.Count(string(b), "interrupt\n")
}

func m9AssertApprovalCancelledBeforeInterrupt(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(b))
	if len(lines) != 2 || lines[0] != "approval_cancel" || lines[1] != "interrupt" {
		t.Fatalf("M6/M3 call order=%q, want approval_cancel then interrupt", string(b))
	}
}

func m9AssertCannotDiscover(t *testing.T, fleetURL, credential, record string) {
	t.Helper()
	got, err := (fleetinterrupt.Client{BaseURL: fleetURL, Credential: credential}).Targets(context.Background())
	if err == nil {
		t.Fatalf("unauthorized credential discovered targets: %+v", got)
	}
	if count := m9InterruptCount(t, record); count != 0 {
		t.Fatalf("unauthorized discovery invoked adapter %d times", count)
	}
}

func m9AssertSubmitRefused(t *testing.T, client fleetinterrupt.Client, in fleetinterrupt.SubmitV1, reason, record string) {
	t.Helper()
	got, err := client.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("negative submit transport error: %v", err)
	}
	if got.Outcome != livesession.RemoteInterruptRefused || got.ReasonCode != reason {
		t.Fatalf("negative submit=%+v want refused/%s", got, reason)
	}
	if count := m9InterruptCount(t, record); count != 0 {
		t.Fatalf("negative submit invoked adapter %d times", count)
	}
}

func m9TLSFixture(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, e := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if e = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); e != nil {
		t.Fatal(e)
	}
	serverCert, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	if e = serverCert.VerifyHostname("127.0.0.1"); e != nil {
		t.Fatalf("generated Fleet certificate hostname: %v", e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(serverCert)
	return cert, keyPath, pool
}
