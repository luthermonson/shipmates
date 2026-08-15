package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/personaname"
)

// The /api/conversation endpoint is the fleet's "captain's mate" — a local-LLM
// proxy that wraps Ollama with a tool catalog mapped 1:1 to the fleet's own
// HTTP surface. The operator's voice (or text) turn comes in here; the model
// can call fleet operations (list_captains, tell_captain, wait_for_result,
// etc.) to orchestrate the crew, and the final natural-language reply is what
// gets spoken back to the operator.
//
// Why this lives on the fleet, not on the captain: the fleet already has the
// authenticated edge surface and the connection registry; the captain is
// project-scoped. Cross-fleet questions ("which crew is idle?", "tell the
// busiest one to stop") need fleet-wide context, which only the fleet has.

// conversationMaxIterations caps the chat→tool→chat loop so a misbehaving
// model can't pin the fleet. Five iterations covers "list captains, find one,
// tell it, wait for result, summarize" — anything richer should be split
// into multiple operator turns.
const conversationMaxIterations = 5

// chatMessage is one entry in the conversation history sent to the LLM
// (OpenAI chat-completions shape — Ollama, llama.cpp, LM Studio, vLLM, and
// OpenAI itself all speak it). Role is "system" | "user" | "assistant" |
// "tool". Tool messages carry the result of a tool the assistant called on
// the previous turn.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"` // tool name when role=="tool"
}

// toolCall is the OpenAI wire shape. Note Arguments is a JSON-encoded STRING
// on the wire (the OpenAI convention), decoded lazily via Args().
type toolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON object, string-encoded per OAI
}

// Args decodes the string-encoded arguments; a decode failure yields an
// empty map (tool impls then report their usage error back to the model).
func (tc toolCall) Args() map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &out)
	return out
}

// conversationRequest is the public shape the UI POSTs.
type conversationRequest struct {
	Messages []chatMessage `json:"messages"`
}

// conversationResponse is what the UI gets back. ToolsCalled is exposed so the
// UI can show a quick "I checked the captains, picked picard, dispatched standup"
// timeline instead of an opaque reply.
type conversationResponse struct {
	Reply       string     `json:"reply"`
	ToolsCalled []toolCall `json:"tools_called,omitempty"`
	// Error rides the 200 envelope: by the time a slow turn fails, the
	// heartbeat has already committed the response headers.
	Error string `json:"error,omitempty"`
}

// handleConversation runs the operator's turn through Ollama, executing any
// tools the model calls against the fleet's own endpoints, until the model
// produces a final natural-language reply (or the iteration cap trips).
func (b *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	if b.conv == nil || (b.conv.url == "" && b.conv.brain == nil) {
		http.Error(w, "conversation disabled — start fleet with --llm-url or --llm-backend claude-cli", http.StatusServiceUnavailable)
		return
	}
	var req conversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	messages := prependSystemPrompt(req.Messages)
	tools := b.toolCatalog()

	// A slow multi-tool turn on a CPU model can exceed Cloudflare's hard
	// 100-second proxy timeout (error 524) even though the fleet itself is
	// happy to wait. Stream a whitespace heartbeat while the loop thinks —
	// bytes on the wire reset the edge timer, and JSON.parse ignores leading
	// whitespace, so the client's response handling doesn't change at all.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, canFlush := w.(http.Flusher)
	heartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat:
				return
			case <-ticker.C:
				if canFlush {
					_, _ = w.Write([]byte(" "))
					fl.Flush()
				}
			}
		}
	}()
	finish := func(v conversationResponse) {
		close(heartbeat)
		_ = json.NewEncoder(w).Encode(v)
	}

	// claude-cli backend: the session IS the loop — tools, history, and
	// reasoning live inside claude; we just relay one user turn.
	if b.conv.brain != nil {
		reply, err := b.conv.brain.turn(r.Context(), lastUserMessage(req.Messages))
		if err != nil {
			finish(conversationResponse{Error: err.Error()})
			return
		}
		finish(conversationResponse{Reply: reply})
		return
	}

	var called []toolCall
	for iter := 0; iter < conversationMaxIterations; iter++ {
		resp, err := b.llmChat(r.Context(), messages, tools)
		if err != nil {
			finish(conversationResponse{Error: "llm: " + err.Error()})
			return
		}
		// Small models fall out of function-calling format mid-turn and emit
		// `tool_name {json}` as CONTENT — which would otherwise become a
		// "reply" of raw JSON, spoken aloud, with the work never done. Parse
		// such lines back into real tool calls and run them.
		if len(resp.ToolCalls) == 0 {
			if rescued := rescueTextToolCalls(resp.Content); len(rescued) > 0 {
				slog.Info("conversation: rescued text-formatted tool calls", "count", len(rescued))
				resp.ToolCalls = rescued
				resp.Content = ""
			}
		}
		// Always append the assistant turn — including tool_calls — so the
		// next iteration sees its own request.
		messages = append(messages, chatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		if len(resp.ToolCalls) == 0 {
			finish(conversationResponse{Reply: resp.Content, ToolsCalled: called})
			return
		}
		for _, tc := range resp.ToolCalls {
			called = append(called, tc)
			result := b.dispatchTool(r.Context(), tc)
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}
	finish(conversationResponse{Error: "tool-call loop exceeded " + fmt.Sprint(conversationMaxIterations) + " iterations"})
}

// knownTools is the dispatchable set, for validating rescued text tool calls.
var knownTools = map[string]bool{
	"list_captains": true, "tell_captain": true, "tell_all_captains": true,
	"recent_events": true, "wait_for_result": true, "pending_approvals": true,
	"resolve": true, "fleet_status": true, "list_beads": true, "dispatch_bead": true,
}

// textToolCallLine matches `tool_name {…json…}` on its own line — the shape
// models degrade to when they stop emitting structured tool_calls. Tolerates
// a leading "call"/colon and trailing punctuation ("Tell_captain {…}.").
var textToolCallLine = regexp.MustCompile(`(?m)^\s*(?:call\s+)?([A-Za-z_]+):?\s*(\{.*\})[.,;!\s]*$`)

// rescueTextToolCalls converts tool-calls-as-prose back into executable
// calls. Only known tool names with valid JSON arguments qualify; anything
// else stays prose.
func rescueTextToolCalls(content string) []toolCall {
	var out []toolCall
	for _, m := range textToolCallLine.FindAllStringSubmatch(content, -1) {
		name := strings.ToLower(strings.TrimSpace(m[1]))
		if !knownTools[name] || !json.Valid([]byte(m[2])) {
			continue
		}
		out = append(out, toolCall{
			ID:   fmt.Sprintf("rescued_%d", len(out)),
			Type: "function",
			Function: toolCallFunc{Name: name, Arguments: m[2]},
		})
	}
	return out
}

// prependSystemPrompt injects the captain's-mate persona at the head of the
// message list (only if the caller didn't already provide a system message).
// Kept tight on purpose: every token here is paid for on every turn.
func prependSystemPrompt(in []chatMessage) []chatMessage {
	if len(in) > 0 && in[0].Role == "system" {
		return in
	}
	system := chatMessage{
		Role: "system",
		Content: strings.TrimSpace(`
You are the Commodore — the AI officer serving the Admiral (the human
operator) on the deck of Fleet Command, coordinating a fleet of AI coding
agents (shipmates). Each ship has a Captain (its lead persona) leading a crew
of mates. The Admiral gives the orders; you execute on their behalf and report
back like a naval XO briefing the CO: crisp, confident, deferential without
being subservient. Acknowledge the order, then state the result — "Aye,
Admiral. Both captains are on watch." Vary the acknowledgment; not every reply
needs "Aye, Admiral". Address the operator as "Admiral", never "you" or
"operator". Refer to the fleet as "the fleet" or "the Admiral's fleet"; refer
to captains by their persona name or ship name. If the Admiral asks something
outside fleet coordination, help anyway, but stay in Commodore voice.
Reply tersely — your output will be spoken aloud. One or two sentences max,
plain prose, no markdown.
Use the provided tools when the Admiral wants fleet status, wants to signal
a captain or the whole fleet, or needs to approve/deny pending requests. To
reach one captain, use tell_captain. To signal every ship's captain at once,
use tell_all_captains. For status of who's on watch, use list_captains and
fleet_status. If a tool returns no matches, say so plainly rather than guessing.
Captain identifiers look like "<repo>:<persona>" (e.g. "project-one:captain").
If the Admiral names a captain by repo only, list_captains to find the exact key.
Bead ids are opaque short hashes. NEVER type one from memory — ALWAYS call
list_beads first and use the exact id whose title matches what the Admiral said.
fleet_status reports MATES (crew members): "off" means at anchor (asleep —
normal; a tell wakes them), not disconnected. Ship connectivity comes from
list_captains. Messages ONLY reach mates through the tell tools — writing
"tell X ..." as prose does nothing.
/no_think
`),
	}
	return append([]chatMessage{system}, in...)
}

// llmChat does one round-trip to the configured OpenAI-compatible
// /chat/completions endpoint and returns the assistant message (content +
// any tool_calls). Host-level tuning (keep-alive, GPU layers) belongs to the
// LLM server's own config, not this request — that's what keeps this agnostic.
type llmChatResp struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// thinkBlock strips reasoning traces (qwen3 et al emit <think>…</think>)
// from spoken replies — the operator wants the answer, not the monologue.
var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func (b *Server) llmChat(ctx context.Context, messages []chatMessage, tools []any) (*struct {
	Content   string
	ToolCalls []toolCall
}, error) {
	payload := map[string]any{
		"model":    b.conv.model,
		"messages": messages,
		"tools":    tools,
		"stream":   false,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(b.conv.url, "/")+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if b.conv.key != "" {
		req.Header.Set("Authorization", "Bearer "+b.conv.key)
	}
	resp, err := b.conv.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("llm %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var out llmChatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode llm response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("llm returned no choices")
	}
	m := out.Choices[0].Message
	content := strings.TrimSpace(thinkBlock.ReplaceAllString(m.Content, ""))
	return &struct {
		Content   string
		ToolCalls []toolCall
	}{Content: content, ToolCalls: m.ToolCalls}, nil
}

// toolCatalog is the set of tools the local model can invoke. Mirrors the
// fleet's own HTTP surface so a model competent at OpenAI-style function
// calling (Qwen 2.5+, Llama 3.1+, Mistral Nemo+) can drive the fleet end-to-end.
func (b *Server) toolCatalog() []any {
	def := func(name, desc string, params map[string]any) any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		}
	}
	objWith := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	intProp := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

	return []any{
		def("list_captains", "List all shipmates captains (online and recently-seen). Returns an array of {client_key, repo, persona, connected}.", objWith(nil)),
		def("tell_captain", "Send a message to a persona on a specific captain. Use this to dispatch work or kick off a slash command like /standup.",
			objWith(map[string]any{
				"captain_key": strProp("the captain's client_key, e.g. 'project-one:captain'"),
				"persona":     strProp("which crew persona to address, e.g. 'captain', 'picard', 'data'"),
				"message":     strProp("the message text — may be a slash command like '/standup'"),
			}, "captain_key", "persona", "message")),
		def("recent_events", "Get the most recent activity-feed events for a captain. Use this to read back results after a tell.",
			objWith(map[string]any{
				"captain_key": strProp("the captain's client_key"),
				"limit":       intProp("max events to return (default 20)"),
			}, "captain_key")),
		def("wait_for_result", "Block until the named captain emits its next 'result' event (a turn-complete marker) or the timeout fires. Returns the final assistant text.",
			objWith(map[string]any{
				"captain_key": strProp("the captain's client_key"),
				"timeout_sec": intProp("max seconds to wait (default 90)"),
			}, "captain_key")),
		def("pending_approvals", "List permission requests awaiting a decision across all captains.", objWith(nil)),
		def("resolve", "Allow or deny a pending permission request by its id.",
			objWith(map[string]any{
				"captain_key": strProp("the captain's client_key"),
				"id":          strProp("the pending request id"),
				"behavior":    strProp("'allow' or 'deny'"),
			}, "captain_key", "id", "behavior")),
		def("tell_all_captains", "Send one message to the CAPTAIN persona of every connected ship at once — the easy way to wake the whole fleet or ask everyone something (e.g. '/standup' or 'how is the project going?').",
			objWith(map[string]any{
				"message": strProp("the message text — may be a slash command like '/standup'"),
			}, "message")),
		def("fleet_status", "Per-mate status across every connected ship: blocked (waiting on approval), working, idle, done, or off. Use to find who is free before dispatching work.", objWith(nil)),
		def("list_beads", "List open beads (the fleet's shared work graph): id, title, status, priority, assignee, and which ships carry each.", objWith(nil)),
		def("dispatch_bead", "Assign a bead to persona@ship and wake that mate to work it: syncs the graph to the target ship, sets the assignee, and tells the mate to claim it.",
			objWith(map[string]any{
				"bead_id":     strProp("the exact bead id as returned by list_beads — do not guess"),
				"captain_key": strProp("target ship's client_key, e.g. 'homelab:captain'"),
				"persona":     strProp("target crew persona, e.g. 'backend'"),
			}, "bead_id", "captain_key", "persona")),
	}
}

// dispatchTool executes a single tool call by name and returns its result as a
// JSON string (which the model receives back as the next "tool" message).
// Errors are returned as JSON {"error": "..."} so the model can recover.
func (b *Server) dispatchTool(ctx context.Context, tc toolCall) string {
	args := tc.Args()
	switch tc.Function.Name {
	case "list_captains":
		return b.toolListCaptains()
	case "tell_captain":
		return b.toolTellCaptain(ctx, args)
	case "recent_events":
		return b.toolRecentEvents(ctx, args)
	case "wait_for_result":
		return b.toolWaitForResult(ctx, args)
	case "pending_approvals":
		return b.toolPendingApprovals(ctx)
	case "resolve":
		return b.toolResolve(ctx, args)
	case "tell_all_captains":
		return b.toolTellAllCaptains(ctx, args)
	case "fleet_status":
		return b.toolFleetStatus(ctx)
	case "list_beads":
		return b.toolListBeads(ctx)
	case "dispatch_bead":
		return b.toolDispatchBead(ctx, args)
	default:
		return toolError("unknown tool: " + tc.Function.Name)
	}
}

func toolError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func (b *Server) toolListCaptains() string {
	connected := map[string]bool{}
	for _, k := range b.dialer.ListClients() {
		connected[k] = true
	}
	type wire struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		Persona   string `json:"persona"`
		Connected bool   `json:"connected"`
	}
	b.mu.Lock()
	out := make([]wire, 0, len(b.captains))
	for k, l := range b.captains {
		out = append(out, wire{ClientKey: k, Repo: l.Repo, Persona: l.Persona, Connected: connected[k]})
	}
	b.mu.Unlock()
	raw, _ := json.Marshal(out)
	return string(raw)
}

func (b *Server) toolTellCaptain(ctx context.Context, args map[string]any) string {
	key, _ := args["captain_key"].(string)
	persona, _ := args["persona"].(string)
	msg, _ := args["message"].(string)
	if key == "" || persona == "" || msg == "" {
		return toolError("tell_captain requires captain_key, persona, message")
	}
	// The persona here is whatever the local model emitted in its tool-call
	// JSON, and the model's context is fed by ship feeds and GitHub-derived
	// text — i.e. hostile input. Gate it before it becomes a path segment.
	if err := personaname.Validate(persona); err != nil {
		return toolError(err.Error())
	}
	payload, _ := json.Marshal(map[string]string{"message": msg})
	_, status, err := b.proxy(ctx, key, "POST", "/tell/"+url.PathEscape(persona), payload)
	if err != nil {
		return toolError("dispatch failed: " + err.Error())
	}
	if status >= 300 {
		return toolError(fmt.Sprintf("captain returned status %d", status))
	}
	return `{"ok": true}`
}

func (b *Server) toolRecentEvents(ctx context.Context, args map[string]any) string {
	key, _ := args["captain_key"].(string)
	if key == "" {
		return toolError("recent_events requires captain_key")
	}
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	body, status, err := b.proxy(ctx, key, "GET", "/events", nil)
	if err != nil || status >= 300 {
		return toolError("fetch events failed")
	}
	var events []map[string]any
	if err := json.Unmarshal(body, &events); err != nil {
		return toolError("decode events failed")
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	raw, _ := json.Marshal(events)
	return string(raw)
}

func (b *Server) toolWaitForResult(ctx context.Context, args map[string]any) string {
	key, _ := args["captain_key"].(string)
	if key == "" {
		return toolError("wait_for_result requires captain_key")
	}
	timeout := 90 * time.Second
	if v, ok := args["timeout_sec"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	// Snapshot the highest event time we've already seen so we only count NEW
	// result events (the captain may have older results from prior turns).
	high := snapshotMaxEventTime(ctx, b, key)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return toolError("cancelled")
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return toolError("timeout waiting for result")
		}
		body, status, err := b.proxy(ctx, key, "GET", "/events", nil)
		if err != nil || status >= 300 {
			continue
		}
		var events []struct {
			Time    string `json:"time"`
			Persona string `json:"persona"`
			Type    string `json:"type"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal(body, &events); err != nil {
			continue
		}
		var lastAssistant string
		for _, e := range events {
			if e.Time <= high {
				continue
			}
			if e.Type == "assistant" && e.Text != "" {
				lastAssistant = e.Text
			}
			if e.Type == "result" {
				raw, _ := json.Marshal(map[string]string{"final_text": lastAssistant})
				return string(raw)
			}
		}
	}
}

func snapshotMaxEventTime(ctx context.Context, b *Server, key string) string {
	body, status, err := b.proxy(ctx, key, "GET", "/events", nil)
	if err != nil || status >= 300 {
		return ""
	}
	var events []struct {
		Time string `json:"time"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return ""
	}
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Time
}

func (b *Server) toolPendingApprovals(ctx context.Context) string {
	type entry struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		ID        string `json:"id"`
		Persona   string `json:"persona"`
		Tool      string `json:"tool"`
		Input     string `json:"input,omitempty"`
	}
	all := make([]entry, 0)
	for _, key := range b.dialer.ListClients() {
		body, status, err := b.proxy(ctx, key, "GET", "/pending.json", nil)
		if err != nil || status >= 300 {
			continue
		}
		var raw []struct {
			ID      string `json:"id"`
			Persona string `json:"persona"`
			Tool    string `json:"tool"`
			Input   string `json:"input"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			continue
		}
		b.mu.Lock()
		captain := b.captains[key]
		b.mu.Unlock()
		repo := ""
		if captain != nil {
			repo = captain.Repo
		}
		for _, p := range raw {
			all = append(all, entry{ClientKey: key, Repo: repo, ID: p.ID, Persona: p.Persona, Tool: p.Tool, Input: p.Input})
		}
	}
	out, _ := json.Marshal(all)
	return string(out)
}

func (b *Server) toolResolve(ctx context.Context, args map[string]any) string {
	key, _ := args["captain_key"].(string)
	id, _ := args["id"].(string)
	behavior, _ := args["behavior"].(string)
	if key == "" || id == "" || (behavior != "allow" && behavior != "deny") {
		return toolError("resolve requires captain_key, id, behavior (allow|deny)")
	}
	// Model-supplied id — same hostile-input argument as tell_captain.
	if !beadIDOK(id) {
		return toolError("bad pending request id")
	}
	payload, _ := json.Marshal(map[string]string{"behavior": behavior})
	_, status, err := b.proxy(ctx, key, "POST", "/resolve/"+url.PathEscape(id), payload)
	if err != nil || status >= 300 {
		return toolError("resolve failed")
	}
	slog.Info("conversation resolved pending", "captain", key, "id", id, "behavior", behavior)
	return `{"ok": true}`
}

// toolTellAllCaptains broadcasts one message to every connected ship's captain
// — the single-call fan-out small models reach for reliably, where "call
// tell_captain N times" degrades into prose.
func (b *Server) toolTellAllCaptains(ctx context.Context, args map[string]any) string {
	msg, _ := args["message"].(string)
	if strings.TrimSpace(msg) == "" {
		return toolError("tell_all_captains requires message")
	}
	payload, _ := json.Marshal(map[string]string{"message": msg})
	results := map[string]string{}
	for _, key := range b.dialer.ListClients() {
		b.mu.Lock()
		captain := b.captains[key]
		b.mu.Unlock()
		persona := "captain"
		if captain != nil && captain.Persona != "" {
			persona = captain.Persona
		}
		// The persona came off the ship's own X-Shipmates-Persona header at
		// connect time; a ship that registers a junk one gets skipped rather
		// than proxied.
		if !personaname.Valid(persona) {
			results[key] = "failed: bad persona"
			continue
		}
		if _, status, err := b.proxy(ctx, key, "POST", "/tell/"+url.PathEscape(persona), payload); err != nil || status >= 300 {
			results[key] = "failed"
			continue
		}
		results[key] = "sent"
	}
	if len(results) == 0 {
		return toolError("no ships connected")
	}
	out, _ := json.Marshal(results)
	return string(out)
}

// toolFleetStatus fans out to every connected captain's /status.json and returns
// a flat [{ship, persona, status}] — the same data behind the UI's dots.
func (b *Server) toolFleetStatus(ctx context.Context) string {
	type mate struct {
		Ship    string `json:"ship"`
		Persona string `json:"persona"`
		Status  string `json:"status"`
	}
	var all []mate
	for _, key := range b.dialer.ListClients() {
		body, status, err := b.proxy(ctx, key, "GET", "/status.json", nil)
		if err != nil || status >= 300 {
			continue
		}
		var raw []struct {
			Persona string `json:"persona"`
			Status  string `json:"status"`
		}
		if json.Unmarshal(body, &raw) != nil {
			continue
		}
		for _, m := range raw {
			all = append(all, mate{Ship: key, Persona: m.Persona, Status: m.Status})
		}
	}
	out, _ := json.Marshal(all)
	return string(out)
}

// toolListBeads is the fleet bead union, deduped by id (synced graphs would
// otherwise repeat every bead once per ship).
func (b *Server) toolListBeads(ctx context.Context) string {
	type bead struct {
		ID       string   `json:"id"`
		Title    string   `json:"title"`
		Status   string   `json:"status"`
		Priority *int     `json:"priority,omitempty"`
		Assignee string   `json:"assignee,omitempty"`
		Ships    []string `json:"ships"`
	}
	merged := map[string]*bead{}
	for _, key := range b.dialer.ListClients() {
		body, status, err := b.proxy(ctx, key, "GET", "/beads.json", nil)
		if err != nil || status >= 300 {
			continue
		}
		var raw []bead
		if json.Unmarshal(body, &raw) != nil {
			continue
		}
		for i := range raw {
			if existing, ok := merged[raw[i].ID]; ok {
				existing.Ships = append(existing.Ships, key)
				continue
			}
			bd := raw[i]
			bd.Ships = []string{key}
			merged[bd.ID] = &bd
		}
	}
	out := make([]bead, 0, len(merged))
	for _, v := range merged {
		sort.Strings(v.Ships)
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	raw, _ := json.Marshal(out)
	return string(raw)
}

// toolDispatchBead is voice-driven cross-ship dispatch: sync the graph to the
// target ship, set the assignee there, then wake the mate — the same sequence
// the UI's dispatch button runs.
func (b *Server) toolDispatchBead(ctx context.Context, args map[string]any) string {
	id, _ := args["bead_id"].(string)
	key, _ := args["captain_key"].(string)
	persona, _ := args["persona"].(string)
	if id == "" || key == "" || persona == "" {
		return toolError("dispatch_bead requires bead_id, captain_key, persona")
	}
	if !beadIDOK(id) {
		return toolError("bad bead id")
	}
	// persona rides into the dispatch tell's path segment (deliverDispatch)
	// and into the assignee string written onto the graph.
	if err := personaname.Validate(persona); err != nil {
		return toolError(err.Error())
	}
	if !b.dialer.HasSession(key) {
		return toolError("ship " + key + " is not connected")
	}
	// pull first so the bead exists locally before the update references it
	if out, status, err := b.proxy(ctx, key, "POST", "/beads/pull?wait=1", nil); err != nil || status >= 300 {
		return toolError("target could not sync the graph: " + string(out))
	}
	// Verify the id is real BEFORE mutating. Small models guess ids instead
	// of looking them up; feeding the actual graph back in the error lets the
	// model self-correct on its next loop iteration instead of hallucinating
	// an excuse to the operator.
	if _, status, err := b.proxy(ctx, key, "GET", "/bead/"+url.PathEscape(id), nil); err != nil || status >= 300 {
		return `{"error":"no bead with id '` + id + `' — pick the EXACT id from open_beads whose title matches","open_beads":` + b.toolListBeads(ctx) + `}`
	}
	shipName, _, _ := strings.Cut(key, ":")
	assignee := persona + "@" + shipName
	upd, _ := json.Marshal(map[string]string{"assignee": assignee})
	if out, status, err := b.proxy(ctx, key, "POST", "/bead/"+url.PathEscape(id)+"/update", upd); err != nil || status >= 300 {
		return toolError("assignee update failed: " + string(out))
	}
	if err := b.deliverDispatch(ctx, queuedDispatch{Ship: key, Persona: persona, Bead: id}, false); err != nil {
		return toolError("wake-up tell failed: " + err.Error())
	}
	slog.Info("conversation dispatched bead", "bead", id, "assignee", assignee)
	raw, _ := json.Marshal(map[string]string{"ok": "true", "assignee": assignee})
	return string(raw)
}
