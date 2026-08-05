package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
	"github.com/luthermonson/shipmates/internal/runtime/containment/watchdog"
)

// These tests are the reason the cgroup path could be deleted. They run the
// codex runtime against a fake app-server that behaves like a runaway backend —
// one spawns a long-lived grandchild, one eats memory — and assert the
// watchdog actually stops it. A test that only proved the wiring compiles would
// prove nothing: the guarantee being claimed is that a codex process tree is
// bounded and can be torn down, on every platform.

const (
	fakeServerEnv    = "SHIPMATES_CODEX_FAKE_APP_SERVER"
	fakeScenarioEnv  = "SHIPMATES_CODEX_FAKE_SCENARIO"
	fakeGrandchildEn = "SHIPMATES_CODEX_FAKE_GRANDCHILD"
	portFileEnv      = "SHIPMATES_CODEX_PORT_FILE"
)

// TestFakeCodexAppServerProcess is a child fixture, not a test. It speaks just
// enough of the app-server handshake for codexapp.Factory.Start to succeed,
// then misbehaves according to the scenario.
func TestFakeCodexAppServerProcess(t *testing.T) {
	if os.Getenv(fakeServerEnv) != "1" {
		return
	}
	// SIGINT must not be the thing that ends this process: the adapter's
	// cooperative step sends it to the ROOT only, and a root that dies there
	// would leave the tree untested. Ignoring it forces Close to escalate into
	// the supervisor's tree teardown, which is what these tests measure.
	signal.Ignore(os.Interrupt)

	dec := json.NewDecoder(os.Stdin)
	var init map[string]any
	if err := dec.Decode(&init); err != nil {
		os.Exit(2)
	}
	reply, _ := json.Marshal(map[string]any{
		"id":     init["id"],
		"result": map[string]any{"userAgent": "codex-cli 0.144.1"},
	})
	fmt.Println(string(reply))

	switch os.Getenv(fakeScenarioEnv) {
	case "spawn-grandchild":
		// A child of the app-server, exactly like a tool call codex would run.
		// It inherits the process group / Job Object, so only real tree
		// teardown reaches it.
		grand := exec.Command(os.Args[0], "-test.run=^TestFakeCodexGrandchildProcess$")
		grand.Env = append(os.Environ(), fakeGrandchildEn+"=1")
		if err := grand.Start(); err != nil {
			os.Exit(3)
		}
	case "spin":
		// Burn CPU the way a wedged backend would, for far longer than any
		// limit the test sets, so only the watchdog can end it.
		deadline := time.Now().Add(5 * time.Minute)
		x := 1
		for time.Now().Before(deadline) {
			for i := 0; i < 5_000_000; i++ {
				x = (x * 31) % 1000003
			}
		}
		os.Exit(x & 1)
	}
	time.Sleep(5 * time.Minute)
	os.Exit(0)
}

// TestFakeCodexGrandchildProcess is the runaway descendant. It publishes a
// loopback listener and holds it: reachable means alive, refused means the
// process is gone. A liveness probe beats polling a PID, which lies about
// zombies on Unix and about handle-held PIDs on Windows.
func TestFakeCodexGrandchildProcess(t *testing.T) {
	if os.Getenv(fakeGrandchildEn) != "1" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(4)
	}
	defer ln.Close()
	if path := os.Getenv(portFileEnv); path != "" {
		line := ln.Addr().String() + " " + strconv.Itoa(os.Getpid())
		if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
			os.Exit(5)
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	time.Sleep(5 * time.Minute)
	os.Exit(0)
}

func fakeStartOptions(t *testing.T, scenario string, sup codexapp.Supervisor) codexapp.StartOptions {
	t.Helper()
	return codexapp.StartOptions{
		WorkingDirectory: t.TempDir(),
		Command:          []string{os.Args[0], "-test.run=^TestFakeCodexAppServerProcess$"},
		Environment: map[string]string{
			fakeServerEnv:   "1",
			fakeScenarioEnv: scenario,
			portFileEnv:     filepath.Join(t.TempDir(), "grandchild.addr"),
		},
		StartupTimeout:  30 * time.Second,
		ShutdownTimeout: 300 * time.Millisecond,
		Supervisor:      sup,
	}
}

// A codex app-server's own children must die with it. This is the containment
// guarantee the cgroup path used to provide on Linux only; the watchdog now
// provides it on Linux, macOS and Windows through process groups and Job
// Objects.
func TestCodexTreeTeardownKillsTheAppServersChildren(t *testing.T) {
	opts := fakeStartOptions(t, "spawn-grandchild", Contain(watchdog.New(), containment.Limits{
		GracefulTimeout: 500 * time.Millisecond,
	}))
	addrFile := opts.Environment[portFileEnv]

	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("start codex runtime: %v", err)
	}
	if !rt.Capabilities().Containment {
		t.Fatal("Caps.Containment is false for a watchdog-supervised transport")
	}

	addr, _ := waitForGrandchild(t, addrFile)
	if !reachable(addr) {
		t.Fatalf("grandchild at %s was never alive; the test proves nothing", addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for reachable(addr) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild at %s outlived the codex transport; the process tree was not torn down", addr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The "none" posture is the control: it starts the child and nothing else, so
// the grandchild survives. Without this, a tree-teardown test could pass for
// reasons that have nothing to do with containment — the fixture crashing, the
// OS reaping an orphan — and nobody would know.
func TestCodexWithoutContainmentLeavesTheChildrenBehind(t *testing.T) {
	opts := fakeStartOptions(t, "spawn-grandchild", Contain(none.New(), containment.Limits{}))
	addrFile := opts.Environment[portFileEnv]

	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("start codex runtime: %v", err)
	}
	if rt.Capabilities().Containment {
		t.Fatal("Caps.Containment is true for the \"none\" posture, which bounds nothing")
	}
	addr, _ := waitForGrandchild(t, addrFile)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = rt.Close(ctx)

	// Give the uncontained case the same chance to die that the contained case
	// gets, so this is a real difference in behavior and not a timing artifact.
	time.Sleep(3 * time.Second)
	if !reachable(addr) {
		t.Fatal("the uncontained grandchild died anyway; the teardown test above may be measuring something else")
	}
}

// A runaway codex backend must be killed by the operator's limit, unprompted,
// and the runtime must say so. This is the half of containment that the config
// block promises and that nothing verified before: the limits an operator
// writes now reach the codex app-server, not just the claude one.
//
// CPU-seconds is the limit under test because it is sampled identically on
// every platform. The memory cap is deliberately NOT asserted here: on Windows
// the Job Object commit limit holds the child BELOW the cap instead of killing
// it, so the RSS sampler never sees a breach — a real platform difference that
// belongs to the watchdog's own tests, not to a test about codex wiring.
func TestCodexCPULimitKillsTheRunawayAppServer(t *testing.T) {
	opts := fakeStartOptions(t, "spin", Contain(watchdog.New(), containment.Limits{
		MaxCPUSeconds:   2,
		PollInterval:    100 * time.Millisecond,
		GracefulTimeout: 500 * time.Millisecond,
	}))
	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("start codex runtime: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	}()

	// Nobody calls Close inside the loop: the point is that the watchdog ends
	// the process by itself, because it breached the operator's cap.
	deadline := time.After(90 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("event stream closed without reporting the kill")
			}
			if ev.Kind == runtime.KindError {
				return
			}
		case <-deadline:
			t.Fatal("the app-server burned far past its CPU limit and was never killed")
		}
	}
}

// The same limits under the "none" posture must NOT kill it. Without this the
// test above could pass because the fixture crashed on its own, and the
// capability report would be built on a coincidence.
func TestCodexWithoutContainmentDoesNotEnforceLimits(t *testing.T) {
	opts := fakeStartOptions(t, "spin", Contain(none.New(), containment.Limits{
		MaxCPUSeconds: 2,
		PollInterval:  100 * time.Millisecond,
	}))
	rt, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("start codex runtime: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	}()

	select {
	case ev, ok := <-rt.Events():
		t.Fatalf("uncontained app-server ended anyway (event %+v, open=%v); the enforcement test may be measuring something else", ev, ok)
	case <-time.After(15 * time.Second):
	}
}

// waitForGrandchild blocks until the grandchild has published its listener
// address and PID, and registers a best-effort kill so a test that is meant to
// leave it running does not leak it into the rest of the suite.
func waitForGrandchild(t *testing.T, path string) (addr string, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(b))
			if len(fields) == 2 {
				pid, err = strconv.Atoi(fields[1])
				if err == nil {
					addr := fields[0]
					t.Cleanup(func() {
						if p, err := os.FindProcess(pid); err == nil {
							_ = p.Kill()
						}
						// Wait for it to really be gone. Kill is asynchronous,
						// and a survivor still holding the fixture's working
						// directory makes t.TempDir's own cleanup fail on
						// Windows — noise that looks like a test bug.
						until := time.Now().Add(10 * time.Second)
						for reachable(addr) && time.Now().Before(until) {
							time.Sleep(50 * time.Millisecond)
						}
					})
					return addr, pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild never published its address to %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// reachable reports whether something is still accepting on addr.
func reachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
