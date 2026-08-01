package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fake LLM
// ---------------------------------------------------------------------------

// fakeLLM is a scripted OpenAI-compatible /chat/completions server. Each turn
// pops the next scripted reply; it records every request payload so tests can
// assert what the loop fed back to the model.
type fakeLLM struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []llmReply
	repeat   *llmReply // used once replies runs out
	requests []map[string]any
	status   int
	rawBody  string // when set, returned verbatim instead of a scripted reply
	authSeen string
}

type llmReply struct {
	content   string
	toolCalls []toolCall
}

func newFakeLLM(t *testing.T, replies ...llmReply) *fakeLLM {
	t.Helper()
	f := &fakeLLM{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)

		f.mu.Lock()
		f.requests = append(f.requests, payload)
		f.authSeen = r.Header.Get("Authorization")
		status, rawBody := f.status, f.rawBody
		var reply llmReply
		switch {
		case len(f.replies) > 0:
			reply, f.replies = f.replies[0], f.replies[1:]
		case f.repeat != nil:
			reply = *f.repeat
		default:
			reply = llmReply{content: "no script left"}
		}
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(rawBody))
			return
		}
		if rawBody != "" {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		out := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role":       "assistant",
					"content":    reply.content,
					"tool_calls": reply.toolCalls,
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

// newConvFleet returns a fleet wired to the given LLM base URL, with a real
// dialer so the tool implementations have something to enumerate.
func newConvFleet(t *testing.T, llmURL string) *Server {
	t.Helper()
	b := newTestFleet(t, "")
	b.conv = &convConfig{
		url:    strings.TrimRight(llmURL, "/"),
		model:  "test-model",
		client: &http.Client{Timeout: 30 * time.Second},
	}
	return b
}

func postConversation(t *testing.T, b *Server, body string) (*httptest.ResponseRecorder, conversationResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	b.handleConversation(rec, httptest.NewRequest("POST", "/api/conversation", strings.NewReader(body)))
	var out conversationResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &out); err != nil {
			t.Fatalf("decode conversation response %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

func TestPrependSystemPrompt(t *testing.T) {
	got := prependSystemPrompt([]chatMessage{{Role: "user", Content: "status?"}})
	if len(got) != 2 || got[0].Role != "system" {
		t.Fatalf("system prompt not prepended: %+v", got)
	}
	if got[1].Content != "status?" {
		t.Errorf("user turn lost: %+v", got[1])
	}
	if !strings.Contains(got[0].Content, "Commodore") {
		t.Errorf("system prompt is not the Commodore persona")
	}

	// A caller-supplied system message wins — we must not stack two.
	custom := []chatMessage{{Role: "system", Content: "be terse"}, {Role: "user", Content: "hi"}}
	again := prependSystemPrompt(custom)
	if len(again) != 2 || again[0].Content != "be terse" {
		t.Fatalf("caller's system message was overridden: %+v", again)
	}
}

func TestToolCallArgs(t *testing.T) {
	tc := toolCall{Function: toolCallFunc{Name: "tell_captain", Arguments: `{"captain_key":"a:b","persona":"data"}`}}
	args := tc.Args()
	if args["captain_key"] != "a:b" || args["persona"] != "data" {
		t.Fatalf("args decoded wrong: %+v", args)
	}
	// A model that emits malformed arguments must yield no usable args and no
	// panic — the tool impl then reports its usage error back to the model.
	// (A JSON `null` leaves the map nil; reading a nil map is safe in Go, so
	// callers behave identically.)
	for _, bad := range []string{"", "not json", `["a"]`, "null", `{"a":`} {
		got := toolCall{Function: toolCallFunc{Arguments: bad}}.Args()
		if len(got) != 0 {
			t.Errorf("Args() for %q should be empty, got %+v", bad, got)
		}
		if v, ok := got["captain_key"]; ok || v != nil {
			t.Errorf("Args() for %q yielded a value: %v", bad, v)
		}
	}
}

// knownTools gates which rescued text tool calls are executable. A tool added
// to the catalog but not to knownTools is silently un-rescuable; one in
// knownTools but not dispatchable answers "unknown tool" to the model.
func TestToolCatalogAndKnownToolsAgree(t *testing.T) {
	b := newTestFleet(t, "")
	raw, err := json.Marshal(b.toolCatalog())
	if err != nil {
		t.Fatal(err)
	}
	var catalog []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog) == 0 {
		t.Fatal("empty tool catalog")
	}
	inCatalog := map[string]bool{}
	for _, tool := range catalog {
		if tool.Type != "function" {
			t.Errorf("%s: want type=function, got %q", tool.Function.Name, tool.Type)
		}
		if tool.Function.Description == "" {
			t.Errorf("%s: tools need a description or the model won't pick them", tool.Function.Name)
		}
		if tool.Function.Parameters == nil {
			t.Errorf("%s: missing parameters schema", tool.Function.Name)
		}
		inCatalog[tool.Function.Name] = true
	}
	for name := range knownTools {
		if !inCatalog[name] {
			t.Errorf("knownTools has %q but the catalog does not offer it", name)
		}
	}
	for name := range inCatalog {
		if !knownTools[name] {
			t.Errorf("catalog offers %q but knownTools won't let it be rescued", name)
		}
	}
}

// Every catalog tool must be dispatchable; "unknown tool" is reserved for
// names the model invented.
func TestDispatchTool_CoversEveryKnownTool(t *testing.T) {
	b := newTestFleet(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name := range knownTools {
		got := b.dispatchTool(ctx, toolCall{Function: toolCallFunc{Name: name, Arguments: "{}"}})
		if strings.Contains(got, "unknown tool") {
			t.Errorf("catalog tool %q is not dispatchable: %s", name, got)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("tool %q returned non-JSON: %s", name, got)
		}
	}
	got := b.dispatchTool(ctx, toolCall{Function: toolCallFunc{Name: "definitely_not_a_tool", Arguments: "{}"}})
	if !strings.Contains(got, "unknown tool") {
		t.Errorf("invented tool name should report unknown, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// llmChat
// ---------------------------------------------------------------------------

func TestLLMChat_StripsThinkBlocks(t *testing.T) {
	llm := newFakeLLM(t, llmReply{content: "<think>\nlots of reasoning\n</think>\nAye, Admiral. All quiet."})
	b := newConvFleet(t, llm.srv.URL)

	got, err := b.llmChat(context.Background(), []chatMessage{{Role: "user", Content: "status?"}}, nil)
	if err != nil {
		t.Fatalf("llmChat: %v", err)
	}
	if got.Content != "Aye, Admiral. All quiet." {
		t.Fatalf("reasoning trace leaked into the spoken reply: %q", got.Content)
	}
}

func TestLLMChat_SendsModelToolsAndKey(t *testing.T) {
	llm := newFakeLLM(t, llmReply{content: "ok"})
	b := newConvFleet(t, llm.srv.URL)
	b.conv.key = "sk-test"

	if _, err := b.llmChat(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, b.toolCatalog()); err != nil {
		t.Fatalf("llmChat: %v", err)
	}
	calls := llm.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 llm call, got %d", len(calls))
	}
	if calls[0]["model"] != "test-model" {
		t.Errorf("model not sent: %v", calls[0]["model"])
	}
	if calls[0]["stream"] != false {
		t.Errorf("stream must be false: %v", calls[0]["stream"])
	}
	if tools, _ := calls[0]["tools"].([]any); len(tools) == 0 {
		t.Errorf("tool catalog not sent")
	}
	llm.mu.Lock()
	auth := llm.authSeen
	llm.mu.Unlock()
	if auth != "Bearer sk-test" {
		t.Errorf("bearer key not forwarded, got %q", auth)
	}
}

func TestLLMChat_Errors(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*fakeLLM)
		wantSub string
	}{
		{"http error", func(f *fakeLLM) { f.status = 500; f.rawBody = "model not loaded" }, "500"},
		{"error envelope", func(f *fakeLLM) { f.rawBody = `{"error":{"message":"context length exceeded"}}` }, "context length exceeded"},
		{"no choices", func(f *fakeLLM) { f.rawBody = `{"choices":[]}` }, "no choices"},
		{"undecodable", func(f *fakeLLM) { f.rawBody = `<html>gateway</html>` }, "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := newFakeLLM(t)
			tc.setup(llm)
			b := newConvFleet(t, llm.srv.URL)
			_, err := b.llmChat(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err, tc.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleConversation
// ---------------------------------------------------------------------------

func TestHandleConversation_DisabledIs503(t *testing.T) {
	b := newTestFleet(t, "")
	rec := httptest.NewRecorder()
	b.handleConversation(rec, httptest.NewRequest("POST", "/api/conversation", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 with no backend configured, got %d", rec.Code)
	}

	// conv present but neither backend configured is still disabled.
	b.conv = &convConfig{client: &http.Client{}}
	rec = httptest.NewRecorder()
	b.handleConversation(rec, httptest.NewRequest("POST", "/api/conversation", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 for an empty conv config, got %d", rec.Code)
	}
}

func TestHandleConversation_BadBodyIs400(t *testing.T) {
	llm := newFakeLLM(t)
	b := newConvFleet(t, llm.srv.URL)
	rec := httptest.NewRecorder()
	b.handleConversation(rec, httptest.NewRequest("POST", "/api/conversation", strings.NewReader(`not json`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestHandleConversation_PlainReply(t *testing.T) {
	llm := newFakeLLM(t, llmReply{content: "Aye, Admiral. Two captains on watch."})
	b := newConvFleet(t, llm.srv.URL)

	rec, out := postConversation(t, b, `{"messages":[{"role":"user","content":"who is on watch?"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	// The heartbeat needs buffering off or an edge proxy swallows the keepalive.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering must be 'no'")
	}
	if out.Reply != "Aye, Admiral. Two captains on watch." {
		t.Fatalf("reply: %q", out.Reply)
	}
	if out.Error != "" {
		t.Errorf("unexpected error: %q", out.Error)
	}
	if len(out.ToolsCalled) != 0 {
		t.Errorf("no tools should have been called: %+v", out.ToolsCalled)
	}
	// The system prompt must ride the very first request.
	calls := llm.calls()
	msgs, _ := calls[0]["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want system+user, got %d messages", len(msgs))
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Errorf("first message is not the system prompt: %+v", msgs[0])
	}
}

// The tool loop: the model asks for a tool, the fleet runs it, feeds the
// result back as a role:"tool" message, and the model's next turn is the
// spoken reply.
func TestHandleConversation_RunsToolThenReplies(t *testing.T) {
	llm := newFakeLLM(t,
		llmReply{toolCalls: []toolCall{{ID: "call_1", Type: "function",
			Function: toolCallFunc{Name: "list_captains", Arguments: "{}"}}}},
		llmReply{content: "Aye, Admiral. One ship on station."},
	)
	b := newConvFleet(t, llm.srv.URL)
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Repo: "homelab", Persona: "picard"}

	_, out := postConversation(t, b, `{"messages":[{"role":"user","content":"who is out there?"}]}`)
	if out.Reply != "Aye, Admiral. One ship on station." {
		t.Fatalf("reply: %q (err %q)", out.Reply, out.Error)
	}
	if len(out.ToolsCalled) != 1 || out.ToolsCalled[0].Function.Name != "list_captains" {
		t.Fatalf("tools_called not reported: %+v", out.ToolsCalled)
	}

	calls := llm.calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 llm round trips, got %d", len(calls))
	}
	msgs, _ := calls[1]["messages"].([]any)
	var sawAssistantToolCall, sawToolResult bool
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		switch mm["role"] {
		case "assistant":
			if tc, ok := mm["tool_calls"].([]any); ok && len(tc) > 0 {
				sawAssistantToolCall = true
			}
		case "tool":
			sawToolResult = true
			if mm["tool_call_id"] != "call_1" {
				t.Errorf("tool result not correlated to the call id: %+v", mm)
			}
			if mm["name"] != "list_captains" {
				t.Errorf("tool result missing its name: %+v", mm)
			}
			if s, _ := mm["content"].(string); !strings.Contains(s, "homelab:captain") {
				t.Errorf("tool result content wrong: %v", mm["content"])
			}
		}
	}
	if !sawAssistantToolCall {
		t.Error("the assistant's own tool_calls turn was not replayed to the model")
	}
	if !sawToolResult {
		t.Error("the tool result was never fed back")
	}
}

// A model stuck in a tool loop must be cut off rather than pinning the fleet.
func TestHandleConversation_IterationCap(t *testing.T) {
	llm := newFakeLLM(t)
	llm.repeat = &llmReply{toolCalls: []toolCall{{ID: "c", Type: "function",
		Function: toolCallFunc{Name: "list_captains", Arguments: "{}"}}}}
	b := newConvFleet(t, llm.srv.URL)

	_, out := postConversation(t, b, `{"messages":[{"role":"user","content":"loop forever"}]}`)
	if out.Error == "" {
		t.Fatalf("want an error envelope, got reply %q", out.Reply)
	}
	if !strings.Contains(out.Error, fmt.Sprint(conversationMaxIterations)) {
		t.Errorf("error should name the cap: %q", out.Error)
	}
	if n := len(llm.calls()); n != conversationMaxIterations {
		t.Errorf("want exactly %d llm calls, got %d", conversationMaxIterations, n)
	}
}

// Small models fall out of function-calling format and emit `tool_name {json}`
// as prose. Left alone that becomes a "reply" of raw JSON, spoken aloud, with
// the work never done.
func TestHandleConversation_RescuesTextFormattedToolCalls(t *testing.T) {
	llm := newFakeLLM(t,
		llmReply{content: `Tell_captain {"captain_key": "homelab:captain", "persona": "captain", "message": "/standup"}.`},
		llmReply{content: "Aye, Admiral. Order passed."},
	)
	b := newConvFleet(t, llm.srv.URL)

	_, out := postConversation(t, b, `{"messages":[{"role":"user","content":"tell homelab to stand up"}]}`)
	if len(out.ToolsCalled) != 1 || out.ToolsCalled[0].Function.Name != "tell_captain" {
		t.Fatalf("text-formatted tool call was not rescued: %+v (reply %q)", out.ToolsCalled, out.Reply)
	}
	if out.Reply != "Aye, Admiral. Order passed." {
		t.Errorf("reply: %q", out.Reply)
	}
	if strings.Contains(out.Reply, "{") {
		t.Errorf("raw JSON leaked into the spoken reply: %q", out.Reply)
	}
}

// LLM failures ride the 200 envelope: by the time a slow turn fails the
// heartbeat has already committed the response headers.
func TestHandleConversation_LLMFailureRidesThe200Envelope(t *testing.T) {
	llm := newFakeLLM(t)
	llm.status = 500
	llm.rawBody = "model not loaded"
	b := newConvFleet(t, llm.srv.URL)

	rec, out := postConversation(t, b, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with an error envelope, got %d", rec.Code)
	}
	if !strings.HasPrefix(out.Error, "llm:") {
		t.Fatalf("want an llm error in the envelope, got %+v", out)
	}
}

// ---------------------------------------------------------------------------
// rescueTextToolCalls edge cases (beyond the existing rescue_test.go)
// ---------------------------------------------------------------------------

func TestRescueTextToolCalls_RejectsUnknownAndMalformed(t *testing.T) {
	cases := []struct{ name, content string }{
		{"unknown tool name", `frobnicate {"x":1}`},
		{"invalid json args", `tell_captain {not json}`},
		{"prose mentioning a tool", "I will use tell_captain to reach the captain."},
		{"json with no tool name", `{"captain_key":"a:b"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rescueTextToolCalls(tc.content); len(got) != 0 {
				t.Fatalf("should not have rescued anything, got %+v", got)
			}
		})
	}
}

func TestRescueTextToolCalls_AssignsDistinctIDs(t *testing.T) {
	content := "tell_captain {\"a\":1}\nlist_captains {}\nfleet_status {}\n"
	got := rescueTextToolCalls(content)
	if len(got) != 3 {
		t.Fatalf("want 3 rescued calls, got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, tc := range got {
		if tc.ID == "" {
			t.Errorf("rescued call has no id: %+v", tc)
		}
		if seen[tc.ID] {
			t.Errorf("duplicate rescued id %q — tool results would be mis-correlated", tc.ID)
		}
		seen[tc.ID] = true
		if tc.Type != "function" {
			t.Errorf("rescued call type %q", tc.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// tool implementations
// ---------------------------------------------------------------------------

func TestToolListCaptains(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")
	b.mu.Lock()
	b.captains["laptop:captain"] = &Captain{ClientKey: "laptop:captain", Repo: "laptop", Persona: "data"}
	b.mu.Unlock()

	got := decodeJSON[[]map[string]any](t, []byte(b.toolListCaptains()))
	if len(got) != 2 {
		t.Fatalf("want 2 captains, got %d", len(got))
	}
	conn := map[string]bool{}
	for _, c := range got {
		conn[c["client_key"].(string)] = c["connected"].(bool)
	}
	if !conn["homelab:captain"] || conn["laptop:captain"] {
		t.Fatalf("connected flags wrong: %+v", conn)
	}
}

func TestToolArgumentValidation(t *testing.T) {
	b := newTestFleet(t, "")
	ctx := context.Background()
	cases := []struct {
		name string
		got  string
	}{
		{"tell_captain without args", b.toolTellCaptain(ctx, map[string]any{})},
		{"tell_captain without message", b.toolTellCaptain(ctx, map[string]any{"captain_key": "a:b", "persona": "data"})},
		{"recent_events without key", b.toolRecentEvents(ctx, map[string]any{})},
		{"wait_for_result without key", b.toolWaitForResult(ctx, map[string]any{})},
		{"resolve without behavior", b.toolResolve(ctx, map[string]any{"captain_key": "a:b", "id": "p1"})},
		{"resolve with a bogus behavior", b.toolResolve(ctx, map[string]any{"captain_key": "a:b", "id": "p1", "behavior": "maybe"})},
		{"tell_all_captains without message", b.toolTellAllCaptains(ctx, map[string]any{})},
		{"tell_all_captains with blank message", b.toolTellAllCaptains(ctx, map[string]any{"message": "   "})},
		{"dispatch_bead without args", b.toolDispatchBead(ctx, map[string]any{})},
		{"dispatch_bead with a traversal id", b.toolDispatchBead(ctx, map[string]any{"bead_id": "../x", "captain_key": "a:b", "persona": "data"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(tc.got), &out); err != nil {
				t.Fatalf("tool result is not JSON: %s", tc.got)
			}
			if out.Error == "" {
				t.Fatalf("want an error result, got %s", tc.got)
			}
		})
	}
}

func TestToolTellAllCaptains_NoShipsIsAnError(t *testing.T) {
	b := newTestFleet(t, "")
	got := b.toolTellAllCaptains(context.Background(), map[string]any{"message": "/standup"})
	if !strings.Contains(got, "no ships connected") {
		t.Fatalf("want a 'no ships connected' error, got %s", got)
	}
}

// The broadcast must address each ship's OWN captain persona, not a hardcoded
// "captain" — a ship whose lead is "picard" would never hear the order.
func TestToolTellAllCaptains_UsesEachShipsPersona(t *testing.T) {
	a := newFakeShip(t)
	c := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")
	b.mu.Lock()
	b.captains["homelab:captain"].Persona = "picard"
	b.captains["laptop:captain"].Persona = "" // no persona recorded → default
	b.mu.Unlock()

	got := decodeJSON[map[string]string](t, []byte(b.toolTellAllCaptains(context.Background(), map[string]any{"message": "/standup"})))
	if got["homelab:captain"] != "sent" || got["laptop:captain"] != "sent" {
		t.Fatalf("broadcast results: %+v", got)
	}
	if n := len(a.hits("POST /tell/picard")); n != 1 {
		t.Errorf("homelab should have been told via /tell/picard, hits=%d (%+v)", n, a.allHits())
	}
	if n := len(c.hits("POST /tell/captain")); n != 1 {
		t.Errorf("persona-less ship should default to /tell/captain, hits=%d (%+v)", n, c.allHits())
	}
}

func TestToolTellAllCaptains_ReportsPerShipFailure(t *testing.T) {
	a := newFakeShip(t)
	c := newFakeShip(t)
	c.status["POST /tell/captain"] = http.StatusInternalServerError
	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")

	got := decodeJSON[map[string]string](t, []byte(b.toolTellAllCaptains(context.Background(), map[string]any{"message": "/standup"})))
	if got["homelab:captain"] != "sent" {
		t.Errorf("healthy ship: %q", got["homelab:captain"])
	}
	if got["laptop:captain"] != "failed" {
		t.Errorf("failing ship must be reported as failed, got %q", got["laptop:captain"])
	}
}

func TestToolTellCaptain_ThroughTunnel(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	got := b.toolTellCaptain(context.Background(), map[string]any{
		"captain_key": "homelab:captain", "persona": "data", "message": "/standup",
	})
	if got != `{"ok": true}` {
		t.Fatalf("tell result: %s", got)
	}
	hits := ship.hits("POST /tell/data")
	if len(hits) != 1 {
		t.Fatalf("want 1 tell, got %d", len(hits))
	}
	var payload map[string]string
	if err := json.Unmarshal(hits[0].body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["message"] != "/standup" {
		t.Errorf("message not forwarded: %+v", payload)
	}
}

func TestToolTellCaptain_ShipErrorSurfaces(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["POST /tell/data"] = http.StatusInternalServerError
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	got := b.toolTellCaptain(context.Background(), map[string]any{
		"captain_key": "homelab:captain", "persona": "data", "message": "hi",
	})
	if !strings.Contains(got, "500") {
		t.Fatalf("the ship's status should reach the model, got %s", got)
	}
}

func TestToolRecentEvents_AppliesLimit(t *testing.T) {
	ship := newFakeShip(t)
	events := make([]map[string]string, 50)
	for i := range events {
		events[i] = map[string]string{"time": fmt.Sprintf("2026-01-01T00:00:%02d", i), "text": fmt.Sprint(i)}
	}
	ship.setJSON("GET /events", events)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")
	ctx := context.Background()

	// Default limit is 20, taken from the TAIL of the log (newest events).
	got := decodeJSON[[]map[string]any](t, []byte(b.toolRecentEvents(ctx, map[string]any{"captain_key": "homelab:captain"})))
	if len(got) != 20 {
		t.Fatalf("default limit: want 20, got %d", len(got))
	}
	if got[len(got)-1]["text"] != "49" {
		t.Errorf("limit must keep the NEWEST events, got last=%v", got[len(got)-1]["text"])
	}

	got5 := decodeJSON[[]map[string]any](t, []byte(b.toolRecentEvents(ctx, map[string]any{"captain_key": "homelab:captain", "limit": float64(5)})))
	if len(got5) != 5 {
		t.Fatalf("explicit limit: want 5, got %d", len(got5))
	}

	// Fewer events than the limit is not an error.
	ship.setJSON("GET /events", events[:2])
	got2 := decodeJSON[[]map[string]any](t, []byte(b.toolRecentEvents(ctx, map[string]any{"captain_key": "homelab:captain"})))
	if len(got2) != 2 {
		t.Fatalf("want 2, got %d", len(got2))
	}
}

func TestToolFleetStatus_FlattensShips(t *testing.T) {
	a := newFakeShip(t)
	a.setJSON("GET /status.json", []map[string]string{{"persona": "picard", "status": "working"}})
	c := newFakeShip(t)
	c.setJSON("GET /status.json", []map[string]string{{"persona": "data", "status": "off"}})
	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")

	got := decodeJSON[[]map[string]string](t, []byte(b.toolFleetStatus(context.Background())))
	if len(got) != 2 {
		t.Fatalf("want 2 mates, got %d", len(got))
	}
	byShip := map[string]string{}
	for _, m := range got {
		byShip[m["ship"]] = m["status"]
	}
	if byShip["homelab:captain"] != "working" || byShip["laptop:captain"] != "off" {
		t.Fatalf("statuses mis-attributed: %+v", byShip)
	}
}

func TestToolListBeads_DedupesAcrossShips(t *testing.T) {
	shared := []map[string]any{{"id": "aaa", "title": "shared", "status": "open", "assignee": "data@homelab"}}
	a := newFakeShip(t)
	a.setJSON("GET /beads.json", shared)
	c := newFakeShip(t)
	c.setJSON("GET /beads.json", shared)
	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")

	got := decodeJSON[[]map[string]any](t, []byte(b.toolListBeads(context.Background())))
	if len(got) != 1 {
		t.Fatalf("shared graph must dedupe to 1 bead, got %d: %+v", len(got), got)
	}
	ships, _ := got[0]["ships"].([]any)
	if len(ships) != 2 {
		t.Fatalf("want both ships listed, got %v", ships)
	}
	if got[0]["assignee"] != "data@homelab" {
		t.Errorf("assignee dropped: %+v", got[0])
	}
}

func TestToolDispatchBead_RejectsOfflineShip(t *testing.T) {
	b := newTestFleet(t, "")
	got := b.toolDispatchBead(context.Background(), map[string]any{
		"bead_id": "abc123", "captain_key": "ghost:captain", "persona": "data",
	})
	if !strings.Contains(got, "not connected") {
		t.Fatalf("want a 'not connected' error, got %s", got)
	}
}

// Small models guess bead ids. When the id doesn't exist the tool must hand
// the real graph back so the model can self-correct instead of inventing an
// excuse for the operator — and it must NOT have mutated anything first.
func TestToolDispatchBead_UnknownIDReturnsTheGraph(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /beads.json", []map[string]any{{"id": "real1", "title": "the real one", "status": "open"}})
	ship.status["GET /bead/guessed"] = http.StatusNotFound
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	got := b.toolDispatchBead(context.Background(), map[string]any{
		"bead_id": "guessed", "captain_key": "homelab:captain", "persona": "data",
	})
	if !json.Valid([]byte(got)) {
		t.Fatalf("result is not JSON: %s", got)
	}
	if !strings.Contains(got, "real1") {
		t.Errorf("the open bead graph should be handed back: %s", got)
	}
	if n := len(ship.hits("POST /bead/guessed/update")); n != 0 {
		t.Errorf("must not mutate the assignee for an id that doesn't exist")
	}
	if n := len(ship.hits("POST /tell/data")); n != 0 {
		t.Errorf("must not wake the mate for an id that doesn't exist")
	}
}

func TestToolDispatchBead_HappyPath(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /bead/real1", map[string]any{"id": "real1", "title": "the real one"})
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	got := decodeJSON[map[string]string](t, []byte(b.toolDispatchBead(context.Background(), map[string]any{
		"bead_id": "real1", "captain_key": "homelab:captain", "persona": "data",
	})))
	if got["ok"] != "true" {
		t.Fatalf("dispatch failed: %+v", got)
	}
	// "persona@ship" — the ship NAME, not the full client key.
	if got["assignee"] != "data@homelab" {
		t.Fatalf("assignee should be persona@shipname, got %q", got["assignee"])
	}
	upd := ship.hits("POST /bead/real1/update")
	if len(upd) != 1 {
		t.Fatalf("want 1 assignee update, got %d", len(upd))
	}
	var body map[string]string
	if err := json.Unmarshal(upd[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body["assignee"] != "data@homelab" {
		t.Errorf("update body: %+v", body)
	}
	if n := len(ship.hits("POST /tell/data")); n != 1 {
		t.Errorf("the mate was not woken, tells=%d", n)
	}
	// Graph sync must happen BEFORE the id check.
	if n := len(ship.hits("POST /beads/pull")); n != 1 {
		t.Errorf("want 1 graph pull, got %d", n)
	}
}

func TestToolWaitForResult_Timeout(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /events", []map[string]string{{"time": "2026-01-01T00:00:00Z", "type": "assistant", "text": "old"}})
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	start := time.Now()
	got := b.toolWaitForResult(context.Background(), map[string]any{
		"captain_key": "homelab:captain", "timeout_sec": float64(1),
	})
	if !strings.Contains(got, "timeout") {
		t.Fatalf("want a timeout error, got %s", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout_sec was not honored, waited %v", elapsed)
	}
}

// Only events NEWER than the snapshot count: the captain's log carries results
// from prior turns, and returning one of those would report the wrong answer.
func TestToolWaitForResult_ReturnsNewResultOnly(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /events", []map[string]any{
		{"time": "2026-01-01T00:00:01Z", "type": "assistant", "text": "stale answer"},
		{"time": "2026-01-01T00:00:02Z", "type": "result"},
	})
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	done := make(chan string, 1)
	go func() {
		done <- b.toolWaitForResult(context.Background(), map[string]any{
			"captain_key": "homelab:captain", "timeout_sec": float64(15),
		})
	}()

	// Let the snapshot land, then append this turn's events.
	time.Sleep(200 * time.Millisecond)
	ship.setJSON("GET /events", []map[string]any{
		{"time": "2026-01-01T00:00:01Z", "type": "assistant", "text": "stale answer"},
		{"time": "2026-01-01T00:00:02Z", "type": "result"},
		{"time": "2026-01-01T00:00:03Z", "type": "assistant", "text": "fresh answer"},
		{"time": "2026-01-01T00:00:04Z", "type": "result"},
	})

	select {
	case got := <-done:
		var out struct {
			FinalText string `json:"final_text"`
		}
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatalf("result is not JSON: %s", got)
		}
		if out.FinalText != "fresh answer" {
			t.Fatalf("want the NEW turn's text, got %q", out.FinalText)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("wait_for_result never returned")
	}
}

func TestToolPendingApprovals(t *testing.T) {
	a := newFakeShip(t)
	a.setJSON("GET /pending.json", []map[string]string{{"id": "p1", "persona": "picard", "tool": "Bash", "input": "rm -rf /"}})
	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")

	got := decodeJSON[[]map[string]string](t, []byte(b.toolPendingApprovals(context.Background())))
	if len(got) != 1 {
		t.Fatalf("want 1 pending approval, got %d", len(got))
	}
	if got[0]["client_key"] != "homelab:captain" || got[0]["repo"] != "homelab" {
		t.Errorf("approval not attributed to its ship: %+v", got[0])
	}
	if got[0]["input"] != "rm -rf /" {
		t.Errorf("the input the operator has to judge was dropped: %+v", got[0])
	}
}

func TestToolPendingApprovals_EmptyIsArray(t *testing.T) {
	b := newTestFleet(t, "")
	if got := b.toolPendingApprovals(context.Background()); got != "[]" {
		t.Fatalf("want [], got %s", got)
	}
}

func TestToolResolve_ThroughTunnel(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	got := b.toolResolve(context.Background(), map[string]any{
		"captain_key": "homelab:captain", "id": "p1", "behavior": "deny",
	})
	if got != `{"ok": true}` {
		t.Fatalf("resolve: %s", got)
	}
	hits := ship.hits("POST /resolve/p1")
	if len(hits) != 1 {
		t.Fatalf("want 1 resolve, got %d", len(hits))
	}
	var body map[string]string
	if err := json.Unmarshal(hits[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body["behavior"] != "deny" {
		t.Errorf("behavior not forwarded: %+v", body)
	}
}
