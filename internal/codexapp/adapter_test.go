package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/policy"
)

const secret = "SHIPMATES-CANARY-DO-NOT-LEAK"

func TestFakeAppServerProcess(t *testing.T) {
	if os.Getenv("SHIPMATES_FAKE_APP_SERVER") != "1" {
		return
	}
	scenario := os.Getenv("SHIPMATES_FAKE_SCENARIO")
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() {
		os.Exit(2)
	}
	var init rpcMessage
	if json.Unmarshal(in.Bytes(), &init) != nil || init.Method != "initialize" {
		os.Exit(3)
	}
	if scenario == "timeout" {
		in.Scan()
		return
	}
	if scenario == "stderr" {
		for i := 0; i < 10000; i++ {
			_, _ = fmt.Fprintln(os.Stderr, secret)
		}
	}
	if scenario == "approval" {
		fmt.Println(`{"id":91,"method":"item/commandExecution/requestApproval","params":{"command":"` + secret + `"}}`)
		if !in.Scan() || !strings.Contains(in.Text(), `"decision":"cancel"`) {
			os.Exit(4)
		}
	}
	if scenario == "user-input" {
		fmt.Println(`{"id":92,"method":"item/tool/requestUserInput","params":{"question":"` + secret + `"}}`)
		if !in.Scan() || !strings.Contains(in.Text(), `"answers":{}`) {
			os.Exit(5)
		}
	}
	if scenario == "unknown-request" {
		fmt.Println(`{"id":93,"method":"future/mandatory","params":{"raw":"` + secret + `"}}`)
		select {}
	}
	if scenario == "malformed" {
		fmt.Println(`{"id":`)
		select {}
	}
	if scenario == "oversized" {
		fmt.Println(`{"method":"` + strings.Repeat("x", 4096) + `"}`)
		select {}
	}
	if scenario == "eof" {
		os.Exit(0)
	}
	id, _ := parseID(init.ID)
	ua := "codex-cli 0.144.1"
	if scenario == "old-version" {
		ua = "codex-cli 0.143.9"
	}
	result := map[string]any{"userAgent": ua, "codexHome": "/safe", "platformFamily": "unix", "platformOs": "linux"}
	if scenario == "missing-capability" {
		result["capabilities"] = Capabilities{ThreadStart: true}
	}
	b, _ := json.Marshal(map[string]any{"id": id, "result": result})
	fmt.Println(string(b))
	if !in.Scan() {
		os.Exit(0)
	}
	if scenario == "ignore-close" {
		select {}
	}
}

func fakeOptions(t *testing.T, scenario string) StartOptions {
	t.Helper()
	t.Setenv("UNRELATED_SECRET", secret)
	return StartOptions{WorkingDirectory: t.TempDir(), Environment: map[string]string{"SHIPMATES_FAKE_APP_SERVER": "1", "SHIPMATES_FAKE_SCENARIO": scenario}, Command: []string{os.Args[0], "-test.run=^TestFakeAppServerProcess$"}, StartupTimeout: 2 * time.Second, ShutdownTimeout: 100 * time.Millisecond, MaxFrameBytes: 1024}
}

func TestStartHandshakeAndIdempotentClose(t *testing.T) {
	a, caps, err := Factory{}.Start(context.Background(), fakeOptions(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if caps != requiredCapabilities() {
		t.Fatalf("capabilities = %#v", caps)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.waitDone:
	default:
		t.Fatal("child was not reaped")
	}
}

func TestStartupRefusalsAreSanitizedAndReaped(t *testing.T) {
	for _, tc := range []struct {
		name string
		code Code
	}{{"old-version", UnsupportedVersion}, {"missing-capability", UnsupportedCapability}, {"malformed", MalformedFrame}, {"oversized", MalformedFrame}, {"eof", UnexpectedEOF}, {"unknown-request", ProtocolViolation}, {"timeout", StartupTimeout}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := fakeOptions(t, tc.name)
			if tc.name == "timeout" {
				opts.StartupTimeout = 150 * time.Millisecond
			}
			_, _, err := Factory{}.Start(context.Background(), opts)
			if ErrorCode(err) != tc.code {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(fmt.Sprint(err), secret) {
				t.Fatalf("secret leaked: %v", err)
			}
		})
	}
}

func TestStartupDeniesRequestsAndDrainsStderr(t *testing.T) {
	for _, scenario := range []string{"approval", "user-input", "stderr"} {
		t.Run(scenario, func(t *testing.T) {
			a, _, err := Factory{}.Start(context.Background(), fakeOptions(t, scenario))
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCallerCancellationCleansChild(t *testing.T) {
	opts := fakeOptions(t, "timeout")
	opts.StartupTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, _, err := Factory{}.Start(ctx, opts)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("secret leaked")
	}
}

func TestCloseKillsIgnoredShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interrupt behavior differs on windows")
	}
	a, _, err := Factory{}.Start(context.Background(), fakeOptions(t, "ignore-close"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.waitDone:
	default:
		t.Fatal("child was not reaped")
	}
}

func TestCorrelationRejectsDuplicateAndHandlesOutOfOrder(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	clientRead, serverWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, stdout: clientRead, nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{}), maxFrame: 1024}
	go a.readLoop()
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			var out map[string]bool
			errs <- a.call(context.Background(), "probe", nil, &out)
		}()
	}
	r := bufio.NewReader(serverRead)
	var reqs []rpcMessage
	for len(reqs) < 2 {
		line, _ := r.ReadBytes('\n')
		var m rpcMessage
		_ = json.Unmarshal(line, &m)
		reqs = append(reqs, m)
	}
	for i := 1; i >= 0; i-- {
		id, _ := parseID(reqs[i].ID)
		b, _ := json.Marshal(map[string]any{"id": id, "result": map[string]bool{"ok": true}})
		_, _ = serverWrite.Write(append(b, '\n'))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id, _ := parseID(reqs[0].ID)
	b, _ := json.Marshal(map[string]any{"id": id, "result": map[string]bool{"ok": true}})
	_, _ = serverWrite.Write(append(b, '\n'))
	select {
	case <-a.done:
	case <-time.After(time.Second):
		t.Fatal("duplicate did not fail closed")
	}
	if ErrorCode(a.terminal) != ProtocolViolation {
		t.Fatalf("terminal=%v", a.terminal)
	}
}

func ioPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

func TestRefusalRequiresExactTurnPolicyBinding(t *testing.T) {
	snapshot, diagnostics := policy.Parse("backend", "root", []policy.Source{
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProject, Path: ".shipmates/policy.yaml", Present: true}, Bytes: []byte("version: 1\nallow: []\nask: []\ndeny: []\n")},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerPersona, Path: ".shipmates/policies/backend.yaml", Present: true}, Bytes: []byte("version: 1\nallow: []\nask: []\ndeny: []\n")},
	})
	if snapshot == nil || len(diagnostics) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", snapshot, diagnostics)
	}
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, events: make(chan Event, 8), policies: map[turnKey]*policy.Snapshot{{threadID: "thread-1", turnID: "turn-1"}: snapshot}, approvals: make(map[string]pendingApproval)}

	request := func(id int, threadID, turnID string, mediated bool) {
		t.Helper()
		params, _ := json.Marshal(map[string]string{"threadId": threadID, "turnId": turnID, "command": "echo safe"})
		idBytes, _ := json.Marshal(id)
		if !a.refuse(rpcMessage{ID: idBytes, Method: "item/commandExecution/requestApproval", Params: params}) {
			t.Fatal("request was not safely refused")
		}
		if mediated {
			return
		}
		line, err := bufio.NewReader(serverRead).ReadString('\n')
		if err != nil || !strings.Contains(line, `"decision":"cancel"`) {
			t.Fatalf("wire refusal = %q, %v", line, err)
		}
	}

	request(1, "thread-1", "turn-1", true)
	select {
	case event := <-a.events:
		if event.Kind != ApprovalRequested || event.ThreadID != "thread-1" || event.TurnID != "turn-1" || event.PolicySnapshotID != snapshot.ID || event.BackendRequestID != "1" {
			t.Fatalf("exact event = %+v", event)
		}
	default:
		t.Fatal("exact request emitted no audit event")
	}
	if ok, err := a.ResolveApproval(context.Background(), ApprovalResponse{RequestID: "1", ThreadID: "thread-1", TurnID: "turn-1", PolicySnapshotID: snapshot.ID}, AllowOnce); err != nil || !ok {
		t.Fatalf("resolve = %v, %v", ok, err)
	}
	line, err := bufio.NewReader(serverRead).ReadString('\n')
	if err != nil || !strings.Contains(line, `"decision":"accept"`) {
		t.Fatalf("wire allow = %q, %v", line, err)
	}
	if _, err := a.ResolveApproval(context.Background(), ApprovalResponse{RequestID: "1", ThreadID: "thread-1", TurnID: "turn-1", PolicySnapshotID: snapshot.ID}, Deny); ErrorCode(err) != ProtocolViolation {
		t.Fatalf("duplicate resolution = %v", err)
	}

	for i, target := range []struct{ thread, turn string }{{"thread-1", "turn-old"}, {"thread-other", "turn-1"}, {"", ""}} {
		request(i+2, target.thread, target.turn, false)
		select {
		case event := <-a.events:
			t.Fatalf("mismatched request was attributed: %+v", event)
		default:
		}
	}
}

func TestResolveApprovalPreCancelledLeavesCorrelationPending(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, approvals: map[string]pendingApproval{"7": {id: 7, threadID: "thread", turnID: "turn", policySnapshotID: strings.Repeat("a", 64)}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ApprovalResponse{RequestID: "7", ThreadID: "thread", TurnID: "turn", PolicySnapshotID: strings.Repeat("a", 64)}
	if ok, err := a.ResolveApproval(ctx, r, AllowOnce); ok || err != context.Canceled {
		t.Fatalf("pre-cancelled resolve = %v, %v", ok, err)
	}
	if ok, err := a.ResolveApproval(context.Background(), r, Deny); err != nil || !ok {
		t.Fatalf("retry deny = %v, %v", ok, err)
	}
	line, err := bufio.NewReader(serverRead).ReadString('\n')
	if err != nil || !strings.Contains(line, `"decision":"cancel"`) {
		t.Fatalf("wire retry = %q, %v", line, err)
	}
}
