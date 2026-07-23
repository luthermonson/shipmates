package factory

import (
	"context"
	"strings"
	"testing"
)

func TestNewFromConfig_Claude(t *testing.T) {
	rt, err := NewFromConfig(context.Background(), "claude", map[string]any{
		"binary":       "/opt/claude/bin/claude",
		"default_args": []any{"--verbose", "--json"},
	})
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q want claude", rt.Name())
	}
	if !rt.Capabilities().Streaming {
		t.Errorf("claude should report Streaming=true")
	}
}

func TestNewFromConfig_ClaudeIgnoresGarbageSettings(t *testing.T) {
	// Wrong types should be silently ignored rather than crash — the whole
	// point of settings map[string]any is graceful degradation across
	// config-file drift.
	rt, err := NewFromConfig(context.Background(), "claude", map[string]any{
		"binary":       42,
		"default_args": "not a list",
	})
	if err != nil {
		t.Fatalf("claude with bad settings: %v", err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
}

func TestNewFromConfig_ClaudeEmptySettings(t *testing.T) {
	rt, err := NewFromConfig(context.Background(), "claude", nil)
	if err != nil {
		t.Fatalf("claude nil settings: %v", err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
}

func TestNewFromConfig_CodexRefuses(t *testing.T) {
	// Codex needs StartOptions the config file cannot reasonably carry.
	// Factory refuses; NewCodexWith is the dedicated path.
	_, err := NewFromConfig(context.Background(), "codex", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "NewCodexWith") {
		t.Errorf("error message should point at NewCodexWith; got %q", err.Error())
	}
}

func TestNewFromConfig_UnknownRuntime(t *testing.T) {
	_, err := NewFromConfig(context.Background(), "gpt", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown; got %q", err.Error())
	}
}

func TestNames_ExactSet(t *testing.T) {
	got := Names()
	want := map[string]bool{"claude": true, "codex": true}
	if len(got) != len(want) {
		t.Errorf("got %d names, want %d", len(got), len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected name %q", n)
		}
	}
}
