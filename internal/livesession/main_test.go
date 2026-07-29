package livesession

import (
	"os"
	"runtime/debug"
	"testing"
)

// TestMain caps the test process's heap. A runaway that allocates live memory
// (a Windows path-walk bug in secureStateDir once committed 236 GB and took
// the desktop down) degrades into GC pressure and a test timeout instead of
// exhausting the machine's commit limit. The limit is soft, so it throttles
// rather than halts growth — the backstop, not the fix.
func TestMain(m *testing.M) {
	debug.SetMemoryLimit(4 << 30)
	os.Exit(m.Run())
}
