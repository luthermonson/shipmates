//go:build windows

package watchdog

import (
	"context"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// queryJob reads back the extended limit information programmed on the Job
// Object recorded for pid, so the tests can assert on what the kernel
// actually holds rather than on what attach meant to set.
func queryJob(t *testing.T, pid int) windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
	t.Helper()
	jobsMu.Lock()
	job, ok := jobs[pid]
	jobsMu.Unlock()
	if !ok {
		t.Fatalf("no job handle recorded for pid %d", pid)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var retlen uint32
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		&retlen,
	); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	return info
}

func TestAttach_SetsMemoryLimitFlags(t *testing.T) {
	const memCap = int64(64 << 20)
	limits := containment.Limits{MaxRSSBytes: memCap}
	h, err := New().Start(sleeper(30*time.Second), limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	info := queryJob(t, h.Pid())
	flags := info.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Errorf("KILL_ON_JOB_CLOSE not set; flags=%#x", flags)
	}
	if flags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY == 0 {
		t.Errorf("JOB_MEMORY not set; flags=%#x", flags)
	}
	if int64(info.JobMemoryLimit) != memCap {
		t.Errorf("JobMemoryLimit = %d, want %d", info.JobMemoryLimit, memCap)
	}
	if flags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS != 0 {
		t.Errorf("ACTIVE_PROCESS set although MaxProcesses was 0; flags=%#x", flags)
	}
}

func TestAttach_SetsActiveProcessLimit(t *testing.T) {
	limits := containment.Limits{MaxProcesses: 8}
	h, err := New().Start(sleeper(30*time.Second), limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	info := queryJob(t, h.Pid())
	flags := info.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS == 0 {
		t.Errorf("ACTIVE_PROCESS not set; flags=%#x", flags)
	}
	if info.BasicLimitInformation.ActiveProcessLimit != 8 {
		t.Errorf("ActiveProcessLimit = %d, want 8", info.BasicLimitInformation.ActiveProcessLimit)
	}
	if flags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY != 0 {
		t.Errorf("JOB_MEMORY set although MaxRSSBytes was 0; flags=%#x", flags)
	}
}

// With empty Limits only the tree-kill flag may be set: a caller who opted
// into no caps must not silently acquire kernel enforcement.
func TestAttach_NoLimits(t *testing.T) {
	h, err := New().Start(sleeper(30*time.Second), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer h.Close(context.Background())

	info := queryJob(t, h.Pid())
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	if got := info.BasicLimitInformation.LimitFlags; got != want {
		t.Errorf("LimitFlags = %#x, want %#x (KILL_ON_JOB_CLOSE only)", got, want)
	}
	if info.JobMemoryLimit != 0 {
		t.Errorf("JobMemoryLimit = %d, want 0", info.JobMemoryLimit)
	}
}

// The job handle must not outlive the process. A shipmates server spawns a
// turn per message; one leaked kernel handle per turn is a slow leak in a
// long-lived process.
func TestRelease_DropsJobHandleAfterExit(t *testing.T) {
	h, err := New().Start(sleeper(100*time.Millisecond), containment.Limits{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := h.Pid()
	select {
	case <-h.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("child never exited")
	}
	jobsMu.Lock()
	_, still := jobs[pid]
	jobsMu.Unlock()
	if still {
		t.Errorf("job handle for pid %d still recorded after exit", pid)
	}
}
