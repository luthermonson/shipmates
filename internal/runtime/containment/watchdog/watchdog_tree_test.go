//go:build linux || darwin

package watchdog

import (
	"context"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// The memory cap must contain the whole process tree, not just the root the
// watchdog launched. Here the root stays small and a CHILD it spawns (a
// grandchild of the test) does the hogging. A root-only sampler — the pre-fix
// behavior, which read RSS for h.cmd.Process.Pid alone — never sees the child's
// RSS climb, so no breach would ever fire and this test would time out. The
// tree-scoped sampler sums the child's process group and fires ReasonMemoryLimit.
//
// This runs only where the Unix samplers exist (Linux/macOS); on Windows the
// Job Object enforces the tree in the kernel and TestMemoryLimit_StopsRunawayAllocator
// already covers that path. CI's linux leg exercises this test.
func TestMemoryLimit_StopsRunawayChild(t *testing.T) {
	limits := containment.Limits{
		MaxRSSBytes:     48 << 20, // 48 MiB — the child wants 512 MiB
		PollInterval:    50 * time.Millisecond,
		GracefulTimeout: 500 * time.Millisecond,
	}
	h, err := New().Start(helperCmd("hogchild", ""), limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	ev := awaitDone(t, h, 25*time.Second)
	t.Logf("terminal event: reason=%s exit=%d detail=%q", ev.Reason, ev.ExitCode, ev.Detail)
	if ev.Reason != containment.ReasonMemoryLimit {
		t.Fatalf("reason = %q, want memory_limit — a hogging child escaped the tree-wide RSS cap (a root-only sampler would miss it); detail=%q", ev.Reason, ev.Detail)
	}
}
