package claude

import (
	"encoding/json"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

func TestDecodeFrame_AssistantMessage(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]}}`)
	ev := decodeFrame(line, "sess1", "turn1")
	if ev.Kind != runtime.KindText {
		t.Errorf("Kind=%q want text", ev.Kind)
	}
	if text, _ := ev.Payload.(string); text != "hello world" {
		t.Errorf("payload=%q want hello world", text)
	}
	if ev.SessionID != "sess1" || ev.TurnID != "turn1" {
		t.Errorf("ids not propagated: %q %q", ev.SessionID, ev.TurnID)
	}
}

func TestDecodeFrame_FlatText(t *testing.T) {
	line := []byte(`{"type":"text","text":"flat body"}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindText {
		t.Errorf("Kind=%q", ev.Kind)
	}
	if text, _ := ev.Payload.(string); text != "flat body" {
		t.Errorf("payload=%q", text)
	}
}

func TestDecodeFrame_ToolUse(t *testing.T) {
	line := []byte(`{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindToolCall {
		t.Errorf("Kind=%q want tool_call", ev.Kind)
	}
	tc, ok := ev.Payload.(ToolCall)
	if !ok {
		t.Fatalf("payload not ToolCall: %T", ev.Payload)
	}
	if tc.ID != "tu_1" || tc.Name != "Bash" {
		t.Errorf("id=%q name=%q", tc.ID, tc.Name)
	}
	if string(tc.InputJSON) != `{"command":"ls"}` {
		t.Errorf("input=%q", string(tc.InputJSON))
	}
}

func TestDecodeFrame_ToolResult(t *testing.T) {
	line := []byte(`{"type":"tool_result","tool_use_id":"tu_1","content":"stdout output"}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindToolResult {
		t.Errorf("Kind=%q want tool_result", ev.Kind)
	}
	tr, ok := ev.Payload.(ToolResult)
	if !ok {
		t.Fatalf("payload not ToolResult: %T", ev.Payload)
	}
	if tr.ToolUseID != "tu_1" {
		t.Errorf("tool_use_id=%q", tr.ToolUseID)
	}
}

func TestDecodeFrame_Result(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success"}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindTurnDone {
		t.Errorf("Kind=%q want turn_done", ev.Kind)
	}
}

func TestDecodeFrame_UnknownType(t *testing.T) {
	line := []byte(`{"type":"future_thing","data":42}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindBackend {
		t.Errorf("Kind=%q want backend", ev.Kind)
	}
	// Raw payload preserved so consumers can still debug.
	raw, _ := ev.Payload.([]byte)
	if !json.Valid(raw) {
		t.Errorf("payload not preserved as valid JSON: %s", raw)
	}
}

func TestDecodeFrame_MalformedJSON(t *testing.T) {
	line := []byte(`{not-json`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindBackend {
		t.Errorf("Kind=%q want backend on malformed", ev.Kind)
	}
	raw, _ := ev.Payload.([]byte)
	if string(raw) != string(line) {
		t.Errorf("raw bytes not preserved: %q vs %q", raw, line)
	}
}

func TestDecodeFrame_Error(t *testing.T) {
	line := []byte(`{"type":"error","message":"rate limit"}`)
	ev := decodeFrame(line, "s", "t")
	if ev.Kind != runtime.KindError {
		t.Errorf("Kind=%q want error", ev.Kind)
	}
}
