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
// Frames observed against claude 2.1.153 (`-p --input-format stream-json
// --output-format stream-json --verbose`):
//
//	{"type":"system","subtype":"init",…}                            per-turn preamble
//	{"type":"assistant","message":{"content":[{"type":"text","text":"…"}]}}
//	{"type":"assistant","message":{"content":[{"type":"thinking",…}]}}
//	{"type":"assistant","message":{"content":[{"type":"tool_use","id":"…","name":"Bash","input":{…}}]}}
//	{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"…","content":"…"}]}}
//	{"type":"result","subtype":"success","is_error":false,…}        turn boundary
//	{"type":"result","subtype":"error_during_execution","is_error":true,…}
//	{"type":"control_response","response":{"subtype":"success","request_id":"…"}}
//	{"type":"rate_limit_event",…}
//	{"type":"control_request","request_id":"…","request":{"subtype":"can_use_tool",…}}
//
// Tool use / tool results are nested inside assistant/user message content
// blocks; we also keep the legacy flat {"type":"tool_use"} handling for
// robustness.

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
	// result-shaped
	IsError bool `json:"is_error,omitempty"`
	// control_request-shaped. request_id sits at the TOP level of the frame,
	// not inside "request" — verified against claude 2.1.153.
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

// controlRequest is the body of a {"type":"control_request"} frame.
type controlRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	ToolUseID   string          `json:"tool_use_id"`
	Input       json.RawMessage `json:"input"`
	Suggestions json.RawMessage `json:"permission_suggestions"`
}

// contentBlock is one element of message.content in assistant/user frames.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use-shaped
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result-shaped
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// frameType cheaply extracts the "type" of one JSONL line so the read loop
// can spot turn-boundary result frames without a full decode. Returns ""
// on malformed JSON.
func frameType(raw []byte) string {
	var t struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return ""
	}
	return t.Type
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
	case "assistant":
		return decodeMessageFrame(ev, f, copyOfRaw)
	case "text":
		ev.Kind = runtime.KindText
		ev.Payload = extractText(f, copyOfRaw)
	case "user":
		// Tool results echo back as user-role frames with tool_result
		// content blocks; anything else user-shaped is backend noise.
		if tr, ok := extractToolResult(f); ok {
			ev.Kind = runtime.KindToolResult
			ev.Payload = tr
		} else {
			ev.Kind = runtime.KindBackend
			ev.Payload = copyOfRaw
		}
	case "tool_use":
		ev.Kind = runtime.KindToolCall
		ev.Payload = ToolCall{ID: f.ID, Name: f.Name, InputJSON: f.Input}
	case "tool_result":
		ev.Kind = runtime.KindToolResult
		ev.Payload = ToolResult{ToolUseID: f.ToolUseID, ContentJSON: f.Content}
	case "result":
		// The per-turn boundary frame. is_error covers API failures and
		// interrupted turns (subtype "error_during_execution").
		if f.IsError {
			ev.Kind = runtime.KindError
		} else {
			ev.Kind = runtime.KindTurnDone
		}
		ev.Payload = copyOfRaw
	case "control_request":
		// The only control_request Claude Code issues toward us is the
		// permission prompt. Anything else stays backend noise.
		if req, ok := extractApproval(f); ok {
			ev.Kind = runtime.KindApprovalNeeded
			ev.Payload = req
		} else {
			ev.Kind = runtime.KindBackend
			ev.Payload = copyOfRaw
		}
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

// decodeMessageFrame maps an assistant frame by its message content: text
// blocks become KindText, tool_use blocks become KindToolCall, anything
// else (thinking blocks, signatures) stays KindBackend with the raw JSON
// preserved.
func decodeMessageFrame(ev runtime.Event, f frame, raw []byte) runtime.Event {
	for _, c := range messageContent(f) {
		switch c.Type {
		case "text":
			if c.Text != "" {
				ev.Kind = runtime.KindText
				ev.Payload = c.Text
				return ev
			}
		case "tool_use":
			ev.Kind = runtime.KindToolCall
			ev.Payload = ToolCall{ID: c.ID, Name: c.Name, InputJSON: c.Input}
			return ev
		}
	}
	// Flat {"type":"assistant","text":"…"} — not observed on current
	// claude, kept for robustness.
	if f.Text != "" {
		ev.Kind = runtime.KindText
		ev.Payload = f.Text
		return ev
	}
	ev.Kind = runtime.KindBackend
	ev.Payload = raw
	return ev
}

// messageContent unmarshals message.content blocks, tolerating absence.
func messageContent(f frame) []contentBlock {
	if len(f.Message) == 0 {
		return nil
	}
	var inner struct {
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(f.Message, &inner); err != nil {
		return nil
	}
	return inner.Content
}

// extractToolResult pulls a tool_result content block out of a user frame.
func extractToolResult(f frame) (ToolResult, bool) {
	for _, c := range messageContent(f) {
		if c.Type == "tool_result" {
			return ToolResult{ToolUseID: c.ToolUseID, ContentJSON: c.Content}, true
		}
	}
	return ToolResult{}, false
}

// extractText pulls a plain-text payload out of a text-shaped frame.
// Handles both flat {"text":"…"} and the nested message.content shape.
func extractText(f frame, fallback []byte) string {
	if f.Text != "" {
		return f.Text
	}
	for _, c := range messageContent(f) {
		if c.Type == "text" && c.Text != "" {
			return c.Text
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

// ApprovalRequest is the KindApprovalNeeded payload — one Claude Code
// `can_use_tool` permission prompt, verbatim.
//
// Verified frame (claude 2.1.153, `--permission-prompt-tool stdio`):
//
//	{"type":"control_request","request_id":"ea418bbe-…","request":{
//	  "subtype":"can_use_tool","tool_name":"Write","display_name":"Write",
//	  "input":{"file_path":"…","content":"…"},"description":"probe-test.txt",
//	  "permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}],
//	  "tool_use_id":"toolu_016nb…"}}
//
// InputJSON is retained verbatim because an "allow" answer must echo the
// tool input back as `updatedInput` — claude 2.1.153 rejects an allow that
// omits it.
type ApprovalRequest struct {
	RequestID   string
	ToolName    string
	DisplayName string
	Description string
	ToolUseID   string
	InputJSON   json.RawMessage
	// SuggestionsJSON holds `permission_suggestions` untouched. Shipmates
	// never echoes them back (it grants "once" scope only), but consumers
	// can display what Claude Code proposed.
	SuggestionsJSON json.RawMessage
}

// Command extracts the shell command an approval is asking about, for the
// tools that run one. The second result reports whether this request is a
// shell execution at all — file writes, web fetches and the rest are not,
// and callers must not present their arguments as a command line.
func (a ApprovalRequest) Command() (string, bool) {
	switch a.ToolName {
	case "Bash", "PowerShell":
	default:
		return "", false
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(a.InputJSON, &in) != nil || in.Command == "" {
		return "", false
	}
	return in.Command, true
}

// extractApproval pulls a can_use_tool permission prompt out of a
// control_request frame. A request without an id cannot be answered, so it
// is rejected rather than surfaced as an unanswerable approval.
func extractApproval(f frame) (ApprovalRequest, bool) {
	if f.RequestID == "" || len(f.Request) == 0 {
		return ApprovalRequest{}, false
	}
	var req controlRequest
	if err := json.Unmarshal(f.Request, &req); err != nil || req.Subtype != "can_use_tool" {
		return ApprovalRequest{}, false
	}
	return ApprovalRequest{
		RequestID:       f.RequestID,
		ToolName:        req.ToolName,
		DisplayName:     req.DisplayName,
		Description:     req.Description,
		ToolUseID:       req.ToolUseID,
		InputJSON:       req.Input,
		SuggestionsJSON: req.Suggestions,
	}, true
}
