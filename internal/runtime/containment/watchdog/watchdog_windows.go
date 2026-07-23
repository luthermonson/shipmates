//go:build windows

package watchdog

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepare marks the process to be created suspended so we can assign it to
// a Job Object before it starts running any code. It also flags the process
// as the root of a new process group so console signals don't propagate
// upstream.
func prepare(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	return nil
}

// attach creates a Job Object, applies "kill everything in the job when it
// closes" semantics, assigns the started (still-suspended) process to it,
// then resumes the process. This is Windows' native answer to cgroups:
// every descendant automatically joins the job and is killed with it.
//
// We stash the job handle on the process handle via a package-level map so
// killTree can Terminate the job later.
func attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("watchdog: cmd not started")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("watchdog: CreateJobObject: %w", err)
	}
	// Configure the job so exiting the job handle kills all associated
	// processes. This is our tree-kill mechanism.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
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
	// Now that the process is in the job, resume it.
	if err := resumeMainThread(cmd); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("watchdog: resume: %w", err)
	}
	jobsMu.Lock()
	jobs[cmd.Process.Pid] = job
	jobsMu.Unlock()
	return nil
}

// killTree terminates the Job Object, taking every process in the tree
// with it. Kill=false attempts a soft close (Ctrl-Break to the group); the
// caller's escalation ladder decides when to escalate to Terminate.
func killTree(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	if !kill {
		// Best-effort soft signal via console control event.
		_ = sendCtrlBreak(cmd.Process.Pid)
		return nil
	}
	jobsMu.Lock()
	job, ok := jobs[cmd.Process.Pid]
	if ok {
		delete(jobs, cmd.Process.Pid)
	}
	jobsMu.Unlock()
	if !ok {
		return cmd.Process.Kill()
	}
	defer windows.CloseHandle(job)
	return windows.TerminateJobObject(job, 1)
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS from psapi.h.
// Not exported by golang.org/x/sys/windows so we declare it locally and
// call psapi.dll directly.
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
	psapi                   = windows.NewLazySystemDLL("psapi.dll")
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

// sampleCPUSeconds uses GetProcessTimes to return user + kernel time.
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
	// Filetime is in 100-nanosecond intervals.
	total := filetimeSeconds(kernel) + filetimeSeconds(user)
	return total, nil
}

func filetimeSeconds(ft windows.Filetime) float64 {
	// 100ns units → seconds. Combine high+low into a single int64.
	units := (int64(ft.HighDateTime) << 32) | int64(ft.LowDateTime)
	return float64(units) / 1e7
}

// resumeMainThread starts the primary thread of the suspended process.
// os/exec doesn't expose the thread handle, so we snapshot threads and
// resume the first one belonging to our pid.
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
			if err == nil {
				_, err = windows.ResumeThread(thread)
				windows.CloseHandle(thread)
				return err
			}
			return err
		}
		if err := windows.Thread32Next(snapshot, &te); err != nil {
			return fmt.Errorf("watchdog: no thread found for pid %d", cmd.Process.Pid)
		}
	}
}

// sendCtrlBreak posts a Ctrl+Break to the process group. The child must be
// in its own group (CREATE_NEW_PROCESS_GROUP) for this to be well-behaved.
func sendCtrlBreak(pid int) error {
	d, err := windows.LoadDLL("kernel32.dll")
	if err != nil {
		return err
	}
	defer d.Release()
	p, err := d.FindProc("GenerateConsoleCtrlEvent")
	if err != nil {
		return err
	}
	r, _, err := p.Call(uintptr(syscall.CTRL_BREAK_EVENT), uintptr(pid))
	if r == 0 {
		return err
	}
	return nil
}

// jobs maps pid → job handle, needed because os/exec doesn't hand us the
// job back after AssignProcessToJobObject.
var (
	jobsMu sync.Mutex
	jobs   = map[int]windows.Handle{}
)
