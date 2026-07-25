package livesession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
)

// approvalEvent builds the runtime event a claude session emits when Claude
// Code asks for permission, attributed to the live tuple.
func approvalEvent(threadID, turnID, requestID, tool, inputJSON string) runtime.Event {
	return runtime.Event{
		Kind:      runtime.KindApprovalNeeded,
		SessionID: threadID,
		TurnID:    turnID,
		Payload: claude.ApprovalRequest{
			RequestID: requestID,
			ToolName:  tool,
			ToolUseID: "toolu_test",
			InputJSON: json.RawMessage(inputJSON),
		},
	}
}

// writePersonaPolicy replaces the persona's policy overlay with a single
// rule of the given effect over the given exact command. Must run before
// the session starts, because the snapshot is bound at turn dispatch.
func writePersonaPolicy(t *testing.T, persona, effect, id, command string) {
	t.Helper()
	body := "version: 1\nallow: []\nask: []\ndeny: []\n"
	rule := "  - id: " + id + "\n    kind: process.exec\n    match:\n      command_exact: " + command + "\n    reason: test rule\n"
	body = strings.Replace(body, effect+": []\n", effect+":\n"+rule, 1)
	if err := os.WriteFile(filepath.Join(".shipmates", "policies", persona+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeApprovalWithoutOperatorIsRefusedNotDropped is the regression
// this whole path exists for: a claude approval used to be dropped with a
// log line, leaving the turn wedged forever. It must now reach the
// mediator, be refused for lack of an operator, and be answered.
func TestRuntimeApprovalWithoutOperatorIsRefusedNotDropped(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	rt.emit(approvalEvent(snap.ThreadID, snap.TurnID, "req-1", "Bash", `{"command":"git status"}`))
	got := rt.waitApproval(t)

	if got.decision.Allow {
		t.Fatal("an unmediated approval was allowed")
	}
	if got.response.ID != "req-1" || got.response.SessionID != snap.ThreadID || got.response.TurnID != snap.TurnID {
		t.Errorf("answer misattributed: %+v", got.response)
	}
	feed, _ := json.Marshal(s.Feed(0))
	for _, want := range []string{`"kind":"request.refused"`, `"reason_code":"mediation_unavailable"`} {
		if !strings.Contains(string(feed), want) {
			t.Errorf("feed missing %q: %s", want, feed)
		}
	}
}

// TestRuntimeApprovalHonorsPolicyAllow proves a project policy rule
// resolves a claude approval without an operator, exactly as it does on
// codex — one `process.exec` rule governs the same command on both.
func TestRuntimeApprovalHonorsPolicyAllow(t *testing.T) {
	fixture(t, "backend")
	writePersonaPolicy(t, "backend", "allow", "gitstatus", "git status")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	rt.emit(approvalEvent(snap.ThreadID, snap.TurnID, "req-allow", "Bash", `{"command":"git status"}`))
	got := rt.waitApproval(t)
	if !got.decision.Allow {
		t.Fatalf("policy allow did not reach the runtime: %+v", got)
	}
	feed, _ := json.Marshal(s.Feed(0))
	if !strings.Contains(string(feed), `"kind":"request.allowed"`) {
		t.Errorf("feed missing request.allowed: %s", feed)
	}
}

// TestRuntimeApprovalHonorsPolicyDeny proves a deny rule blocks the tool
// call and is published to the audit feed.
func TestRuntimeApprovalHonorsPolicyDeny(t *testing.T) {
	fixture(t, "backend")
	writePersonaPolicy(t, "backend", "deny", "nocurl", "curl evil.example")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	rt.emit(approvalEvent(snap.ThreadID, snap.TurnID, "req-deny", "Bash", `{"command":"curl evil.example"}`))
	got := rt.waitApproval(t)
	if got.decision.Allow {
		t.Fatalf("policy deny was allowed: %+v", got)
	}
	if got.decision.Rationale == "" {
		t.Error("deny carried no rationale; claude requires a non-empty message")
	}
	feed, _ := json.Marshal(s.Feed(0))
	if !strings.Contains(string(feed), `"kind":"request.denied"`) {
		t.Errorf("feed missing request.denied: %s", feed)
	}
}

// TestRuntimeApprovalWithoutBindingIsDeniedDirectly proves the fail-closed
// boundary: an approval that cannot be bound to the live turn is answered
// (so the turn is never wedged) but never forwarded to the state machine,
// which would treat the unbindable tuple as a protocol violation.
func TestRuntimeApprovalWithoutBindingIsDeniedDirectly(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	// No turn attribution at all.
	rt.emit(approvalEvent(snap.ThreadID, "", "req-orphan", "Bash", `{"command":"whoami"}`))
	if got := rt.waitApproval(t); got.decision.Allow || got.response.ID != "req-orphan" {
		t.Fatalf("orphan approval answer = %+v", got)
	}
	// A payload with no tool name cannot be named in a policy.
	rt.emit(approvalEvent(snap.ThreadID, snap.TurnID, "req-nameless", "", `{}`))
	if got := rt.waitApproval(t); got.decision.Allow || got.response.ID != "req-nameless" {
		t.Fatalf("nameless approval answer = %+v", got)
	}
	if s.Snapshot().State == Failed {
		t.Fatal("an unbindable approval killed the session instead of being refused")
	}
}

// TestApprovalCommandExact pins how a runtime request is named for policy
// matching and for the audit feed.
func TestApprovalCommandExact(t *testing.T) {
	cases := []struct {
		name   string
		req    claude.ApprovalRequest
		want   string
		wantOK bool
	}{
		{
			name: "bash yields the command verbatim, matching codex",
			req:  claude.ApprovalRequest{ToolName: "Bash", InputJSON: json.RawMessage(`{"command":"git push --force","description":"push"}`)},
			want: "git push --force", wantOK: true,
		},
		{
			name: "powershell is a shell too",
			req:  claude.ApprovalRequest{ToolName: "PowerShell", InputJSON: json.RawMessage(`{"command":"Remove-Item x"}`)},
			want: "Remove-Item x", wantOK: true,
		},
		{
			name: "write is named by its path, never as a command line",
			req:  claude.ApprovalRequest{ToolName: "Write", InputJSON: json.RawMessage(`{"file_path":"/etc/passwd","content":"x"}`)},
			want: "Write(/etc/passwd)", wantOK: true,
		},
		{
			name: "webfetch is named by its url",
			req:  claude.ApprovalRequest{ToolName: "WebFetch", InputJSON: json.RawMessage(`{"url":"https://example.com","prompt":"read"}`)},
			want: "WebFetch(https://example.com)", wantOK: true,
		},
		{
			name: "an unrecognized tool still gets a stable descriptor",
			req:  claude.ApprovalRequest{ToolName: "Mystery", InputJSON: json.RawMessage(`{"whatever":1}`)},
			want: "Mystery()", wantOK: true,
		},
		{
			name: "a bash call with no command falls back to the descriptor",
			req:  claude.ApprovalRequest{ToolName: "Bash", InputJSON: json.RawMessage(`{}`)},
			want: "Bash()", wantOK: true,
		},
		{
			name:   "no tool name at all is unusable",
			req:    claude.ApprovalRequest{InputJSON: json.RawMessage(`{}`)},
			wantOK: false,
		},
		{
			name:   "an oversized command is refused rather than truncated",
			req:    claude.ApprovalRequest{ToolName: "Bash", InputJSON: json.RawMessage(`{"command":"` + strings.Repeat("a", maxApprovalCommandBytes+1) + `"}`)},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ApprovalCommandExact(tc.req)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("command=%q, want %q", got, tc.want)
			}
		})
	}
}

// TestApprovalDescriptorDoesNotMatchCommandRules guards the property that
// makes the shared naming safe: a non-shell descriptor cannot be approved
// by an exec rule written for a shell command, so it falls through to the
// operator.
func TestApprovalDescriptorDoesNotMatchCommandRules(t *testing.T) {
	const allowLS = "version: 1\nallow:\n  - id: ls\n    kind: process.exec\n    match:\n      command_exact: ls\n    reason: harmless\nask: []\ndeny: []\n"
	snapshot, diags := policy.Parse("backend", "root", []policy.Source{
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProject, Path: ".shipmates/policy.yaml", Present: true}, Bytes: []byte(allowLS)},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerPersona, Path: ".shipmates/policies/backend.yaml", Present: true},
			Bytes: []byte("version: 1\nallow: []\nask: []\ndeny: []\n")},
	})
	if snapshot == nil {
		t.Fatalf("policy did not parse: %v", diags)
	}
	command, ok := ApprovalCommandExact(claude.ApprovalRequest{ToolName: "Write", InputJSON: json.RawMessage(`{"file_path":"ls"}`)})
	if !ok {
		t.Fatal("descriptor not produced")
	}
	if got := policy.Evaluate(snapshot, policy.Request{Kind: policy.ProcessExec, CommandExact: command}); got.PolicyEffect != policy.Ask {
		t.Fatalf("Write(ls) evaluated as %q; a file write must never inherit a command rule", got.PolicyEffect)
	}
}
