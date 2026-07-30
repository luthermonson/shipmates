//go:build windows

package watchdog

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// prepare marks the process to be created suspended so it can be assigned to
// a Job Object before it runs any code, and as the root of a new process
// group so console signals do not propagate upstream. Limits cannot be
// programmed before the Job Object exists, so they are consumed in attach.
func prepare(cmd *exec.Cmd, _ containment.Limits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	return nil
}

// attach creates a Job Object, applies "kill everything in the job when it
// closes" plus any kernel-enforced caps from limits, assigns the still-
// suspended process to it, and resumes it.
//
// This is Windows' native answer to cgroups: every descendant automatically
// joins the job and dies with it, and memory and process-count caps are
// enforced by the kernel with no polling gap. The sampler still runs on top
// as defense in depth, and it remains the only enforcement for CPU-seconds,
// which Job Objects do not express this way.
func attach(cmd *exec.Cmd, limits containment.Limits) error {
	if cmd.Process == nil {
		return fmt.Errorf("watchdog: cmd not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("watchdog: CreateJobObject: %w", err)
	}
	// Start with tree-kill semantics and layer on the requested caps.
	// Setting a LimitFlags bit without also setting its paired value field
	// is a no-op, so each addition is guarded on a non-zero limit — that is
	// also what keeps a caller who opted into nothing from silently getting
	// kernel enforcement.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if limits.MaxRSSBytes > 0 {
		// JobMemoryLimit is total committed memory across the job. Once it
		// is hit, allocations fail in the kernel — no polling gap. Most
		// runtimes' allocate-then-die behavior means the process goes
		// promptly.
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.MaxRSSBytes)
	}
	if limits.MaxProcesses > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = limits.MaxProcesses
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("watchdog: SetInformationJobObject: %w", err)
	}
	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("watchdog: OpenProcess: %w", err)
	}
	defer windows.CloseHandle(procHandle)
	if err := windows.AssignProcessToJobObject(job, procHandle); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("watchdog: AssignProcessToJobObject: %w", err)
	}
	// In the job now — safe to let it run.
	if err := resumeMainThread(cmd); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("watchdog: resume: %w", err)
	}
	jobsMu.Lock()
	jobs[cmd.Process.Pid] = job
	jobsMu.Unlock()
	return nil
}

// killTree terminates the Job Object, taking every process in the tree with
// it. kill=false attempts a cooperative Ctrl+Break to the process group; the
// caller's escalation ladder decides when to insist.
func killTree(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	if !kill {
		return sendCtrlBreak(cmd.Process.Pid)
	}
	job, ok := takeJob(cmd.Process.Pid)
	if !ok {
		return cmd.Process.Kill()
	}
	defer windows.CloseHandle(job)
	return windows.TerminateJobObject(job, 1)
}

// release drops the Job Object handle for a process that has already exited,
// so a long-lived shipmates process does not accumulate one handle per turn.
// Closing the last handle also kills any survivor in the job, which is the
// behavior we want: KILL_ON_JOB_CLOSE is set precisely so an orphan the
// runtime spawned cannot outlive it.
func release(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if job, ok := takeJob(cmd.Process.Pid); ok {
		_ = windows.CloseHandle(job)
	}
}

// takeJob removes and returns the job handle recorded for pid.
func takeJob(pid int) (windows.Handle, bool) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	job, ok := jobs[pid]
	if ok {
		delete(jobs, pid)
	}
	return job, ok
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
// golang.org/x/sys/windows does not export it, so it is declared here and
// psapi.dll is called directly.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var (
	psapi                    = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// sampleRSS calls psapi!GetProcessMemoryInfo; WorkingSetSize is Windows'
// analog of RSS.
func sampleRSS(pid int) (int64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	r, _, callErr := procGetProcessMemoryInfo.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if r == 0 {
		return 0, fmt.Errorf("watchdog: GetProcessMemoryInfo: %v", callErr)
	}
	return int64(counters.WorkingSetSize), nil
}

// sampleCPUSeconds uses GetProcessTimes to return kernel + user time.
func sampleCPUSeconds(pid int) (float64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeSeconds(kernel) + filetimeSeconds(user), nil
}

// filetimeSeconds converts a FILETIME's 100-nanosecond units to seconds.
func filetimeSeconds(ft windows.Filetime) float64 {
	units := (int64(ft.HighDateTime) << 32) | int64(ft.LowDateTime)
	return float64(units) / 1e7
}

// resumeMainThread starts the primary thread of the suspended process.
// os/exec does not expose the thread handle, so the thread list is snapshotted
// and the first thread belonging to our pid is resumed.
func resumeMainThread(cmd *exec.Cmd) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	if err := windows.Thread32First(snapshot, &te); err != nil {
		return err
	}
	for {
		if te.OwnerProcessID == uint32(cmd.Process.Pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
			if err != nil {
				return err
			}
			_, err = windows.ResumeThread(thread)
			windows.CloseHandle(thread)
			return err
		}
		if err := windows.Thread32Next(snapshot, &te); err != nil {
			return fmt.Errorf("watchdog: no thread found for pid %d", cmd.Process.Pid)
		}
	}
}

// sendCtrlBreak posts a Ctrl+Break to the process group. The child must be in
// its own group (CREATE_NEW_PROCESS_GROUP, set in prepare) for this to be
// well-behaved.
func sendCtrlBreak(pid int) error {
	r, _, err := procGenerateConsoleCtrlEvent.Call(uintptr(syscall.CTRL_BREAK_EVENT), uintptr(pid))
	if r == 0 {
		return err
	}
	return nil
}

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

// jobs maps pid to job handle, because os/exec does not hand the job back
// after AssignProcessToJobObject.
var (
	jobsMu sync.Mutex
	jobs   = map[int]windows.Handle{}
)
