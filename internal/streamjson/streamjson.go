// Package streamjson decodes Claude Code's `--output-format stream-json`
// lines into renderable items. The format is Anthropic's contract and can
// drift between CLI versions, so ALL knowledge of its shape lives here — the
// server consumes Items, never raw maps. Decode is forward-tolerant: unknown
// message types and content blocks yield nothing rather than errors.
package streamjson

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Item is one renderable unit from a stream-json line.
type Item struct {
	Kind       string         // text | thinking | tool_use | tool_result | result
	Text       string         // text/thinking body, tool_result output, "" for result
	Tool       string         // tool_use: the tool name
	Input      map[string]any // tool_use: the raw input object
	Model      string         // assistant messages: the serving model
	CostUSD    float64        // result: total turn cost
	DurationMS int64          // result: wall-clock turn duration
}

// truncateAt bounds bulky bodies (thinking can run to many KB, tool results
// to MB) so the feed stays a feed. Full content lives in the session; the
// timeline is a summary surface.
const truncateAt = 700

// Decode converts one parsed stream-json object into zero or more Items.
func Decode(obj map[string]any) []Item {
	switch obj["type"] {
	case "assistant":
		return decodeAssistant(obj)
	case "user":
		return decodeUser(obj)
	case "result":
		return decodeResult(obj)
	}
	// system, stream_event (partial deltas — the full message follows), and
	// anything a future CLI adds: not timeline material.
	return nil
}

func decodeAssistant(obj map[string]any) []Item {
	m, _ := obj["message"].(map[string]any)
	if m == nil {
		return nil
	}
	model, _ := m["model"].(string)
	content, _ := m["content"].([]any)
	var items []Item
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if cm == nil {
			continue
		}
		switch cm["type"] {
		case "text":
			if t, _ := cm["text"].(string); strings.TrimSpace(t) != "" {
				items = append(items, Item{Kind: "text", Text: t, Model: model})
			}
		case "thinking":
			if t, _ := cm["thinking"].(string); strings.TrimSpace(t) != "" {
				items = append(items, Item{Kind: "thinking", Text: truncate(t), Model: model})
			}
		case "tool_use":
			name, _ := cm["name"].(string)
			input, _ := cm["input"].(map[string]any)
			items = append(items, Item{Kind: "tool_use", Tool: name, Input: input, Model: model})
		}
	}
	return items
}

func decodeUser(obj map[string]any) []Item {
	m, _ := obj["message"].(map[string]any)
	if m == nil {
		return nil
	}
	content, _ := m["content"].([]any)
	var items []Item
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if cm == nil || cm["type"] != "tool_result" {
			continue
		}
		if t := resultText(cm["content"]); strings.TrimSpace(t) != "" {
			items = append(items, Item{Kind: "tool_result", Text: truncate(t)})
		}
	}
	return items
}

// resultText flattens a tool_result's content, which the CLI emits either as
// a plain string or as a list of typed blocks.
func resultText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, c := range v {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
				if t, ok := cm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

func decodeResult(obj map[string]any) []Item {
	it := Item{Kind: "result"}
	if v, ok := obj["total_cost_usd"].(float64); ok {
		it.CostUSD = v
	}
	if v, ok := obj["duration_ms"].(float64); ok {
		it.DurationMS = int64(v)
	}
	return []Item{it}
}

// Summary renders a result item's stats as a compact human string, e.g.
// "$0.0312 · 42s". Empty when the CLI didn't report stats.
func (it Item) Summary() string {
	if it.Kind != "result" {
		return ""
	}
	var parts []string
	if it.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", it.CostUSD))
	}
	if it.DurationMS > 0 {
		secs := float64(it.DurationMS) / 1000
		if secs >= 60 {
			parts = append(parts, fmt.Sprintf("%dm%02ds", int(secs)/60, int(secs)%60))
		} else {
			parts = append(parts, fmt.Sprintf("%.0fs", secs))
		}
	}
	return strings.Join(parts, " · ")
}

func truncate(s string) string {
	if len(s) <= truncateAt {
		return s
	}
	return s[:truncateAt] + " …[truncated]"
}

// DecodeLine parses one raw line then decodes it — a convenience for tests
// and any consumer holding bytes instead of a parsed map.
func DecodeLine(line []byte) []Item {
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		return nil
	}
	return Decode(obj)
}
