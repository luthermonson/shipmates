package streamjson

import (
	"strings"
	"testing"
)

func one(t *testing.T, line string) Item {
	t.Helper()
	items := DecodeLine([]byte(line))
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d from %s", len(items), line)
	}
	return items[0]
}

func TestDecodeAssistantText(t *testing.T) {
	it := one(t, `{"type":"assistant","message":{"model":"claude-opus-4-7","content":[{"type":"text","text":"hello"}]}}`)
	if it.Kind != "text" || it.Text != "hello" || it.Model != "claude-opus-4-7" {
		t.Fatalf("got %+v", it)
	}
}

func TestDecodeThinking(t *testing.T) {
	it := one(t, `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"pondering...","signature":"x"}]}}`)
	if it.Kind != "thinking" || it.Text != "pondering..." {
		t.Fatalf("got %+v", it)
	}
}

func TestDecodeToolUse(t *testing.T) {
	it := one(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la"}}]}}`)
	if it.Kind != "tool_use" || it.Tool != "Bash" || it.Input["command"] != "ls -la" {
		t.Fatalf("got %+v", it)
	}
}

func TestDecodeToolResultBlocks(t *testing.T) {
	it := one(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"file1\nfile2"}]}]}}`)
	if it.Kind != "tool_result" || it.Text != "file1\nfile2" {
		t.Fatalf("got %+v", it)
	}
}

func TestDecodeToolResultString(t *testing.T) {
	it := one(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"plain output"}]}}`)
	if it.Kind != "tool_result" || it.Text != "plain output" {
		t.Fatalf("got %+v", it)
	}
}

func TestDecodeResult(t *testing.T) {
	it := one(t, `{"type":"result","subtype":"success","total_cost_usd":0.0312,"duration_ms":42000,"num_turns":6,"result":"done"}`)
	if it.Kind != "result" || it.CostUSD != 0.0312 || it.DurationMS != 42000 {
		t.Fatalf("got %+v", it)
	}
	if s := it.Summary(); s != "$0.0312 · 42s" {
		t.Fatalf("Summary = %q", s)
	}
}

func TestSummaryMinutes(t *testing.T) {
	it := Item{Kind: "result", DurationMS: 95_000}
	if s := it.Summary(); s != "1m35s" {
		t.Fatalf("Summary = %q", s)
	}
}

func TestUnknownTypesIgnored(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init","tools":["Bash"]}`,
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`,
		`{"type":"mystery_future_thing","payload":1}`,
		`not json at all`,
	} {
		if items := DecodeLine([]byte(line)); len(items) != 0 {
			t.Fatalf("expected nothing from %s, got %+v", line, items)
		}
	}
}

func TestTruncation(t *testing.T) {
	big := strings.Repeat("x", 5000)
	it := one(t, `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"`+big+`"}]}}`)
	if len(it.Text) > truncateAt+20 || !strings.HasSuffix(it.Text, "…[truncated]") {
		t.Fatalf("thinking not truncated: len=%d", len(it.Text))
	}
}
