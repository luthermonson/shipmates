//go:build unix

package commands

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserver"
	"github.com/luthermonson/shipmates/internal/fleetsteer"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/server"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *synchronizedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Len()
}

func TestM8FakeCodexAppServer(t *testing.T) {
	if os.Getenv("SHIPMATES_M8_FAKE") != "1" {
		return
	}
	s := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, v any) {
		b, _ := json.Marshal(map[string]any{"id": json.RawMessage(id), "result": v})
		fmt.Println(string(b))
	}
	for s.Scan() {
		var q struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(s.Bytes(), &q)
		switch q.Method {
		case "initialize":
			reply(q.ID, map[string]any{"userAgent": "codex-cli 0.144.1"})
		case "initialized":
		case "thread/start":
			reply(q.ID, map[string]any{"thread": map[string]string{"id": "thread-e2e"}})
		case "turn/start":
			reply(q.ID, map[string]any{"turn": map[string]string{"id": "turn-e2e"}})
		case "turn/steer":
			f, _ := os.OpenFile(os.Getenv("SHIPMATES_M8_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			fmt.Fprintln(f, "steer")
			f.Close()
			reply(q.ID, map[string]any{"accepted": true})
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"thread-e2e","turn":{"id":"turn-e2e"}}}`)
		case "turn/interrupt":
			reply(q.ID, map[string]any{"accepted": true})
		default:
			os.Exit(3)
		}
	}
}

func TestShipObserveRefusesDivergentDiscoveryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".codex", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".codex", "agents", "backend.toml"), []byte("name = \"backend\"\ndeveloper_instructions = \"Test backend.\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(root, ".shipmates", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	otherRoot, _ := project.CanonicalRoot(other)
	otherScope, _ := project.ScopeID(otherRoot)
	record, _ := json.Marshal(map[string]any{
		"schema_version": 1, "project_root": otherRoot, "project_scope": otherScope,
		"address": "127.0.0.1:1", "pid": os.Getpid(),
		"control_token": strings.Repeat("x", 43),
	})
	if err := os.WriteFile(filepath.Join(sessions, "server.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Ship().Run(context.Background(), []string{"ship", "observe", "--project", root, "--identity-store", t.TempDir(), "--steer-epoch", "7"})
	var discoveryErr *fleetsteer.LocalDiscoveryError
	if !errors.As(err, &discoveryErr) || discoveryErr.Reason != fleetsteer.DiscoveryMetadataInvalid {
		t.Fatalf("divergent metadata err=%v", err)
	}
}

func TestProductionFleetShipObserveRemoteSteerVerticalSlice(t *testing.T) {
	m11InstallHostileRuntimeGuard(t)
	root := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	for _, d := range []string{".codex/agents", ".shipmates/policies", ".shipmates/sessions"} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRender("codex", "backend", renderCodex(frontmatter{
		Name:        "backend",
		Description: "Production steering E2E fixture",
	}, "Exercise the production remote-steering lifecycle.")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".shipmates/policy.yaml", ".shipmates/policies/backend.yaml"} {
		if err := os.WriteFile(path, []byte(emptyStrictPolicy), 0600); err != nil {
			t.Fatal(err)
		}
	}
	auth := filepath.Join(t.TempDir(), "authority")
	p, err := fleetobserver.OpenProduction(fleetobserver.ProductionConfig{AuthorityStore: auth, FleetID: "flt_0123456789abcdef", FleetEpoch: "epc_0123456789abcdef", SteerEpoch: 7, ServiceIdentity: "fleet-service", Random: bytes.NewReader(bytes.Repeat([]byte{9}, 65536))})
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Registry.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := p.Registry.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	op, err := p.Registry.IssueOperator("sub_0123456789abcdef", []string{enrolled.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(p.Handler())
	defer h.Close()
	idDir := filepath.Join(t.TempDir(), "identity")
	os.MkdirAll(idDir, 0700)
	if err = fleetidentity.StoreShipState(idDir, fleetidentity.ShipState{SchemaVersion: 1, FleetID: enrolled.FleetID, ShipID: enrolled.ShipID, FleetDestination: h.URL, FleetServiceIdentity: "fleet-service", CredentialID: enrolled.Credential.CredentialID, CredentialSecret: enrolled.Credential.Secret}); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "steers")
	srv := server.NewWithCodexOptions(codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestM8FakeCodexAppServer$"}, Environment: map[string]string{"SHIPMATES_M8_FAKE": "1", "SHIPMATES_M8_RECORD": record}, StartupTimeout: time.Second, ShutdownTimeout: time.Second})
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Run(serverCtx) }()
	defer func() {
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("local server cleanup: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("local server cleanup timeout")
		}
	}()
	select {
	case <-srv.Ready():
	case serverErr := <-serverDone:
		t.Fatalf("local server exited before readiness: %v", serverErr)
	case <-time.After(3 * time.Second):
		t.Fatal("local server readiness timeout")
	}
	// Server goroutines outlive command/test cwd changes. Every metadata path
	// must remain anchored to the canonical root captured by server.New.
	foreignRoot := t.TempDir()
	if err := os.Chdir(foreignRoot); err != nil {
		t.Fatal(err)
	}
	recordBytes, err := os.ReadFile(filepath.Join(root, ".shipmates", "sessions", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	var discovery struct {
		SchemaVersion uint64 `json:"schema_version"`
		ProjectRoot   string `json:"project_root"`
		ProjectScope  string `json:"project_scope"`
		Address       string `json:"address"`
		PID           int    `json:"pid"`
		ControlToken  string `json:"control_token"`
	}
	if err := json.Unmarshal(recordBytes, &discovery); err != nil {
		t.Fatal(err)
	}
	wantRoot, _ := project.CanonicalRoot(root)
	wantScope, _ := project.ScopeID(wantRoot)
	recordPath := filepath.Join(wantRoot, ".shipmates", "sessions", "server.json")
	info, err := os.Lstat(recordPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("canonical server record mode: path=%q info=%v err=%v", recordPath, info, err)
	}
	if discovery.SchemaVersion != 1 || discovery.ProjectRoot != wantRoot || discovery.ProjectScope != wantScope || discovery.Address == "" || discovery.PID != os.Getpid() || len(discovery.ControlToken) < 32 {
		t.Fatalf("public discovery metadata invalid: %+v want root=%q scope=%q pid=%d", discovery, wantRoot, wantScope, os.Getpid())
	}
	alive, err := project.ProcessAlive(discovery.PID)
	if err != nil || !alive {
		t.Fatalf("discovery pid not live: pid=%d alive=%v err=%v", discovery.PID, alive, err)
	}
	healthReq, _ := http.NewRequest(http.MethodGet, "http://"+discovery.Address+"/health", nil)
	healthReq.Header.Set("X-Shipmates-Project", discovery.ProjectScope)
	healthResp, err := http.DefaultClient.Do(healthReq)
	if err != nil || healthResp.StatusCode != http.StatusOK || healthResp.Header.Get("X-Shipmates-Project") != discovery.ProjectScope {
		t.Fatalf("scoped health: err=%v response=%v", err, healthResp)
	}
	healthBody, _ := io.ReadAll(healthResp.Body)
	healthResp.Body.Close()
	if string(healthBody) != "ok" {
		t.Fatalf("scoped health body=%q", healthBody)
	}
	reqBody := strings.NewReader(`{"prompt":"work"}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+discovery.Address+"/api/live/backend", reqBody)
	req.Header.Set("X-Shipmates-Project", discovery.ProjectScope)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		var body struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if resp != nil && resp.Body != nil {
			_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
			resp.Body.Close()
		}
		t.Fatalf("start live: err=%v status=%v code=%q message=%q", err, resp, body.Code, body.Message)
	}
	var snap livesession.Snapshot
	if json.NewDecoder(resp.Body).Decode(&snap) != nil {
		t.Fatal("invalid live snapshot")
	}
	resp.Body.Close()
	// Public discovery remains cwd-scoped. A different project cannot discover
	// or control the server-owned session, even while that session stays healthy
	// after the process-wide cwd drift above.
	if err := os.MkdirAll(filepath.Join(foreignRoot, ".codex", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignRoot, ".codex", "agents", "backend.toml"), []byte("name = \"backend\"\ndeveloper_instructions = \"Test backend.\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignErr := Ship().Run(context.Background(), []string{"ship", "observe", "--identity-store", idDir, "--steer-epoch", "7"})
	var discoveryErr *fleetsteer.LocalDiscoveryError
	if !errors.As(foreignErr, &discoveryErr) || discoveryErr.Reason != fleetsteer.DiscoveryMetadataUnavailable {
		t.Fatalf("foreign cwd discovery err=%v", foreignErr)
	}
	if err := os.Chdir(wantRoot); err != nil {
		t.Fatal(err)
	}
	// Exercise the authenticated, scoped production target-query route directly
	// before ship observe adds Fleet and reverse-transport lifecycle around it.
	targetReq, err := http.NewRequest(http.MethodGet, "http://"+discovery.Address+"/api/local/v1/steer-targets", nil)
	if err != nil {
		t.Fatal(err)
	}
	targetReq.Header.Set("Authorization", "Bearer "+discovery.ControlToken)
	targetReq.Header.Set("X-Shipmates-Project", discovery.ProjectScope)
	targetResp, err := http.DefaultClient.Do(targetReq)
	if err != nil {
		t.Fatalf("direct target query: %v", err)
	}
	var targetBody struct {
		SchemaVersion uint64                          `json:"schema_version"`
		ProjectScope  string                          `json:"project_scope"`
		Targets       []livesession.RemoteSteerTarget `json:"targets"`
	}
	targetDecodeErr := json.NewDecoder(io.LimitReader(targetResp.Body, 64<<10)).Decode(&targetBody)
	targetResp.Body.Close()
	if targetResp.StatusCode != http.StatusOK || targetResp.Header.Get("X-Shipmates-Project") != discovery.ProjectScope || targetDecodeErr != nil {
		t.Fatalf("direct target query: status=%d scope=%q decode=%v body=%+v", targetResp.StatusCode, targetResp.Header.Get("X-Shipmates-Project"), targetDecodeErr, targetBody)
	}
	wantTarget := fleetsteer.CurrentTargetReference("backend", snap)
	if targetBody.SchemaVersion != 1 || targetBody.ProjectScope != discovery.ProjectScope || len(targetBody.Targets) != 1 || targetBody.Targets[0].Persona != "backend" || targetBody.Targets[0].Reference != wantTarget {
		t.Fatalf("direct target query body=%+v want target=%q", targetBody, wantTarget)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var observeOut, observeErr synchronizedBuffer
	shipCommand := Ship()
	shipCommand.Writer = &observeOut
	shipCommand.ErrWriter = &observeErr
	go func() {
		done <- shipCommand.Run(ctx, []string{"ship", "observe", "--identity-store", idDir, "--steer-epoch", "7"})
	}()
	observeReadyCtx, stopObserveReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopObserveReady()
	waitConnected := make(chan error, 1)
	go func() { waitConnected <- p.Steer.WaitConnected(observeReadyCtx, enrolled.ShipID, 1) }()
	select {
	case observeRunErr := <-done:
		t.Fatalf("ship observe exited before readiness: err=%v stdout=%q stderr=%q", observeRunErr, observeOut.String(), observeErr.String())
	case readyErr := <-waitConnected:
		if readyErr != nil {
			t.Fatalf("ship observe readiness: %v stdout=%q stderr=%q", readyErr, observeOut.String(), observeErr.String())
		}
	}
	if observeOut.Len() != 0 || observeErr.Len() != 0 {
		t.Fatalf("ship observe startup output: stdout=%q stderr=%q", observeOut.String(), observeErr.String())
	}
	gen := p.Registry.ConnectionGeneration(enrolled.ShipID)
	if gen != 1 {
		t.Fatalf("observation generation=%d, want 1", gen)
	}
	ref := fleetsteer.CurrentTargetReference("backend", snap)
	cl := fleetsteer.Client{BaseURL: h.URL, Credential: op.Record.CredentialID + "." + op.Secret}
	got, err := cl.Submit(context.Background(), fleetsteer.SubmitV1{SchemaVersion: 1, FleetID: enrolled.FleetID, FleetEpoch: 7, ShipID: enrolled.ShipID, ConnectionGeneration: gen, Persona: "backend", SteerTargetRef: ref, Message: "steer once"})
	if got.Outcome != livesession.RemoteSteerAccepted {
		t.Fatalf("steer=%+v err=%v", got, err)
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := os.ReadFile(record)
	if strings.Count(string(b), "steer") != 1 {
		t.Fatalf("adapter calls=%q", b)
	}
	stale, _ := cl.Submit(context.Background(), fleetsteer.SubmitV1{SchemaVersion: 1, FleetID: enrolled.FleetID, FleetEpoch: 7, ShipID: enrolled.ShipID, ConnectionGeneration: gen, Persona: "backend", SteerTargetRef: ref, Message: "stale"})
	if stale.ReasonCode != "stale_target" {
		t.Fatalf("terminal stale=%+v", stale)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observe did not stop")
	}
	off, _ := cl.Submit(context.Background(), fleetsteer.SubmitV1{SchemaVersion: 1, FleetID: enrolled.FleetID, FleetEpoch: 7, ShipID: enrolled.ShipID, ConnectionGeneration: gen, Persona: "backend", SteerTargetRef: ref, Message: "offline"})
	if off.ReasonCode != "ship_offline" {
		t.Fatalf("disconnect=%+v", off)
	}
}
