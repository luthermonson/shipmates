package codexapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const secret = "SHIPMATES-CANARY-DO-NOT-LEAK"

func TestActivityNotificationsExposeBoundedStructuredDetail(t *testing.T) {
	a := &Adapter{events: make(chan Event, 4), approvals: make(map[string]pendingApproval)}
	commandParams, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"type": "commandExecution", "command": "go test ./..."}})
	a.notification(rpcMessage{Method: "item/started", Params: commandParams})
	command := <-a.events
	if command.Kind != Activity || command.Category != "command" || command.Detail != "go test ./..." {
		t.Fatalf("command event = %+v", command)
	}
	fileParams, _ := json.Marshal(map[string]any{"threadId": "thread", "turnId": "turn", "item": map[string]any{"type": "fileChange", "changes": []map[string]string{{"kind": "update", "path": "internal/app.go"}}}})
	a.notification(rpcMessage{Method: "item/started", Params: fileParams})
	file := <-a.events
	if file.Category != "file_change" || file.Detail != "update internal/app.go" {
		t.Fatalf("file event = %+v", file)
	}
}

// A frame larger than the bufio reader's 64 KiB buffer but smaller than the
// configured cap must be accumulated and returned, not reported as malformed.
// An earlier readBoundedFrame treated bufio.ErrBufferFull as a fault, so any
// legitimate frame in that range terminally faulted the adapter and blamed the
// backend for a client-side buffer size.
func TestReadBoundedFrameAcceptsMultiMegabyteFrameWithinLimit(t *testing.T) {
	payload := strings.Repeat("x", 2<<20)
	frame := []byte(`{"method":"item/started","params":{"payload":"` + payload + `"}}` + "\n")
	got, err := readBoundedFrame(bufio.NewReaderSize(bytes.NewReader(frame), 64*1024), 8<<20)
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatalf("large frame: bytes=%d err=%v", len(got), err)
	}
	if _, err := readBoundedFrame(bufio.NewReaderSize(bytes.NewReader(frame), 64*1024), 1<<20); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

// A frame that spans many buffer refills must survive the loop intact, which is
// the property the accumulation exists for.
func TestReadBoundedFrameReassemblesManyFragmentsExactly(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 40*1024) // 320 KiB, ~5 buffer fills
	frame := append(append([]byte{'{'}, body...), '}', '\n')
	got, err := readBoundedFrame(bufio.NewReaderSize(bytes.NewReader(frame), 64*1024), 1<<20)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("reassembled %d bytes, want %d identical", len(got), len(frame))
	}
}

func TestFakeAppServerProcess(t *testing.T) {
	if os.Getenv("SHIPMATES_FAKE_APP_SERVER") != "1" {
		return
	}
	scenario := os.Getenv("SHIPMATES_FAKE_SCENARIO")
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16<<20)
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
	// No threadId/turnId: an unattributable approval request, which the adapter
	// must refuse on the wire rather than mediate.
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
	// A thread/turn round trip driven entirely by the fixture, so the happy path
	// is covered without a real codex binary on PATH.
	if scenario == "thread-turn" {
		for in.Scan() {
			var m rpcMessage
			if json.Unmarshal(in.Bytes(), &m) != nil {
				os.Exit(6)
			}
			callID, ok := parseID(m.ID)
			if !ok {
				continue
			}
			var reply any
			switch m.Method {
			case "thread/start", "thread/resume":
				threadID := "thread-0001"
				var p struct {
					ThreadID string `json:"threadId"`
				}
				if json.Unmarshal(m.Params, &p) == nil && p.ThreadID != "" {
					threadID = p.ThreadID
				}
				reply = map[string]any{"thread": map[string]string{"id": threadID}}
			case "turn/start":
				reply = map[string]any{"turn": map[string]string{"id": "turn-0001"}}
			default:
				reply = map[string]any{}
			}
			out, _ := json.Marshal(map[string]any{"id": callID, "result": reply})
			fmt.Println(string(out))
		}
		os.Exit(0)
	}
}

func fakeOptions(t *testing.T, scenario string) StartOptions {
	t.Helper()
	t.Setenv("UNRELATED_SECRET", secret)
	// StartupTimeout is a ceiling, not a target — the non-timeout scenarios
	// finish in milliseconds. On a contended runner a tight budget turns into a
	// spurious startup_timeout. Tests that assert on the timeout itself override
	// this.
	return StartOptions{WorkingDirectory: t.TempDir(), Environment: map[string]string{"SHIPMATES_FAKE_APP_SERVER": "1", "SHIPMATES_FAKE_SCENARIO": scenario}, Command: []string{os.Args[0], "-test.run=^TestFakeAppServerProcess$"}, StartupTimeout: 30 * time.Second, ShutdownTimeout: 100 * time.Millisecond, MaxFrameBytes: 1024}
}

// This test is the end-to-end guard on openProcessIdentity: Factory.Start treats
// a failure there as fatal and reports Internal, so while process_windows.go
// returned os.ErrInvalid every Codex app-server launch on Windows failed with
// "the Codex app-server could not be started" and this test could not pass.
func TestStartHandshakeAndIdempotentClose(t *testing.T) {
	a, caps, err := Factory{}.Start(context.Background(), fakeOptions(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	if caps != requiredCapabilities() {
		t.Fatalf("capabilities = %#v", caps)
	}
	if a.pidfd <= 0 {
		t.Fatalf("process identity = %d, want a real handle/descriptor", a.pidfd)
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

func TestThreadAndTurnRoundTripAgainstFakeAppServer(t *testing.T) {
	a, _, err := Factory{}.Start(context.Background(), fakeOptions(t, "thread-turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	thread, err := a.StartThread(ctx, ThreadOptions{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if thread.ID != "thread-0001" {
		t.Fatalf("thread = %+v", thread)
	}
	turn, err := a.StartTurn(ctx, thread.ID, TurnInput{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if turn.ID != "turn-0001" {
		t.Fatalf("turn = %+v", turn)
	}
	if err := a.SteerTurn(ctx, thread.ID, turn.ID, "keep going"); err != nil {
		t.Fatal(err)
	}
	if err := a.InterruptTurn(ctx, thread.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.ResumeThread(ctx, thread.ID, ThreadOptions{WorkingDirectory: t.TempDir()})
	if err != nil || resumed.ID != thread.ID {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
}

// The reaper and the read loop observe child exit through independent events,
// and os/exec closes the parent end of the stdout pipe once cmd.Wait returns.
// When the reaper wins that race the read fails with os.ErrClosed rather than
// io.EOF, and it must still be reported as an unexpected close.
func TestReaperClosingStdoutIsClassifiedAsUnexpectedEOF(t *testing.T) {
	// The write end is kept open by the pipe cleanup, so the loop can never see a
	// genuine io.EOF here; the only possible termination is the closed descriptor.
	stdout, _ := ioPipe(t)
	a := &Adapter{stdout: stdout, nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{}), maxFrame: 1024}
	go a.readLoop()
	_ = stdout.Close()
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("read loop did not terminate")
	}
	if ErrorCode(a.terminal) != UnexpectedEOF {
		t.Fatalf("terminal = %v, want %q", a.terminal, UnexpectedEOF)
	}
}

func TestThreadParamsToollessReadOnly(t *testing.T) {
	p := threadParams(ThreadOptions{WorkingDirectory: "/tmp/project", Model: "gpt-5.6-sol", ReadOnly: true, Toolless: true})
	if p["sandbox"] != "read-only" || p["model"] != "gpt-5.6-sol" {
		t.Fatalf("unsafe thread params: %#v", p)
	}
	tools, ok := p["tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Fatalf("tools were not disabled: %#v", p["tools"])
	}
}

// The runtime layer advertises Caps.Approvals, which is only true if the backend
// is actually told to ask. "never" would make that claim a lie regardless of how
// the client behaves.
func TestThreadParamsRequestApprovalsSoTheCapabilityIsReal(t *testing.T) {
	p := threadParams(ThreadOptions{WorkingDirectory: "/tmp/project"})
	if p["approvalPolicy"] != "on-request" {
		t.Fatalf("approvalPolicy = %v, want on-request", p["approvalPolicy"])
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

// Windows is not skipped here. Closing stdin is the graceful path on every
// platform; when the child ignores it, Close escalates through the process
// identity to a real kill, which is exactly what a working openProcessIdentity
// buys on Windows.
func TestCloseKillsIgnoredShutdown(t *testing.T) {
	a, _, err := Factory{}.Start(context.Background(), fakeOptions(t, "ignore-close"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

func TestProcessGroupHandleTerminatesAndReapsChild(t *testing.T) {
	a, _, err := Factory{}.Start(context.Background(), fakeOptions(t, "ignore-close"))
	if err != nil {
		t.Fatal(err)
	}
	h := a.ProcessGroupHandle()
	if h == nil {
		t.Fatal("missing process-group handle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = h.GracefulTerminate(ctx)
	cancel()
	if err != nil {
		if killErr := h.ForceKill(context.Background()); killErr != nil {
			t.Fatal(killErr)
		}
	}
	if err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProcessGroupHandleReportsContainmentCleanupFailure(t *testing.T) {
	done := make(chan struct{})
	close(done)
	a := &Adapter{waitDone: done, shutdown: time.Millisecond, containmentErr: errors.New("contained descendants remain")}
	if err := a.ProcessGroupHandle().Wait(context.Background()); ErrorCode(err) != CleanupFailed {
		t.Fatalf("wait error code = %q, want %q (err=%v)", ErrorCode(err), CleanupFailed, err)
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

// turn/steer carries expectedTurnId, a precondition the app-server enforces.
// Sending "turnId" instead — which an earlier version did — is silently ignored
// by the real server as an unknown member, so the call degrades to "steer
// whatever turn is running now" and the exact-turn guarantee evaporates. The
// assertion that turnId is absent is the point: a test that accepted either
// spelling is what let the bug survive.
func TestSteerTurnUsesExpectedTurnIDPrecondition(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	clientRead, serverWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, stdout: clientRead, nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{}), maxFrame: 4096}
	go a.readLoop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.SteerTurn(context.Background(), "thread-00000001", "turn-0000000001", "continue")
	}()

	line, err := bufio.NewReader(serverRead).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ThreadID       string          `json:"threadId"`
			ExpectedTurnID string          `json:"expectedTurnId"`
			LegacyTurnID   json.RawMessage `json:"turnId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "turn/steer" || request.Params.ThreadID != "thread-00000001" || request.Params.ExpectedTurnID != "turn-0000000001" || request.Params.LegacyTurnID != nil {
		t.Fatalf("steer request = %s", line)
	}
	id, ok := parseID(request.ID)
	if !ok {
		t.Fatal("missing request id")
	}
	response, _ := json.Marshal(map[string]any{"id": id, "result": map[string]any{}})
	if _, err := serverWrite.Write(append(response, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
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

// An approval request bound to an exact (threadId, turnId) must be held open and
// surfaced, never auto-answered. The wire assertion is the load-bearing part: the
// FIRST line the server sees must be the owner's decision, so an implementation
// that cancelled on arrival — as an earlier one did while still reporting
// Caps.Approvals true — fails here rather than passing quietly.
func TestApprovalWithExactTurnBindingIsMediatedNotAutoAnswered(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, events: make(chan Event, 8), approvals: make(map[string]pendingApproval)}
	server := bufio.NewReader(serverRead)

	params, _ := json.Marshal(map[string]string{"threadId": "thread-1", "turnId": "turn-1", "command": "echo safe"})
	if !a.refuse(rpcMessage{ID: json.RawMessage("1"), Method: "item/commandExecution/requestApproval", Params: params}) {
		t.Fatal("request was not handled")
	}
	select {
	case event := <-a.events:
		if event.Kind != ApprovalRequested || event.ThreadID != "thread-1" || event.TurnID != "turn-1" ||
			event.BackendRequestID != "1" || event.Category != "command" || event.CommandExact != "echo safe" {
			t.Fatalf("approval event = %+v", event)
		}
	default:
		t.Fatal("bound approval request emitted no event")
	}
	if a.PendingApprovals() != 1 {
		t.Fatalf("pending approvals = %d, want 1", a.PendingApprovals())
	}

	response := ApprovalResponse{RequestID: "1", ThreadID: "thread-1", TurnID: "turn-1"}
	if ok, err := a.ResolveApproval(context.Background(), response, AllowOnce); err != nil || !ok {
		t.Fatalf("resolve = %v, %v", ok, err)
	}
	line, err := server.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, `"decision":"accept"`) {
		t.Fatalf("first wire response = %q, want the owner's accept (nothing may be written before it)", line)
	}
	if a.PendingApprovals() != 0 {
		t.Fatalf("resolved approval still pending: %d", a.PendingApprovals())
	}
	if _, err := a.ResolveApproval(context.Background(), response, Deny); ErrorCode(err) != ProtocolViolation {
		t.Fatalf("duplicate resolution = %v", err)
	}

	// A correlation that does not match the captured request exactly is refused
	// rather than best-effort matched.
	params2, _ := json.Marshal(map[string]string{"threadId": "thread-1", "turnId": "turn-2", "command": "echo safe"})
	if !a.refuse(rpcMessage{ID: json.RawMessage("2"), Method: "item/commandExecution/requestApproval", Params: params2}) {
		t.Fatal("second request was not handled")
	}
	<-a.events
	for _, bad := range []ApprovalResponse{
		{RequestID: "2", ThreadID: "thread-other", TurnID: "turn-2"},
		{RequestID: "2", ThreadID: "thread-1", TurnID: "turn-1"},
		{RequestID: "99", ThreadID: "thread-1", TurnID: "turn-2"},
	} {
		if _, err := a.ResolveApproval(context.Background(), bad, AllowOnce); ErrorCode(err) != ProtocolViolation {
			t.Fatalf("mismatched resolve %+v = %v, want protocol_violation", bad, err)
		}
	}
}

// Requests the adapter cannot attribute to an exact turn are refused on the wire
// and deliberately absent from the event stream: no session or turn owns them, so
// there is nothing for a consumer to file them under and nobody who could answer.
func TestUnattributableRequestsAreRefusedOnTheWireAndNotSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name, method, wire string
		params             map[string]any
	}{
		{"no identity", "item/commandExecution/requestApproval", `"decision":"cancel"`, map[string]any{"command": "echo hi"}},
		{"legacy exec approval", "execCommandApproval", `"decision":"abort"`, map[string]any{"command": "echo hi"}},
		{"legacy patch approval", "applyPatchApproval", `"decision":"abort"`, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverRead, clientWrite := ioPipe(t)
			a := &Adapter{stdin: clientWrite, events: make(chan Event, 4), approvals: make(map[string]pendingApproval)}
			params, _ := json.Marshal(tc.params)
			if !a.refuse(rpcMessage{ID: json.RawMessage("5"), Method: tc.method, Params: params}) {
				t.Fatal("request was not safely refused")
			}
			line, err := bufio.NewReader(serverRead).ReadString('\n')
			if err != nil || !strings.Contains(line, tc.wire) {
				t.Fatalf("wire refusal = %q, %v; want %s", line, err, tc.wire)
			}
			select {
			case event := <-a.events:
				t.Fatalf("unattributable request was surfaced: %+v", event)
			default:
			}
			if a.PendingApprovals() != 0 {
				t.Fatalf("unattributable request left %d pending", a.PendingApprovals())
			}
		})
	}
}

// A bound request whose payload is not reviewable (no command text) falls back to
// the wire refusal, but is still reported as a refusal against its turn.
func TestBoundRequestWithoutReviewablePayloadIsRefusedAndReported(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, events: make(chan Event, 4), approvals: make(map[string]pendingApproval)}
	params, _ := json.Marshal(map[string]string{"threadId": "thread-1", "turnId": "turn-1"})
	if !a.refuse(rpcMessage{ID: json.RawMessage("8"), Method: "item/commandExecution/requestApproval", Params: params}) {
		t.Fatal("request was not safely refused")
	}
	line, err := bufio.NewReader(serverRead).ReadString('\n')
	if err != nil || !strings.Contains(line, `"decision":"cancel"`) {
		t.Fatalf("wire refusal = %q, %v", line, err)
	}
	event := <-a.events
	if event.Kind != RequestRefused || event.TurnID != "turn-1" || event.ReasonCode != "unsupported_request" {
		t.Fatalf("refusal event = %+v", event)
	}
}

func TestFileChangeApprovalIsMediatedWithChangeSummary(t *testing.T) {
	_, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, events: make(chan Event, 4), approvals: make(map[string]pendingApproval)}
	params, _ := json.Marshal(map[string]any{
		"threadId": "thread-1", "turnId": "turn-1",
		"changes": []map[string]string{{"kind": "update", "path": "internal/app.go"}, {"kind": "add", "path": "internal/new.go"}},
	})
	if !a.refuse(rpcMessage{ID: json.RawMessage("11"), Method: "item/fileChange/requestApproval", Params: params}) {
		t.Fatal("request was not handled")
	}
	event := <-a.events
	if event.Kind != ApprovalRequested || event.Category != "file_change" ||
		event.Detail != "update internal/app.go, add internal/new.go" {
		t.Fatalf("file change approval = %+v", event)
	}
}

// A turn that completes or fails will never read a decision, so its pending
// approvals must not stay answerable.
func TestCompletedTurnAbandonsItsPendingApprovals(t *testing.T) {
	_, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, events: make(chan Event, 8), approvals: make(map[string]pendingApproval)}
	params, _ := json.Marshal(map[string]string{"threadId": "thread-1", "turnId": "turn-1", "command": "echo safe"})
	if !a.refuse(rpcMessage{ID: json.RawMessage("3"), Method: "item/commandExecution/requestApproval", Params: params}) {
		t.Fatal("request was not handled")
	}
	<-a.events
	done, _ := json.Marshal(map[string]any{"threadId": "thread-1", "turnId": "turn-1"})
	a.notification(rpcMessage{Method: "turn/completed", Params: done})
	<-a.events
	if a.PendingApprovals() != 0 {
		t.Fatalf("pending after turn completion = %d, want 0", a.PendingApprovals())
	}
}

func TestResolveApprovalPreCancelledLeavesCorrelationPending(t *testing.T) {
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, approvals: map[string]pendingApproval{"7": {id: 7, threadID: "thread", turnID: "turn"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ApprovalResponse{RequestID: "7", ThreadID: "thread", TurnID: "turn"}
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

func TestCredentialFreeEnvironmentHasNoProviderCredentialHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/secret/provider-home")
	t.Setenv("HOME", "/secret/home")
	for _, entry := range controlledEnvironment(map[string]string{"CODEX_HOME": "/secret/override", "SOL_TOKEN": "secret"}, true, "") {
		if strings.HasPrefix(entry, "CODEX_HOME=") || strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "SOL_TOKEN=") {
			t.Fatalf("credential-bearing environment leaked: %q", entry)
		}
	}
}

func TestCredentialFreeEnvironmentAllowsOnlyExplicitTransportCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/secret/provider-home")
	t.Setenv("HOME", "/secret/home")
	// Absolute on both platforms: controlledEnvironment refuses a
	// TransportCodexHome that filepath.IsAbs rejects, and "/tmp/..." is not
	// absolute on Windows.
	isolated := filepath.Join(t.TempDir(), "shipmates-transport-auth")
	got := make(map[string]string)
	for _, entry := range controlledEnvironment(map[string]string{"CODEX_HOME": "/secret/override", "SOL_TOKEN": "secret"}, true, isolated) {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["CODEX_HOME"] != isolated {
		t.Fatalf("CODEX_HOME = %q, want isolated transport home", got["CODEX_HOME"])
	}
	if _, ok := got["HOME"]; ok {
		t.Fatal("ambient HOME leaked")
	}
	if _, ok := got["SOL_TOKEN"]; ok {
		t.Fatal("provider token leaked")
	}
	if got[ManagedSessionEnvironment] != "1" {
		t.Fatalf("%s = %q, want 1", ManagedSessionEnvironment, got[ManagedSessionEnvironment])
	}
}

func TestContainedExecutableResolvesNativeCodexPackage(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("native Codex containment mapping is Linux-only")
	}
	root := t.TempDir()
	script := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	packageName, target := "codex-linux-x64", "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		packageName, target = "codex-linux-arm64", "aarch64-unknown-linux-musl"
	}
	native := filepath.Join(root, "node_modules", "@openai", packageName, "vendor", target, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, append([]byte("\x7fELF"), make([]byte, 64)...), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := containedExecutable(script)
	if err != nil || got != native {
		t.Fatalf("contained executable = %q, %v; want %q", got, err, native)
	}
}

// Containment is cgroup-based and therefore Linux-only by nature. Everywhere
// else the fallback must be a loud refusal, never a silent pass that would let a
// caller believe an uncontained child was contained.
func TestContainmentRefusedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("containment is implemented on linux")
	}
	if _, err := StartContainedWithHelperCurrent(DefaultPreExecHelper, exec.Command(os.Args[0]), "id"); err == nil {
		t.Fatal("containment must not report success on a platform without cgroups")
	}
	opts := fakeOptions(t, "ok")
	opts.RequireExecutionContainment = true
	opts.ExecutionID = "probe"
	if _, _, err := (Factory{}).Start(context.Background(), opts); err == nil {
		t.Fatal("Start must refuse a contained transport where containment is unavailable")
	}
}

func TestContainedRealCodexTransport(t *testing.T) {
	if os.Getenv("SHIPMATES_REAL_CONTAINMENT_TEST") != "1" {
		t.Skip("set SHIPMATES_REAL_CONTAINMENT_TEST=1 inside a delegated Linux scope")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wrapper, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err = filepath.EvalSymlinks(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	native, err := containedExecutable(wrapper)
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(native, "--version")
	probe.Env = controlledEnvironment(nil, true, filepath.Join(home, ".codex"))
	var probeStderr bytes.Buffer
	probe.Stderr = &probeStderr
	contained, err := StartContainedWithHelperCurrent("/usr/libexec/shipmates/shipmates-cgroup-launcher", probe, "native-probe")
	if err != nil {
		t.Fatalf("native containment: %v; helper stderr: %q", err, probeStderr.String())
	}
	_ = probe.Wait()
	if err := contained.Close(); err != nil {
		t.Fatalf("native containment cleanup: %v", err)
	}
	adapter, _, err := (Factory{}).Start(ctx, StartOptions{
		WorkingDirectory:            t.TempDir(),
		CredentialFree:              true,
		TransportCodexHome:          filepath.Join(home, ".codex"),
		RequireExecutionContainment: true,
		ExecutionID:                 "real-codex-probe",
		// Empty configuration must resolve to the unified installer's helper.
		PreExecHelper: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
