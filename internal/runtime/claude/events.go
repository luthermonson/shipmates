package claude

import (
	"encoding/json"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// Claude Code's stream-json output emits one JSON object per line. Its
// shape isn't strictly documented as a public API, so we parse
// defensively: recognized types map to runtime.Kind; everything else
// falls through as KindBackend with the raw JSON preserved as the
// payload. That guarantees consumers never lose information even if
// Anthropic adds fields.
//
// The most common frames we care about:
//
//   {"type":"assistant","message":{"content":[{"type":"text","text":"…"}]}}
//   {"type":"tool_use","id":"…","name":"Bash","input":{…}}
//   {"type":"tool_result","tool_use_id":"…","content":"…"}
//   {"type":"result","subtype":"…"}
//
// Plus wrapping/system messages we treat as backend events.

type frame struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Text    string          `json:"text,omitempty"`
	// tool_result-shaped
	ToolUseID string `json:"tool_use_id,omitempty"`
}

// decodeFrame parses one JSONL line and produces a normalized event.
// Malformed JSON becomes a KindBackend event carrying the raw bytes;
// callers can still see the line for debugging.
func decodeFrame(raw []byte, sessionID, turnID string) runtime.Event {
	ev := runtime.Event{
		Timestamp: time.Now(),
		SessionID: sessionID,
		TurnID:    turnID,
	}
	// Copy raw bytes — scanner.Bytes() is only valid until the next Scan().
	copyOfRaw := make([]byte, len(raw))
	copy(copyOfRaw, raw)

	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		ev.Kind = runtime.KindBackend
		ev.Payload = copyOfRaw
		return ev
	}

	switch f.Type {
	case "assistant", "text":
		ev.Kind = runtime.KindText
		ev.Payload = extractText(f, copyOfRaw)
	case "tool_use":
		ev.Kind = runtime.KindToolCall
		ev.Payload = ToolCall{ID: f.ID, Name: f.Name, InputJSON: f.Input}
	case "tool_result":
		ev.Kind = runtime.KindToolResult
		ev.Payload = ToolResult{ToolUseID: f.ToolUseID, ContentJSON: f.Content}
	case "result":
		ev.Kind = runtime.KindTurnDone
		ev.Payload = copyOfRaw
	case "system":
		ev.Kind = runtime.KindBackend
		ev.Payload = copyOfRaw
	case "error":
		ev.Kind = runtime.KindError
		ev.Payload = copyOfRaw
	default:
		ev.Kind = runtime.KindBackend
		ev.Payload = copyOfRaw
	}
	return ev
}

// extractText pulls a plain-text payload out of an assistant/text frame.
// Claude Code sometimes nests text in message.content[].text; we handle
// both flat {"text":"…"} and the nested shape.
func extractText(f frame, fallback []byte) string {
	if f.Text != "" {
		return f.Text
	}
	if len(f.Message) > 0 {
		var inner struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(f.Message, &inner); err == nil {
			for _, c := range inner.Content {
				if c.Type == "text" && c.Text != "" {
					return c.Text
				}
			}
		}
	}
	return string(fallback)
}

// ToolCall is the KindToolCall payload — the requested tool + its raw
// input args as JSON so consumers don't have to guess at the schema.
type ToolCall struct {
	ID        string
	Name      string
	InputJSON json.RawMessage
}

// ToolResult is the KindToolResult payload — the tool call's opaque
// result content, still as JSON so consumers can format it as they see
// fit.
type ToolResult struct {
	ToolUseID   string
	ContentJSON json.RawMessage
}
