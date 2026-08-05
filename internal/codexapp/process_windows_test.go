//go:build windows

package codexapp

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// longLivedChild starts a process that will not exit on its own, so the tests
// below can observe the identity handle against a live process.
func longLivedChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeAppServerProcess$")
	cmd.Env = append(os.Environ(),
		"SHIPMATES_FAKE_APP_SERVER=1", "SHIPMATES_FAKE_SCENARIO=ignore-close")
	// Give it a stdin it will block reading, so it does not exit before we look.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// openProcessIdentity used to return os.ErrInvalid on Windows. Factory.Start
// treats a failure there as fatal, so that stub made *every* Codex app-server
// launch on Windows fail with "the Codex app-server could not be started". This
// asserts the contract Start depends on: a real, usable handle.
func TestOpenProcessIdentityReturnsUsableHandle(t *testing.T) {
	cmd := longLivedChild(t)
	fd, err := openProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("openProcessIdentity: %v", err)
	}
	if fd <= 0 {
		t.Fatalf("handle = %d, want a real handle", fd)
	}
	defer func() {
		if err := closeProcessIdentity(fd); err != nil {
			t.Errorf("closeProcessIdentity: %v", err)
		}
	}()

	// SYNCHRONIZE was requested, so the handle must be waitable. A live process
	// times out rather than signalling.
	state, err := windows.WaitForSingleObject(windows.Handle(fd), 0)
	if err != nil {
		t.Fatalf("WaitForSingleObject: %v", err)
	}
	if state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("wait state = %d, want WAIT_TIMEOUT for a live process", state)
	}

	// The graceful half is a documented no-op: these children have no console, so
	// there is no SIGINT analogue to deliver. It must report success rather than
	// an error, because callers map a signalling error onto CleanupFailed.
	if err := signalProcessIdentity(fd, false); err != nil {
		t.Fatalf("graceful signal on windows should be a successful no-op, got %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if state, err := windows.WaitForSingleObject(windows.Handle(fd), 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("graceful no-op terminated the child: state=%d err=%v", state, err)
	}

	// PROCESS_TERMINATE was requested, so the kill must land.
	if err := signalProcessIdentity(fd, true); err != nil {
		t.Fatalf("kill via process identity: %v", err)
	}
	if state, err := windows.WaitForSingleObject(windows.Handle(fd), 5000); err != nil || state != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("child did not exit after kill: state=%d err=%v", state, err)
	}

	// An already-exited process reports success. On unix SIGKILL to an unreaped
	// child succeeds, and callers treat a kill error as CleanupFailed; without the
	// exited check TerminateProcess would return ERROR_ACCESS_DENIED for a child
	// that had already done exactly what was asked.
	if err := signalProcessIdentity(fd, true); err != nil {
		t.Fatalf("kill of an already-exited process should succeed, got %v", err)
	}
}

// The handle names one process object for as long as it is held, which is the
// pidfd property that matters: the identity cannot drift onto a recycled PID.
func TestProcessIdentityHandleOutlivesTheProcess(t *testing.T) {
	cmd := longLivedChild(t)
	fd, err := openProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := signalProcessIdentity(fd, true); err != nil {
		t.Fatal(err)
	}
	if _, err := windows.WaitForSingleObject(windows.Handle(fd), 5000); err != nil {
		t.Fatal(err)
	}
	// Still queryable after exit — the object is kept alive by our reference.
	var code uint32
	if err := windows.GetExitCodeProcess(windows.Handle(fd), &code); err != nil {
		t.Fatalf("handle unusable after process exit: %v", err)
	}
	if err := closeProcessIdentity(fd); err != nil {
		t.Fatalf("closeProcessIdentity: %v", err)
	}
}
