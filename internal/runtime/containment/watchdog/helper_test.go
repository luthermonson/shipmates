package watchdog

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The tests here need child processes that sleep, burn CPU and allocate
// memory, on Linux, macOS and Windows alike. Rather than depend on `sleep`,
// `ping /n` and PowerShell all behaving, they re-exec the test binary itself
// and switch on an environment variable — the same trick os/exec's own tests
// use. One code path, identical behavior on every platform, and the memory
// hog is a Go allocation instead of a shell incantation.

const (
	helperEnv    = "SHIPMATES_WATCHDOG_HELPER"
	helperArgEnv = "SHIPMATES_WATCHDOG_HELPER_ARG"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "sleep":
		d, err := time.ParseDuration(os.Getenv(helperArgEnv))
		if err != nil {
			os.Exit(2)
		}
		time.Sleep(d)
		os.Exit(0)
	case "spin":
		// Burn CPU for long enough that a CPU-seconds limit must fire first.
		deadline := time.Now().Add(60 * time.Second)
		x := 1
		for time.Now().Before(deadline) {
			for i := 0; i < 5_000_000; i++ {
				x = (x * 31) % 1000003
			}
		}
		os.Exit(x & 1)
	case "hog":
		// Grow the resident set in visible steps, touching every page so it
		// is really resident, then linger. On Windows the Job Object memory
		// cap fails the allocation in the kernel; elsewhere the sampler
		// notices and kills the tree.
		var chunks [][]byte
		for range 64 {
			c := make([]byte, 8<<20) // 8 MiB
			for i := range c {
				c[i] = byte(i)
			}
			chunks = append(chunks, c)
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(30 * time.Second)
		os.Exit(len(chunks) & 1)
	case "hogchild":
		// Stay small in the root and spawn a CHILD (grandchild of the test)
		// that does the hogging. The child inherits this process's group —
		// which the watchdog made this process the leader of — so a
		// process-group sampler must count the child's RSS against the cap,
		// while a root-only sampler never would. This is the case that
		// distinguishes tree-scoped enforcement from root-only.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=hog", helperArgEnv+"=")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		// Linger so the root outlives the child's allocation; the watchdog
		// kills the whole group on breach.
		_ = child.Wait()
		os.Exit(0)
	default:
		os.Exit(3)
	}
}

// helperCmd builds an exec.Cmd that re-runs this test binary in helper mode.
func helperCmd(mode, arg string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperArgEnv+"="+arg)
	return cmd
}

// sleeper returns a child that sleeps for d and exits 0.
func sleeper(d time.Duration) *exec.Cmd { return helperCmd("sleep", d.String()) }
