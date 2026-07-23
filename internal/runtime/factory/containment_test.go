package factory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/config"
)

func TestNewFromResolved_WatchdogDefault(t *testing.T) {
	rt, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime: "claude",
		Containment: config.Containment{
			Mode:              "watchdog",
			MemoryLimitMB:     4096,
			PollIntervalMS:    250,
			GracefulTimeoutMS: 3000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
}

func TestNewFromResolved_NoneMode(t *testing.T) {
	rt, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime:     "claude",
		Containment: config.Containment{Mode: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
}

func TestNewFromResolved_CgroupDegradesToWatchdog(t *testing.T) {
	// Until the cgroup adapter lands, cgroup mode should degrade to
	// watchdog rather than fail — operators who picked cgroup optimistically
	// still get bounded processes.
	rt, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime:     "claude",
		Containment: config.Containment{Mode: "cgroup", MemoryLimitMB: 2048},
	})
	if err != nil {
		t.Fatalf("cgroup should degrade to watchdog, not error: %v", err)
	}
	if rt.Name() != "claude" {
		t.Errorf("Name()=%q", rt.Name())
	}
}

func TestNewFromResolved_UnknownMode(t *testing.T) {
	_, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime:     "claude",
		Containment: config.Containment{Mode: "quantum"},
	})
	if err == nil {
		t.Fatal("expected error for unknown containment mode")
	}
}

func TestNewFromResolved_CodexRoutedToNewCodexWith(t *testing.T) {
	// Codex needs StartOptions the config file cannot reasonably carry.
	// NewFromResolved should refuse and point at NewCodexWith.
	_, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime:     "codex",
		Containment: config.Containment{Mode: "watchdog"},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "NewCodexWith") {
		t.Errorf("error should point at NewCodexWith; got %q", err.Error())
	}
}

func TestNewFromResolved_UnknownRuntime(t *testing.T) {
	_, err := NewFromResolved(context.Background(), config.Resolved{
		Runtime:     "gpt",
		Containment: config.Containment{Mode: "watchdog"},
	})
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention unknown; got %q", err.Error())
	}
}

func TestContainmentFor_LimitsMapping(t *testing.T) {
	watcher, limits, err := containmentFor(config.Containment{
		Mode:              "watchdog",
		MemoryLimitMB:     100,
		CPULimitSeconds:   60,
		PollIntervalMS:    100,
		GracefulTimeoutMS: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if watcher.Kind() != "watchdog" {
		t.Errorf("Kind()=%q", watcher.Kind())
	}
	if limits.MaxRSSBytes != 100*1024*1024 {
		t.Errorf("MaxRSSBytes=%d", limits.MaxRSSBytes)
	}
	if limits.MaxCPUSeconds != 60 {
		t.Errorf("MaxCPUSeconds=%v", limits.MaxCPUSeconds)
	}
	if limits.PollInterval != 100*time.Millisecond {
		t.Errorf("PollInterval=%v", limits.PollInterval)
	}
	if limits.GracefulTimeout != 500*time.Millisecond {
		t.Errorf("GracefulTimeout=%v", limits.GracefulTimeout)
	}
}

func TestContainmentFor_NoneIgnoresLimits(t *testing.T) {
	_, limits, _ := containmentFor(config.Containment{Mode: "none", MemoryLimitMB: 999})
	if limits.MaxRSSBytes != 0 {
		t.Errorf("none mode should not carry limits, got MaxRSSBytes=%d", limits.MaxRSSBytes)
	}
}
