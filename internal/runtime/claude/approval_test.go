package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// waitApproval drains events until a KindApprovalNeeded arrives and returns
// it along with the event's session/turn attribution.
func waitApproval(t *testing.T, rt *Runtime) (ApprovalRequest, runtime.Event) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before an approval request arrived")
			}
			if ev.Kind == runtime.KindApprovalNeeded {
				req, ok := ev.Payload.(ApprovalRequest)
				if !ok {
					t.Fatalf("KindApprovalNeeded payload is %T, want ApprovalRequest", ev.Payload)
				}
				return req, ev
			}
		case <-timeout:
			t.Fatal("no approval request within 15s")
		}
	}
}

// TestApproval_AllowRoundTrip proves the full loop: the decoder surfaces a
// can_use_tool control_request as KindApprovalNeeded with the tool call
// intact, ResolveApproval writes the allow answer, and the child unblocks
// and finishes the turn.
func TestApproval_AllowRoundTrip(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "approve:git status"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	req, ev := waitApproval(t, rt)

	if req.RequestID == "" {
		t.Fatal("approval carries no request id")
	}
	if req.ToolName != "Bash" {
		t.Errorf("ToolName=%q, want Bash", req.ToolName)
	}
	if req.ToolUseID != "toolu_fake" {
		t.Errorf("ToolUseID=%q", req.ToolUseID)
	}
	if got, ok := req.Command(); !ok || got != "git status" {
		t.Errorf("Command()=%q,%v; want \"git status\",true", got, ok)
	}
	if len(req.SuggestionsJSON) == 0 {
		t.Error("permission_suggestions were dropped")
	}
	if ev.SessionID != s.ID() || ev.TurnID != turn.ID() {
		t.Errorf("approval attributed to %s/%s, want %s/%s", ev.SessionID, ev.TurnID, s.ID(), turn.ID())
	}

	ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{
		ID: req.RequestID, SessionID: s.ID(), TurnID: turn.ID(),
	}, runtime.ApprovalDecision{Allow: true, AllowFor: "once"})
	if err != nil || !ok {
		t.Fatalf("ResolveApproval(allow)=%v,%v", ok, err)
	}
	waitEventText(t, rt, "approval:allow:")
	waitTurnDone(t, rt)
}

// TestApproval_DenyCarriesRationale proves a denial reaches the child with
// the operator's rationale as the message the model sees.
func TestApproval_DenyCarriesRationale(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "approve:rm -rf /"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	req, _ := waitApproval(t, rt)

	ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: req.RequestID},
		runtime.ApprovalDecision{Allow: false, Rationale: "not on my watch"})
	if err != nil || !ok {
		t.Fatalf("ResolveApproval(deny)=%v,%v", ok, err)
	}
	waitEventText(t, rt, "approval:deny:not on my watch")
	waitTurnDone(t, rt)
}

// TestApproval_DenyHasDefaultMessage proves a denial with no rationale
// still carries a message: claude rejects a deny without one.
func TestApproval_DenyHasDefaultMessage(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "approve:curl evil.example"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	req, _ := waitApproval(t, rt)
	if _, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: req.RequestID}, runtime.ApprovalDecision{}); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	waitEventText(t, rt, "approval:deny:Denied by shipmates approval mediation.")
	waitTurnDone(t, rt)
}

// TestResolveApproval_RejectsUnknownAndReusedIDs proves the runtime fails
// closed rather than reporting a write that never happened.
func TestResolveApproval_RejectsUnknownAndReusedIDs(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: "nope"}, runtime.ApprovalDecision{Allow: true}); ok || err == nil {
		t.Fatalf("unknown id: got %v,%v; want false and an error", ok, err)
	}
	if ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{}, runtime.ApprovalDecision{Allow: true}); ok || err == nil {
		t.Fatalf("empty id: got %v,%v; want false and an error", ok, err)
	}

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "approve:ls"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	req, _ := waitApproval(t, rt)

	// Wrong turn attribution must be refused, not answered.
	if ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{
		ID: req.RequestID, SessionID: s.ID(), TurnID: "some-other-turn",
	}, runtime.ApprovalDecision{Allow: true}); ok || err == nil {
		t.Fatalf("mismatched turn: got %v,%v; want false and an error", ok, err)
	}
	if _, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: req.RequestID, TurnID: turn.ID()}, runtime.ApprovalDecision{Allow: true}); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second answer for the same request must be refused: claude has moved on.
	if ok, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: req.RequestID}, runtime.ApprovalDecision{Allow: true}); ok || err == nil {
		t.Fatalf("double resolve: got %v,%v; want false and an error", ok, err)
	}
	waitTurnDone(t, rt)
}

// TestCapabilities_ReportsApprovals guards the honesty of Caps.Approvals.
func TestCapabilities_ReportsApprovals(t *testing.T) {
	if !New(Config{}).Capabilities().Approvals {
		t.Fatal("claude Caps.Approvals must be true now that can_use_tool is mediated")
	}
}

// TestSpawnArgs_RequestStdioPermissionPrompt proves the flag that makes
// permission prompts observable is actually passed. Without it claude
// resolves (and silently denies) prompts on its own.
func TestSpawnArgs_RequestStdioPermissionPrompt(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ARGS", "1")
	defer rt.Close(context.Background())
	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "hi"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitEventText(t, rt, "--permission-prompt-tool stdio")
	waitTurnDone(t, rt)
}

// TestDecodeFrame_ApprovalShapes pins the exact wire shape observed against
// claude 2.1.153 and the negative cases around it.
func TestDecodeFrame_ApprovalShapes(t *testing.T) {
	const real = `{"type":"control_request","request_id":"ea418bbe-425c-4017-af33-292558f4a71b","request":{"subtype":"can_use_tool","tool_name":"Write","display_name":"Write","input":{"file_path":"/tmp/probe-test.txt","content":"banana"},"description":"probe-test.txt","permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],"tool_use_id":"toolu_016nbEodpNG2MdyW6eRgHpsE"}}`
	ev := decodeFrame([]byte(real), "sess", "turn")
	if ev.Kind != runtime.KindApprovalNeeded {
		t.Fatalf("Kind=%q, want %q", ev.Kind, runtime.KindApprovalNeeded)
	}
	req := ev.Payload.(ApprovalRequest)
	if req.RequestID != "ea418bbe-425c-4017-af33-292558f4a71b" || req.ToolName != "Write" || req.Description != "probe-test.txt" {
		t.Fatalf("decoded %+v", req)
	}
	var input map[string]any
	if err := json.Unmarshal(req.InputJSON, &input); err != nil {
		t.Fatalf("InputJSON not preserved: %v", err)
	}
	if input["file_path"] != "/tmp/probe-test.txt" {
		t.Errorf("input=%v", input)
	}
	// Write is not a shell tool, so Command() must not present its args as one.
	if got, ok := req.Command(); ok {
		t.Errorf("Command()=%q,true for a Write; want false", got)
	}

	for name, raw := range map[string]string{
		"other subtype": `{"type":"control_request","request_id":"x","request":{"subtype":"interrupt"}}`,
		"no request id": `{"type":"control_request","request":{"subtype":"can_use_tool","tool_name":"Bash"}}`,
		"no request":    `{"type":"control_request","request_id":"x"}`,
	} {
		if got := decodeFrame([]byte(raw), "s", "t"); got.Kind != runtime.KindBackend {
			t.Errorf("%s: Kind=%q, want backend", name, got.Kind)
		}
	}

	// The control_response frames claude sends back stay backend noise.
	if got := decodeFrame([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"x"}}`), "s", "t"); got.Kind != runtime.KindBackend {
		t.Errorf("control_response Kind=%q, want backend", got.Kind)
	}
}

// TestApproval_DroppedWhenTurnEnds proves a pending approval does not
// outlive its turn: once the turn is over claude is no longer waiting, so
// answering it must fail rather than write to a process that ignores it.
func TestApproval_DroppedWhenTurnEnds(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "approve:sleep 1"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	req, _ := waitApproval(t, rt)
	// Interrupt ends the turn without the approval ever being answered.
	if err := rt.InterruptTurn(context.Background(), s.ID(), turn.ID()); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	deadline := time.After(15 * time.Second)
	for {
		_, err := rt.ResolveApproval(context.Background(), runtime.ApprovalResponse{ID: req.RequestID}, runtime.ApprovalDecision{Allow: true})
		if err != nil && strings.Contains(err.Error(), "no pending approval") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("approval still resolvable after the turn ended")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
