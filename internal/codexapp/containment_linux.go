//go:build linux

package codexapp

// This file is deliberately additive. The existing adapter remains the
// protocol owner; this narrow seam owns only execution-scoped child cleanup.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const defaultCgroupRoot = "/sys/fs/cgroup"

type ExecutionCapabilities struct {
	Pidfd                bool
	DelegatedCgroupV2    bool
	CgroupKill           bool
	PopulatedZero        bool
	TrustedPreExecHelper bool
}

func (c ExecutionCapabilities) AssessmentEnabled() bool {
	return c.Pidfd && c.DelegatedCgroupV2 && c.CgroupKill && c.PopulatedZero && c.TrustedPreExecHelper
}

func DetectExecutionCapabilities(helper string) ExecutionCapabilities {
	return DetectExecutionCapabilitiesAt(defaultCgroupRoot, effectivePreExecHelper(helper))
}

// DetectExecutionCapabilitiesAt is the testable equivalent of the production
// probe. It reports enabled only after exercising a child cgroup in root.
func DetectExecutionCapabilitiesAt(root, helper string) ExecutionCapabilities {
	var c ExecutionCapabilities
	if fd, err := unix.PidfdOpen(os.Getpid(), 0); err == nil {
		c.Pidfd = true
		_ = unix.Close(fd)
	}
	if !probeDelegatedCgroup(root) {
		return c
	}
	c.DelegatedCgroupV2 = true
	c.CgroupKill, c.PopulatedZero = true, true
	c.TrustedPreExecHelper = trustedPreExecHelper(helper)
	return c
}

func trustedPreExecHelper(path string) bool {
	h, err := openTrustedHelper(path)
	if err != nil {
		return false
	}
	return h.Close() == nil
}

type helperIdentity struct {
	file   *os.File
	device uint64
	inode  uint64
	digest [32]byte
}

func openTrustedHelper(configured string) (*helperIdentity, error) {
	if configured == "" || !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
		return nil, errors.New("helper must be absolute and canonical")
	}
	canonical, err := filepath.EvalSymlinks(configured)
	if err != nil || !filepath.IsAbs(canonical) || canonical != configured {
		return nil, errors.New("helper path is not canonical")
	}
	parts := strings.Split(strings.TrimPrefix(canonical, "/"), "/")
	if len(parts) < 2 {
		return nil, errors.New("invalid helper path")
	}
	dirfd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("unsafe helper parent")
		}
		next, openErr := unix.Openat(dirfd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, openErr
		}
		if err := validateTrustedFD(next, true); err != nil {
			_ = unix.Close(next)
			return nil, err
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	finalfd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := validateTrustedFD(finalfd, false); err != nil {
		_ = unix.Close(finalfd)
		return nil, err
	}
	f := os.NewFile(uintptr(finalfd), canonical)
	if f == nil {
		_ = unix.Close(finalfd)
		return nil, errors.New("helper fd unavailable")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	var st unix.Stat_t
	if err := unix.Fstat(finalfd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &helperIdentity{file: f, device: uint64(st.Dev), inode: uint64(st.Ino), digest: digest}, nil
}

func validateTrustedFD(fd int, directory bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	mode := st.Mode
	if directory {
		if mode&syscall.S_IFMT != syscall.S_IFDIR {
			return errors.New("helper parent is not a directory")
		}
	} else if mode&syscall.S_IFMT != syscall.S_IFREG || mode&0111 == 0 {
		return errors.New("helper is not a regular executable")
	}
	if st.Uid != 0 || mode&0022 != 0 {
		return errors.New("helper ownership or mode is unsafe")
	}
	return nil
}

func (h *helperIdentity) Close() error {
	if h == nil || h.file == nil {
		return nil
	}
	err := h.file.Close()
	h.file = nil
	return err
}

func (h *helperIdentity) verify() error {
	if h == nil || h.file == nil {
		return errors.New("helper identity unavailable")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(h.file.Fd()), &st); err != nil || uint64(st.Dev) != h.device || uint64(st.Ino) != h.inode {
		return errors.New("helper identity changed")
	}
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, h.file); err != nil {
		return err
	}
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if !bytes.Equal(hash.Sum(nil), h.digest[:]) {
		return errors.New("helper digest changed")
	}
	return nil
}

func openExecutableTarget(path string) (*os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("target path is not canonical")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, errors.New("target path changed")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0111 == 0 {
		_ = unix.Close(fd)
		return nil, errors.New("target is not an executable file")
	}
	return os.NewFile(uintptr(fd), path), nil
}

func writeLaunchSpec(w *os.File, args []string) error {
	if w == nil || len(args) == 0 || len(args) > 4096 {
		return errors.New("invalid launch specification")
	}
	var b bytes.Buffer
	b.WriteString("SMCL-LAUNCH-V1\x00")
	// Byte 15 is reserved so the argument count begins at the protocol's
	// fixed 16-byte boundary consumed by the privileged launcher.
	b.WriteByte(0)
	var n [4]byte
	binary.LittleEndian.PutUint32(n[:], uint32(len(args)))
	b.Write(n[:])
	for _, arg := range args {
		if arg == "" || len(arg) > 4096 || strings.IndexByte(arg, 0) >= 0 {
			return errors.New("invalid launch argument")
		}
		binary.LittleEndian.PutUint32(n[:], uint32(len(arg)))
		b.Write(n[:])
		b.WriteString(arg)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func writeCgroup(path, name, value string) bool {
	fd, err := unix.Open(filepath.Join(path, name), unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	_, err = unix.Write(fd, []byte(value))
	return err == nil
}

func processInCgroup(pid int, path string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return false
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	mountpoint, mountRoot, err := parseCgroup2Mount(string(mountInfo))
	if err != nil {
		return false
	}
	// A delegated root supplied by the qualifier is held as a directory FD.
	// Its visible path is /proc/self/fd/N; resolve that stable live descriptor
	// before comparing it with the kernel's mount-relative cgroup identity.
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	want = filepath.Clean(want)
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			actual, pathErr := cgroupPathAt(mountpoint, mountRoot, parts[2])
			return pathErr == nil && actual == want
		}
	}
	return false
}

// probeDelegatedCgroup exercises only a newly-created child cgroup and a
// child process created for this probe. It never writes cgroup.kill in the
// delegated root and never uses unrelated work as evidence.
func probeDelegatedCgroup(root string) bool {
	if filepath.IsAbs(root) == false || filepath.Clean(root) != root {
		return false
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if b, err := os.ReadFile(filepath.Join(root, "cgroup.controllers")); err != nil || strings.TrimSpace(string(b)) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.procs")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.kill")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.events")); err != nil {
		return false
	}
	name := fmt.Sprintf("shipmates-probe-%d", time.Now().UnixNano())
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0700); err != nil {
		return false
	}
	defer os.Remove(path)
	if !safeCgroupDir(path) {
		return false
	}
	child := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := child.Start(); err != nil {
		return false
	}
	fd, err := unix.PidfdOpen(child.Process.Pid, 0)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return false
	}
	joined := writeCgroup(path, "cgroup.procs", fmt.Sprintf("%d\n", child.Process.Pid))
	killed := joined && writeCgroup(path, "cgroup.kill", "1\n")
	_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
	_ = unix.Close(fd)
	_ = child.Wait()
	empty := waitCgroupEmpty(path, 500*time.Millisecond)
	return joined && killed && empty
}

func safeCgroupDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

func waitCgroupEmpty(path string, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cgroupPopulatedZero(path) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func cgroupPopulatedZero(path string) bool {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0"
		}
	}
	return false
}

type ExecutionContainment struct {
	path          string
	pid           int
	fd            int
	cmd           *exec.Cmd
	helper        *helperIdentity
	cgroup        *os.File
	delegatedRoot *os.File
}

func (c *ExecutionContainment) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

func (c *ExecutionContainment) PID() int {
	if c == nil {
		return 0
	}
	return c.pid
}

// StartContained requires cgroup v2 and pidfd support. It fails closed when
// either prerequisite is unavailable and never reconstructs ownership from a
// persisted PID. The caller must retain the returned live handle.
func StartContained(cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	return StartContainedAt(defaultCgroupRoot, cmd, executionID)
}

func StartContainedWithHelper(helper string, cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	return StartContainedWithHelperAt(defaultCgroupRoot, helper, cmd, executionID)
}

// StartContainedWithHelperCurrent discovers the caller's unified hierarchy
// and retains a no-follow descriptor for that delegated scope throughout the
// child lifetime. An unprivileged caller never writes beneath the global
// cgroup mount root.
func StartContainedWithHelperCurrent(helper string, cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, errors.New("cgroup mount discovery unavailable")
	}
	selfCgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, errors.New("current cgroup discovery unavailable")
	}
	layout, err := DiscoverCgroup2Layout(mountInfo, selfCgroup)
	if err != nil {
		return nil, err
	}
	root, err := OpenDelegatedCgroup(layout.Hierarchy, layout)
	if err != nil {
		return nil, fmt.Errorf("current cgroup is not a delegated scope: %w", err)
	}
	c, err := StartContainedWithHelperInDir(root, helper, cmd, executionID)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	c.delegatedRoot = root
	return c, nil
}

func StartContainedWithHelperAt(root, helper string, cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	h, err := openTrustedHelper(helper)
	if err != nil {
		return nil, errors.New("trusted pre-exec helper unavailable")
	}
	c, err := startContainedAt(root, "", cmd, executionID, h, nil)
	if err != nil {
		_ = h.Close()
	}
	return c, err
}

// StartContainedWithHelperInDir starts beneath an already validated delegated
// cgroup directory.  The qualifier deliberately keeps this FD open from its
// no-follow validation through launch; accepting a pathname here would reopen
// a replacement race between validation and disposable-child creation.
func StartContainedWithHelperInDir(root *os.File, helper string, cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	if root == nil || root.Fd() < 0 {
		return nil, errors.New("delegated cgroup descriptor unavailable")
	}
	h, err := openTrustedHelper(helper)
	if err != nil {
		return nil, errors.New("trusted pre-exec helper unavailable")
	}
	path := fmt.Sprintf("/proc/self/fd/%d", root.Fd())
	c, err := startContainedAt(path, "", cmd, executionID, h, root)
	if err != nil {
		_ = h.Close()
	}
	return c, err
}

// StartContainedAt is the deterministic fixture seam. Production callers use
// StartContained, pinned to the kernel cgroup root.
func StartContainedAt(root string, cmd *exec.Cmd, executionID string) (*ExecutionContainment, error) {
	return startContainedAt(root, "", cmd, executionID, nil, nil)
}

func startContainedAt(root, helper string, cmd *exec.Cmd, executionID string, helperID *helperIdentity, rootFD *os.File) (*ExecutionContainment, error) {
	if cmd == nil || executionID == "" || strings.ContainsAny(executionID, "/\\") {
		return nil, errors.New("invalid contained execution")
	}
	if root == "" || filepath.Clean(root) != root {
		return nil, errors.New("invalid cgroup root")
	}
	if rootFD == nil {
		rootStat, err := os.Lstat(root)
		if err != nil || !rootStat.IsDir() || rootStat.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe cgroup root")
		}
	} else {
		var st unix.Stat_t
		if err := unix.Fstat(int(rootFD.Fd()), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || st.Mode&0022 != 0 {
			return nil, errors.New("unsafe delegated cgroup descriptor")
		}
	}
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil || len(strings.TrimSpace(string(controllers))) == 0 {
		return nil, errors.New("cgroup v2 unavailable")
	}
	path := filepath.Join(root, "shipmates-"+executionID)
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, fmt.Errorf("execution cgroup unavailable: %w", err)
	}
	clean := func() { _ = os.Remove(path) }
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		clean()
		return nil, errors.New("unsafe execution cgroup")
	}
	// The socket-free fixture uses ordinary files in place of cgroupfs
	// controls. Real cgroupfs creates cgroup.procs itself.
	if root != defaultCgroupRoot {
		for _, name := range []string{"cgroup.procs", "cgroup.kill"} {
			if _, err := os.Stat(filepath.Join(path, name)); errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(filepath.Join(path, name), nil, 0600); err != nil {
					clean()
					return nil, err
				}
			}
		}
	}
	var readyR, readyW, gateR, gateW *os.File
	var launchR, launchW *os.File
	var targetFile *os.File
	var cgroupFile *os.File
	if helperID != nil {
		if err := helperID.verify(); err != nil {
			clean()
			return nil, err
		}
		var pipeErr error
		readyR, readyW, pipeErr = os.Pipe()
		if pipeErr != nil {
			clean()
			return nil, pipeErr
		}
		gateR, gateW, pipeErr = os.Pipe()
		if pipeErr != nil {
			_ = readyR.Close()
			_ = readyW.Close()
			clean()
			return nil, pipeErr
		}
		target, args := cmd.Path, append([]string(nil), cmd.Args...)
		var openErr error
		targetFile, openErr = openExecutableTarget(target)
		if openErr != nil {
			_ = readyR.Close()
			_ = readyW.Close()
			_ = gateR.Close()
			_ = gateW.Close()
			clean()
			return nil, openErr
		}
		launchR, launchW, openErr = os.Pipe()
		if openErr != nil {
			_ = targetFile.Close()
			clean()
			return nil, openErr
		}
		if err := writeLaunchSpec(launchW, args); err != nil {
			_ = launchR.Close()
			_ = launchW.Close()
			_ = targetFile.Close()
			clean()
			return nil, err
		}
		_ = launchW.Close()
		cgroupFile, openErr = os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = launchR.Close()
			_ = targetFile.Close()
			clean()
			return nil, openErr
		}
		base := len(cmd.ExtraFiles)
		cgroupFD, readyFD, gateFD, helperFD := 3+base, 4+base, 5+base, 6+base
		launchFD, targetFD := 7+base, 8+base
		helperPath := fmt.Sprintf("/proc/self/fd/%d", helperFD)
		cmd.Path = helperPath
		cmd.Args = []string{helperPath, fmt.Sprintf("--cgroup-fd=%d", cgroupFD), fmt.Sprintf("--ready-fd=%d", readyFD), fmt.Sprintf("--gate-fd=%d", gateFD), fmt.Sprintf("--launch-fd=%d", launchFD), fmt.Sprintf("--target-fd=%d", targetFD), "--version=shipmates-cgroup-launcher-v1"}
		cmd.ExtraFiles = append(cmd.ExtraFiles, cgroupFile, readyW, gateR, helperID.file, launchR, targetFile)
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, "SHIPMATES_EXECUTION_CGROUP="+path)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		if readyR != nil {
			_ = readyR.Close()
			_ = readyW.Close()
			_ = gateR.Close()
			_ = gateW.Close()
		}
		if launchR != nil {
			_ = launchR.Close()
		}
		if targetFile != nil {
			_ = targetFile.Close()
		}
		if cgroupFile != nil {
			_ = cgroupFile.Close()
		}
		clean()
		return nil, err
	}
	if readyR != nil {
		_ = readyW.Close()
		_ = gateR.Close()
		_ = launchR.Close()
		_ = targetFile.Close()
	}
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		clean()
		return nil, errors.New("pidfd unavailable")
	}
	if !writeCgroup(path, "cgroup.procs", fmt.Sprintf("%d\n", cmd.Process.Pid)) {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = cmd.Wait()
		_ = unix.Close(fd)
		clean()
		return nil, errors.New("execution containment unavailable")
	}
	if readyR != nil {
		ready := make(chan []byte, 1)
		go func() { b, _ := io.ReadAll(readyR); ready <- b }()
		select {
		case b := <-ready:
			identity, identityErr := cgroupFile.Stat()
			expected := ""
			if identityErr == nil {
				if st, ok := identity.Sys().(*syscall.Stat_t); ok {
					expected = fmt.Sprintf("SHIPMATES_CGROUP_READY_V1 %d %d", st.Dev, st.Ino)
				}
			}
			readyMatches := bytes.Equal(bytes.TrimSpace(b), []byte(expected))
			membershipMatches := processInCgroup(cmd.Process.Pid, path)
			if !readyMatches || !membershipMatches {
				_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				_ = cmd.Wait()
				_ = unix.Close(fd)
				_ = gateW.Close()
				_ = readyR.Close()
				clean()
				if !readyMatches {
					return nil, errors.New("helper readiness handshake failed")
				}
				return nil, errors.New("helper cgroup membership handshake failed")
			}
		case <-time.After(2 * time.Second):
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			_ = cmd.Wait()
			_ = unix.Close(fd)
			_ = gateW.Close()
			_ = readyR.Close()
			clean()
			return nil, errors.New("helper placement handshake timeout")
		}
		_ = readyR.Close()
		if _, err := gateW.Write([]byte("go\n")); err != nil {
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			_ = cmd.Wait()
			_ = unix.Close(fd)
			_ = gateW.Close()
			clean()
			return nil, errors.New("helper release handshake failed")
		}
		_ = gateW.Close()
	}
	return &ExecutionContainment{path: path, pid: cmd.Process.Pid, fd: fd, cmd: cmd, helper: helperID, cgroup: cgroupFile}, nil
}

func (c *ExecutionContainment) GracefulTerminate(ctx context.Context) error {
	if c == nil || c.fd <= 0 {
		return errors.New("invalid containment handle")
	}
	if err := unix.PidfdSendSignal(c.fd, unix.SIGINT, nil, 0); err != nil {
		return err
	}
	return c.wait(ctx)
}

func (c *ExecutionContainment) ForceKill(ctx context.Context) error {
	if c == nil || c.fd <= 0 {
		return errors.New("invalid containment handle")
	}
	// cgroup.kill reaches descendants; pidfd remains the identity-safe direct
	// process signal path and is never reconstructed from persisted state.
	if !writeCgroup(c.path, "cgroup.kill", "1\n") {
		return errors.New("cgroup kill unavailable")
	}
	return c.wait(ctx)
}

func (c *ExecutionContainment) killCgroup() error {
	if c == nil {
		return errors.New("invalid containment handle")
	}
	if !writeCgroup(c.path, "cgroup.kill", "1\n") {
		return errors.New("cgroup kill unavailable")
	}
	return nil
}

func (c *ExecutionContainment) wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ExecutionContainment) Empty() (bool, error) {
	if c == nil {
		return false, errors.New("invalid containment handle")
	}
	b, err := os.ReadFile(filepath.Join(c.path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "populated" {
			return fields[1] == "0", nil
		}
	}
	return false, errors.New("cgroup populated state unavailable")
}

func (c *ExecutionContainment) Close() error {
	if c == nil {
		return nil
	}
	var closeErr error
	if c.fd > 0 {
		closeErr = unix.Close(c.fd)
		c.fd = 0
	}
	if c.helper != nil {
		if err := c.helper.Close(); closeErr == nil {
			closeErr = err
		}
	}
	defer func() {
		if c.delegatedRoot != nil {
			_ = c.delegatedRoot.Close()
			c.delegatedRoot = nil
		}
	}()
	empty, err := c.Empty()
	if err != nil || !empty {
		if closeErr != nil {
			return closeErr
		}
		return errors.New("execution descendants remain")
	}
	if err := os.Remove(c.path); err == nil {
		return closeErr
	}
	// The socket-free fixture represents cgroup pseudo-files as ordinary
	// files. Real cgroupfs removes them as part of rmdir; only clean regular
	// fixture entries before retrying, and only after populated=0.
	if entries, readErr := os.ReadDir(c.path); readErr == nil {
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				_ = os.Remove(filepath.Join(c.path, entry.Name()))
			}
		}
	}
	if err := os.Remove(c.path); closeErr == nil {
		closeErr = err
	}
	return closeErr
}
