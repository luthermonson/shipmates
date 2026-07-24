//go:build windows

package watchdog

import (
	"context"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// TestAttach_SetsMemoryLimitFlags verifies that when Limits.MaxRSSBytes is
// non-zero, attach programs JOB_OBJECT_LIMIT_JOB_MEMORY on the underlying
// Job Object and stores the requested byte value in JobMemoryLimit. We
// prove this by round-tripping through QueryInformationJobObject.
func TestAttach_SetsMemoryLimitFlags(t *testing.T) {
	const memCap = int64(64 * 1024 * 1024) // 64 MiB
	cmd := sleeper(30)
	limits := containment.Limits{MaxRSSBytes: memCap}
	if err := prepare(cmd, limits); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	if err := attach(cmd, limits); err != nil {
		t.Fatalf("attach: %v", err)
	}

	jobsMu.Lock()
	job, ok := jobs[cmd.Process.Pid]
	jobsMu.Unlock()
	if !ok {
		t.Fatalf("no job handle recorded for pid %d", cmd.Process.Pid)
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

	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Errorf("JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE not set; flags=%#x", info.BasicLimitInformation.LimitFlags)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY == 0 {
		t.Errorf("JOB_OBJECT_LIMIT_JOB_MEMORY not set; flags=%#x", info.BasicLimitInformation.LimitFlags)
	}
	if int64(info.JobMemoryLimit) != memCap {
		t.Errorf("JobMemoryLimit=%d want %d", info.JobMemoryLimit, memCap)
	}
	// MaxProcesses was zero, so the active-process flag should NOT be set.
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS != 0 {
		t.Errorf("unexpected JOB_OBJECT_LIMIT_ACTIVE_PROCESS set with MaxProcesses=0; flags=%#x", info.BasicLimitInformation.LimitFlags)
	}
}

// TestAttach_SetsActiveProcessLimit verifies MaxProcesses maps to
// ActiveProcessLimit and the corresponding flag bit.
func TestAttach_SetsActiveProcessLimit(t *testing.T) {
	cmd := sleeper(30)
	limits := containment.Limits{MaxProcesses: 8}
	if err := prepare(cmd, limits); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	if err := attach(cmd, limits); err != nil {
		t.Fatalf("attach: %v", err)
	}

	jobsMu.Lock()
	job, ok := jobs[cmd.Process.Pid]
	jobsMu.Unlock()
	if !ok {
		t.Fatalf("no job handle recorded for pid %d", cmd.Process.Pid)
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

	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS == 0 {
		t.Errorf("JOB_OBJECT_LIMIT_ACTIVE_PROCESS not set; flags=%#x", info.BasicLimitInformation.LimitFlags)
	}
	if info.BasicLimitInformation.ActiveProcessLimit != 8 {
		t.Errorf("ActiveProcessLimit=%d want 8", info.BasicLimitInformation.ActiveProcessLimit)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY != 0 {
		t.Errorf("unexpected JOB_OBJECT_LIMIT_JOB_MEMORY set with MaxRSSBytes=0; flags=%#x", info.BasicLimitInformation.LimitFlags)
	}
}

// TestAttach_NoLimits verifies that with an empty Limits, only the
// KILL_ON_JOB_CLOSE flag is set — i.e. we don't accidentally enable
// kernel enforcement for callers who didn't opt in.
func TestAttach_NoLimits(t *testing.T) {
	cmd := sleeper(30)
	if err := prepare(cmd, containment.Limits{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer cmd.Process.Kill()
	if err := attach(cmd, containment.Limits{}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	jobsMu.Lock()
	job, ok := jobs[cmd.Process.Pid]
	jobsMu.Unlock()
	if !ok {
		t.Fatalf("no job handle recorded for pid %d", cmd.Process.Pid)
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

	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)
	if info.BasicLimitInformation.LimitFlags != want {
		t.Errorf("LimitFlags=%#x want %#x (KILL_ON_JOB_CLOSE only)",
			info.BasicLimitInformation.LimitFlags, want)
	}
	if info.JobMemoryLimit != 0 {
		t.Errorf("JobMemoryLimit=%d want 0", info.JobMemoryLimit)
	}
}

// TestKernelMemoryLimit_KillsAllocator spawns a PowerShell process with a
// tight 32 MiB job memory cap and asks it to allocate 128 MiB. The kernel
// should fail the allocation and the process should die before the sleep
// completes. This is the end-to-end proof that the kernel enforcement is
// wired correctly.
//
// This test is somewhat environment-sensitive — PowerShell startup timing
// and CI resource pressure can make it flaky — so failures fall through to
// t.Skip rather than t.Fatal if PowerShell isn't available.
func TestKernelMemoryLimit_KillsAllocator(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("powershell.exe not available")
	}
	// Allocate ~128 MiB in a single shot; well above the 32 MiB cap.
	// Sleep afterward so a successful allocation would linger visibly.
	script := `$a = New-Object byte[](128MB); Start-Sleep -Seconds 10`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)

	w := New()
	limits := containment.Limits{
		MaxRSSBytes:     32 * 1024 * 1024, // 32 MiB
		GracefulTimeout: 500 * time.Millisecond,
	}
	h, err := w.Start(cmd, limits)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	select {
	case ev := <-h.Done():
		// The kernel-forced allocation failure should terminate PowerShell
		// with a non-zero exit. We're not picky about the exact reason
		// classification: today the watchdog surfaces it as ReasonExited
		// (natural exit) because we don't listen to the job's completion
		// port. What we DO care about: the process died promptly and did
		// NOT complete the 10s sleep, and the exit code is not the "OK, all
		// done" 0. That combination proves the kernel cap fired.
		if ev.ExitCode == 0 {
			t.Errorf("process exited cleanly despite exceeding memory cap; ev=%+v", ev)
		}
		t.Logf("terminal event: reason=%s exit=%d detail=%q",
			ev.Reason, ev.ExitCode, ev.Detail)
	case <-time.After(8 * time.Second):
		// If the process is still alive after 8s, the 128 MiB allocation
		// clearly did NOT fail — the memory cap isn't being enforced.
		_ = h.Close(context.Background())
		t.Fatalf("process survived past 8s; kernel memory cap not enforced")
	}
}
