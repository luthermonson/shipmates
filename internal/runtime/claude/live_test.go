package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// Live tests drive the REAL `claude` binary and therefore cost tokens, need
// a logged-in CLI, and are far too slow for CI. They are opt-in:
//
//	SHIPMATES_CLAUDE_LIVE=1 go test -run TestLive -count=1 ./internal/runtime/claude/
//
// They exist because the permission control protocol is not part of Claude
// Code's documented CLI surface: the only way to know shipmates speaks it
// correctly is to speak it to the real thing.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SHIPMATES_CLAUDE_LIVE") != "1" {
		t.Skip("set SHIPMATES_CLAUDE_LIVE=1 to run against the real claude binary")
	}
}

// TestLiveApproval_AllowLetsTheToolRun asserts that a real claude session
// asks before writing a file, that shipmates sees the request, and that an
// allow answer actually results in the file appearing on disk.
func TestLiveApproval_AllowLetsTheToolRun(t *testing.T) {
	requireLive(t)
	dir := t.TempDir()
	// --setting-sources project keeps the developer's own allow rules from
	// pre-approving the tool and skipping the prompt we want to observe.
	rt := New(Config{DefaultArgs: []string{"--setting-sources", "project"}})
	defer rt.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s, err := rt.StartSession(ctx, runtime.SessionSpec{ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(ctx, s.ID(), runtime.TurnInput{
		Text: "Use the Write tool to create a file named live-allow.txt in the current directory containing exactly the word banana. Do not use Bash.",
	}); err != nil {
		t.Fatal(err)
	}
	req := liveWaitApproval(t, ctx, rt)
	if req.ToolName != "Write" {
		t.Fatalf("ToolName=%q, want Write", req.ToolName)
	}
	var input map[string]any
	if err := json.Unmarshal(req.InputJSON, &input); err != nil {
		t.Fatalf("input: %v", err)
	}
	t.Logf("live can_use_tool: id=%s tool=%s desc=%q input=%v suggestions=%s",
		req.RequestID, req.ToolName, req.Description, input, req.SuggestionsJSON)

	if ok, err := rt.ResolveApproval(ctx, runtime.ApprovalResponse{ID: req.RequestID}, runtime.ApprovalDecision{Allow: true, AllowFor: "once"}); err != nil || !ok {
		t.Fatalf("ResolveApproval=%v,%v", ok, err)
	}
	liveWaitTerminal(t, ctx, rt)
	if _, err := os.Stat(filepath.Join(dir, "live-allow.txt")); err != nil {
		t.Fatalf("allow did not let the tool run: %v", err)
	}
}

// TestLiveApproval_DenyBlocksTheTool asserts the deny answer is honored by
// the real binary: the file is never created and the turn still completes.
func TestLiveApproval_DenyBlocksTheTool(t *testing.T) {
	requireLive(t)
	dir := t.TempDir()
	rt := New(Config{DefaultArgs: []string{"--setting-sources", "project"}})
	defer rt.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s, err := rt.StartSession(ctx, runtime.SessionSpec{ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(ctx, s.ID(), runtime.TurnInput{
		Text: "Use the Write tool to create a file named live-deny.txt containing the word kiwi. Do not use Bash. If it fails, reply FAILED and stop.",
	}); err != nil {
		t.Fatal(err)
	}
	req := liveWaitApproval(t, ctx, rt)
	if ok, err := rt.ResolveApproval(ctx, runtime.ApprovalResponse{ID: req.RequestID}, runtime.ApprovalDecision{Allow: false, Rationale: "shipmates policy says no"}); err != nil || !ok {
		t.Fatalf("ResolveApproval=%v,%v", ok, err)
	}
	liveWaitTerminal(t, ctx, rt)
	if _, err := os.Stat(filepath.Join(dir, "live-deny.txt")); err == nil {
		t.Fatal("deny did not block the tool: file exists")
	}
}

func liveWaitApproval(t *testing.T, ctx context.Context, rt *Runtime) ApprovalRequest {
	t.Helper()
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before an approval arrived")
			}
			if ev.Kind == runtime.KindApprovalNeeded {
				return ev.Payload.(ApprovalRequest)
			}
			if ev.Kind == runtime.KindTurnDone {
				t.Fatal("turn finished without ever asking for permission")
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for an approval")
		}
	}
}

func liveWaitTerminal(t *testing.T, ctx context.Context, rt *Runtime) {
	t.Helper()
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				return
			}
			if ev.Kind == runtime.KindTurnDone || ev.Kind == runtime.KindError {
				return
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the turn to end")
		}
	}
}
