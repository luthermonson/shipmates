package backend

import "testing"

func TestExplicitCapabilities(t *testing.T) {
	tests := []struct {
		name string
		kind string
		want Capability
	}{
		{"claude", "claude", Headless | Interactive | LiveTell | PTY},
		{"codex", "codex", Headless},
		{"command", "command", PTY},
		{"unknown", "other", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.kind)
			if got.Capabilities != tt.want {
				t.Fatalf("capabilities = %04b, want %04b", got.Capabilities, tt.want)
			}
		})
	}
}
