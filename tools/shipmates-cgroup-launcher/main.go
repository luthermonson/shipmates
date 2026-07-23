//go:build linux

// shipmates-cgroup-launcher is an intentionally closed pre-exec helper. It
// accepts descriptors, never a command string, and is intended to be
// installed by an administrator as /usr/libexec/shipmates/shipmates-cgroup-launcher.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	version = "shipmates-cgroup-launcher-v1"
	magic   = "SMCL-LAUNCH-V1\x00"
	maxArg  = 4096
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Every error returned by run is a fixed diagnostic code. Never include
		// paths, arguments, environment values, or wrapped system errors here.
		fmt.Fprintf(os.Stderr, "shipmates-cgroup-launcher: %s\n", err)
		os.Exit(126)
	}
}

func run(args []string) error {
	flags, err := parseFlags(args)
	if err != nil || flags["version"] != version {
		return errors.New("invalid_flags")
	}
	cgroupFD, err := descriptor(flags, "cgroup-fd")
	if err != nil || !directoryFD(cgroupFD) {
		return errors.New("invalid_cgroup_fd")
	}
	readyFD, err := descriptor(flags, "ready-fd")
	if err != nil || !pipeFD(readyFD, true) {
		return errors.New("invalid_ready_fd")
	}
	gateFD, err := descriptor(flags, "gate-fd")
	if err != nil || !pipeFD(gateFD, false) {
		return errors.New("invalid_gate_fd")
	}
	launchFD, err := descriptor(flags, "launch-fd")
	if err != nil {
		return errors.New("invalid_launch_fd")
	}
	targetFD, err := descriptor(flags, "target-fd")
	if err != nil || !elfFD(targetFD) {
		return errors.New("invalid_target_fd")
	}
	argv, err := readLaunchSpec(launchFD)
	if err != nil || len(argv) == 0 {
		return errors.New("invalid_launch_spec")
	}
	if err := joinAndVerify(cgroupFD); err != nil {
		return errors.New("cgroup_join_failed")
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(cgroupFD, &st); err != nil {
		return errors.New("cgroup_stat_failed")
	}
	ready := os.NewFile(uintptr(readyFD), "ready")
	if ready == nil {
		return errors.New("ready_unavailable")
	}
	if _, err := fmt.Fprintf(ready, "SHIPMATES_CGROUP_READY_V1 %d %d\n", st.Dev, st.Ino); err != nil {
		return errors.New("ready_write_failed")
	}
	_ = ready.Close()
	gate := bufio.NewReader(os.NewFile(uintptr(gateFD), "gate"))
	line, err := gate.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "go" {
		return errors.New("release_failed")
	}
	// The target is an already-open executable identity. /proc/self/fd is the
	// execveat(AT_EMPTY_PATH) equivalent available without a cgo dependency.
	path := "/proc/self/fd/" + strconv.Itoa(targetFD)
	return syscall.Exec(path, argv, os.Environ())
}

func parseFlags(args []string) (map[string]string, error) {
	allowed := map[string]bool{"cgroup-fd": true, "ready-fd": true, "gate-fd": true, "launch-fd": true, "target-fd": true, "version": true}
	out := make(map[string]string)
	for _, arg := range args {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "--") || !allowed[parts[0][2:]] || out[parts[0][2:]] != "" {
			return nil, errors.New("unknown launcher argument")
		}
		out[parts[0][2:]] = parts[1]
	}
	if len(out) != len(allowed) {
		return nil, errors.New("incomplete launcher arguments")
	}
	return out, nil
}

func descriptor(flags map[string]string, name string) (int, error) {
	n, err := strconv.Atoi(flags[name])
	if err != nil || n < 3 || n > 1024 {
		return -1, errors.New("invalid descriptor")
	}
	return n, nil
}

func directoryFD(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR && (st.Uid == 0 || st.Uid == uint32(os.Geteuid())) && st.Mode&0022 == 0
}

func pipeFD(fd int, writable bool) bool {
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return false
	}
	if st.Mode&syscall.S_IFMT == syscall.S_IFSOCK || st.Mode&syscall.S_IFMT == syscall.S_IFIFO {
		return true
	}
	_ = writable
	return false
}

func elfFD(fd int) bool {
	f := os.NewFile(uintptr(fd), "target")
	if f == nil {
		return false
	}
	var header [20]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return false
	}
	return string(header[:4]) == "\x7fELF" && header[4] == 2 && header[5] == 1 && header[6] == 1
}

func readLaunchSpec(fd int) ([]string, error) {
	f := os.NewFile(uintptr(fd), "launch")
	if f == nil {
		return nil, errors.New("missing launch descriptor")
	}
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[:len(magic)]) != magic || hdr[15] != 0 {
		return nil, errors.New("bad launch magic")
	}
	count := binary.LittleEndian.Uint32(hdr[16:20])
	if count == 0 || count > maxArg {
		return nil, errors.New("bad launch count")
	}
	argv := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		var nbuf [4]byte
		if _, err := io.ReadFull(f, nbuf[:]); err != nil {
			return nil, errors.New("truncated launch spec")
		}
		n := binary.LittleEndian.Uint32(nbuf[:])
		if n == 0 || n > maxArg {
			return nil, errors.New("bad launch argument")
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(f, b); err != nil || strings.IndexByte(string(b), 0) >= 0 {
			return nil, errors.New("invalid launch argument")
		}
		argv = append(argv, string(b))
	}
	return argv, nil
}

func joinAndVerify(fd int) error {
	procs, err := syscall.Openat(fd, "cgroup.procs", syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("cgroup_join_failed")
	}
	_, writeErr := syscall.Write(procs, []byte(fmt.Sprintf("%d\n", os.Getpid())))
	_ = syscall.Close(procs)
	if writeErr != nil {
		return errors.New("cgroup_join_failed")
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return errors.New("cgroup_identity_unavailable")
	}
	link = filepath.Clean(link)
	if !filepath.IsAbs(link) {
		return errors.New("cgroup_identity_unavailable")
	}
	mountpoint, mountRoot, err := cgroup2Mount()
	if err != nil {
		return errors.New("cgroup_identity_unavailable")
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("cgroup_membership_unavailable")
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" && parts[1] == "" {
			actual, mapErr := cgroupPath(mountpoint, mountRoot, parts[2])
			if mapErr == nil && actual == link {
				return nil
			}
		}
	}
	return errors.New("cgroup_membership_mismatch")
}

func cgroup2Mount() (string, string, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator >= 5 && separator+1 < len(fields) && fields[separator+1] == "cgroup2" {
			return decodeMountField(fields[4]), decodeMountField(fields[3]), nil
		}
	}
	return "", "", errors.New("cgroup2_mount_missing")
}

func cgroupPath(mountpoint, mountRoot, hierarchy string) (string, error) {
	mountpoint, mountRoot, hierarchy = filepath.Clean(mountpoint), filepath.Clean(mountRoot), filepath.Clean(hierarchy)
	if !filepath.IsAbs(mountpoint) || !filepath.IsAbs(mountRoot) || !filepath.IsAbs(hierarchy) {
		return "", errors.New("cgroup_path_invalid")
	}
	rel, err := filepath.Rel(mountRoot, hierarchy)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("cgroup_path_outside_mount")
	}
	if rel == "." {
		return mountpoint, nil
	}
	return filepath.Join(mountpoint, rel), nil
}

func decodeMountField(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(s)
}
