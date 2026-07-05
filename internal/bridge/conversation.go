package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// The /api/conversation endpoint is the bridge's "captain's mate" — a local-LLM
// proxy that wraps Ollama with a tool catalog mapped 1:1 to the bridge's own
// HTTP surface. The operator's voice (or text) turn comes in here; the model
// can call fleet operations (list_leads, tell_lead, wait_for_result, etc.) to
// orchestrate the crew, and the final natural-language reply is what gets
// spoken back to the operator.
//
// Why this lives on the bridge, not on the lead: the bridge already has the
// authenticated edge surface and the connection registry; the lead is project-
// scoped. Cross-fleet questions ("which crew is idle?", "tell the busiest one
// to stop") need bridge-wide context, which only the bridge has.

const (
	// conversationMaxIterations caps the chat→tool→chat loop so a misbehaving
	// model can't pin the bridge. Five iterations covers "list leads, find one,
	// tell it, wait for result, summarize" — anything richer should be split
	// into multiple operator turns.
	conversationMaxIterations = 5
	// conversationKeepAlive instructs Ollama to keep the model resident in RAM
	// rather than its 5min default. Cold loads on a 7B model take 30s on this
	// hardware — pinning warm keeps every turn at ~3s.
	conversationKeepAlive = "24h"
)

// chatMessage is one entry in the conversation history we send to Ollama.
// Role is "system" | "user" | "assistant" | "tool". Tool messages carry the
// result of a tool the assistant called on the previous turn.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"` // tool name when role=="tool"
}

type toolCall struct {
	ID       string       `json:"id,omitempty"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// conversationRequest is the public shape the UI POSTs.
type conversationRequest struct {
	Messages []chatMessage `json:"messages"`
}

// conversationResponse is what the UI gets back. ToolsCalled is exposed so the
// UI can show a quick "I checked the leads, picked picard, dispatched standup"
// timeline instead of an opaque reply.
type conversationResponse struct {
	Reply       string     `json:"reply"`
	ToolsCalled []toolCall `json:"tools_called,omitempty"`
}

// handleConversation runs the operator's turn through Ollama, executing any
// tools the model calls against the bridge's own endpoints, until the model
// produces a final natural-language reply (or the iteration cap trips).
func (b *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	if b.conv == nil {
		http.Error(w, "conversation disabled — start bridge with --ollama-url", http.StatusServiceUnavailable)
		return
	}
	var req conversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	messages := prependSystemPrompt(req.Messages)
	tools := b.toolCatalog()
	var called []toolCall

	for iter := 0; iter < conversationMaxIterations; iter++ {
		resp, err := b.ollamaChat(r.Context(), messages, tools)
		if err != nil {
			http.Error(w, "ollama: "+err.Error(), http.StatusBadGateway)
			return
		}
		// Always append the assistant turn — including tool_calls — so the
		// next iteration sees its own request.
		messages = append(messages, chatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		if len(resp.ToolCalls) == 0 {
			_ = json.NewEncoder(w).Encode(conversationResponse{
				Reply:       resp.Content,
				ToolsCalled: called,
			})
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
	http.Error(w, "tool-call loop exceeded "+fmt.Sprint(conversationMaxIterations)+" iterations", http.StatusBadGateway)
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
You are the operator's voice assistant for a fleet of AI coding agents (shipmates).
Reply tersely — your output will be spoken aloud. One or two sentences max.
Use the provided tools when the operator asks about fleet status, wants to
dispatch work, or needs to approve/deny pending permission requests. If a tool
returns no matches, say so plainly rather than guessing.
Lead identifiers look like "<repo>:<persona>" (e.g. "card-cannon:lead").
If the operator names a lead by repo only, list leads to find the exact key.
`),
	}
	return append([]chatMessage{system}, in...)
}

// ollamaChat does one round-trip to Ollama's /api/chat and returns the
// assistant message (content + any tool_calls).
type ollamaChatResp struct {
	Message struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	} `json:"message"`
}

func (b *Server) ollamaChat(ctx context.Context, messages []chatMessage, tools []any) (*struct {
	Content   string
	ToolCalls []toolCall
}, error) {
	payload := map[string]any{
		"model":      b.conv.model,
		"messages":   messages,
		"tools":      tools,
		"stream":     false,
		"keep_alive": conversationKeepAlive,
	}
	if b.conv.cpuOnly {
		// num_gpu 0 forces CPU inference — for hosts whose GPU ollama probes
		// but can't actually run (old cards, driver/toolchain mismatches).
		payload["options"] = map[string]any{"num_gpu": 0}
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", b.conv.url+"/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.conv.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out ollamaChatResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	return &struct {
		Content   string
		ToolCalls []toolCall
	}{Content: out.Message.Content, ToolCalls: out.Message.ToolCalls}, nil
}

// toolCatalog is the set of tools the local model can invoke. Mirrors the
// bridge's own HTTP surface so a model competent at OpenAI-style function
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
		def("list_leads", "List all shipmates leads (online and recently-seen). Returns an array of {client_key, repo, persona, connected}.", objWith(nil)),
		def("tell_lead", "Send a message to a persona on a specific lead. Use this to dispatch work or kick off a slash command like /standup.",
			objWith(map[string]any{
				"lead_key": strProp("the lead's client_key, e.g. 'card-cannon:lead'"),
				"persona":  strProp("which crew persona to address, e.g. 'lead', 'picard', 'data'"),
				"message":  strProp("the message text — may be a slash command like '/standup'"),
			}, "lead_key", "persona", "message")),
		def("recent_events", "Get the most recent activity-feed events for a lead. Use this to read back results after a tell.",
			objWith(map[string]any{
				"lead_key": strProp("the lead's client_key"),
				"limit":    intProp("max events to return (default 20)"),
			}, "lead_key")),
		def("wait_for_result", "Block until the named lead emits its next 'result' event (a turn-complete marker) or the timeout fires. Returns the final assistant text.",
			objWith(map[string]any{
				"lead_key":    strProp("the lead's client_key"),
				"timeout_sec": intProp("max seconds to wait (default 90)"),
			}, "lead_key")),
		def("pending_approvals", "List permission requests awaiting a decision across all leads.", objWith(nil)),
		def("resolve", "Allow or deny a pending permission request by its id.",
			objWith(map[string]any{
				"lead_key": strProp("the lead's client_key"),
				"id":       strProp("the pending request id"),
				"behavior": strProp("'allow' or 'deny'"),
			}, "lead_key", "id", "behavior")),
	}
}

// dispatchTool executes a single tool call by name and returns its result as a
// JSON string (which the model receives back as the next "tool" message).
// Errors are returned as JSON {"error": "..."} so the model can recover.
func (b *Server) dispatchTool(ctx context.Context, tc toolCall) string {
	args := tc.Function.Arguments
	switch tc.Function.Name {
	case "list_leads":
		return b.toolListLeads()
	case "tell_lead":
		return b.toolTellLead(ctx, args)
	case "recent_events":
		return b.toolRecentEvents(ctx, args)
	case "wait_for_result":
		return b.toolWaitForResult(ctx, args)
	case "pending_approvals":
		return b.toolPendingApprovals(ctx)
	case "resolve":
		return b.toolResolve(ctx, args)
	default:
		return toolError("unknown tool: " + tc.Function.Name)
	}
}

func toolError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func (b *Server) toolListLeads() string {
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
	out := make([]wire, 0, len(b.leads))
	for k, l := range b.leads {
		out = append(out, wire{ClientKey: k, Repo: l.Repo, Persona: l.Persona, Connected: connected[k]})
	}
	b.mu.Unlock()
	raw, _ := json.Marshal(out)
	return string(raw)
}

func (b *Server) toolTellLead(ctx context.Context, args map[string]any) string {
	key, _ := args["lead_key"].(string)
	persona, _ := args["persona"].(string)
	msg, _ := args["message"].(string)
	if key == "" || persona == "" || msg == "" {
		return toolError("tell_lead requires lead_key, persona, message")
	}
	payload, _ := json.Marshal(map[string]string{"message": msg})
	_, status, err := b.proxy(ctx, key, "POST", "/tell/"+persona, payload)
	if err != nil {
		return toolError("dispatch failed: " + err.Error())
	}
	if status >= 300 {
		return toolError(fmt.Sprintf("lead returned status %d", status))
	}
	return `{"ok": true}`
}

func (b *Server) toolRecentEvents(ctx context.Context, args map[string]any) string {
	key, _ := args["lead_key"].(string)
	if key == "" {
		return toolError("recent_events requires lead_key")
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
	key, _ := args["lead_key"].(string)
	if key == "" {
		return toolError("wait_for_result requires lead_key")
	}
	timeout := 90 * time.Second
	if v, ok := args["timeout_sec"].(float64); ok && v > 0 {
		timeout = time.Duration(v) * time.Second
	}
	// Snapshot the highest event time we've already seen so we only count NEW
	// result events (the lead may have older results from prior turns).
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
		lead := b.leads[key]
		b.mu.Unlock()
		repo := ""
		if lead != nil {
			repo = lead.Repo
		}
		for _, p := range raw {
			all = append(all, entry{ClientKey: key, Repo: repo, ID: p.ID, Persona: p.Persona, Tool: p.Tool, Input: p.Input})
		}
	}
	out, _ := json.Marshal(all)
	return string(out)
}

func (b *Server) toolResolve(ctx context.Context, args map[string]any) string {
	key, _ := args["lead_key"].(string)
	id, _ := args["id"].(string)
	behavior, _ := args["behavior"].(string)
	if key == "" || id == "" || (behavior != "allow" && behavior != "deny") {
		return toolError("resolve requires lead_key, id, behavior (allow|deny)")
	}
	payload, _ := json.Marshal(map[string]string{"behavior": behavior})
	_, status, err := b.proxy(ctx, key, "POST", "/resolve/"+id, payload)
	if err != nil || status >= 300 {
		return toolError("resolve failed")
	}
	slog.Info("conversation resolved pending", "lead", key, "id", id, "behavior", behavior)
	return `{"ok": true}`
}
