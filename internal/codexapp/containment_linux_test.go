//go:build linux

package codexapp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWriteLaunchSpecMatchesLauncherWireFormat(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchSpec(w, []string{"codex", "app-server"}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	b, err := os.ReadFile("/proc/self/fd/" + fmt.Sprint(r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if len(b) < 20 || !bytes.Equal(b[:15], []byte("SMCL-LAUNCH-V1\x00")) || b[15] != 0 {
		t.Fatalf("invalid launch header: %x", b)
	}
	if got := binary.LittleEndian.Uint32(b[16:20]); got != 2 {
		t.Fatalf("argument count = %d, want 2", got)
	}
}

func TestTrustedHelperRequiresCanonicalSafeIdentity(t *testing.T) {
	canonical, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skip("host has no /bin/sh")
	}
	st, ok := mustStatUID(canonical)
	if !ok || st != 0 {
		t.Skip("managed host helper is not root-owned")
	}
	if !trustedPreExecHelper(canonical) {
		t.Fatal("canonical root-owned executable was rejected")
	}
	if trustedPreExecHelper("bin/sh") {
		t.Fatal("relative helper was accepted")
	}
	if canonical != "/bin/sh" && trustedPreExecHelper("/bin/sh") {
		t.Fatal("symlink helper was accepted")
	}
	if h, err := openTrustedHelper(canonical); err != nil {
		t.Fatal(err)
	} else if h.file == nil || h.inode == 0 || h.device == 0 {
		t.Fatal("opened helper did not retain identity")
	} else if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustStatUID(path string) (uint32, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func TestTrustedHelperRejectsPathSwapAndUnsafeParent(t *testing.T) {
	if trustedPreExecHelper("/tmp/../bin/sh") {
		t.Fatal("non-canonical path accepted")
	}
	if trustedPreExecHelper(filepath.Join(t.TempDir(), "missing", "helper")) {
		t.Fatal("missing or unsafe parent accepted")
	}
}

func TestStartContainedAtUsesLivePidfdAndFixtureCgroup(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu memory\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	c, err := StartContainedAt(root, cmd, "exe_fixture")
	if err != nil {
		t.Fatal(err)
	}
	if c.fd <= 0 || c.pid == 0 {
		t.Fatalf("missing live process identity: fd=%d pid=%d", c.fd, c.pid)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.path, "cgroup.events"), []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	empty, err := c.Empty()
	if err != nil || !empty {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// StartContained must fail closed where the kernel prerequisites are missing.
// The catch is that this asserts a path that only exists on such a host: a
// privileged caller on a machine with a writable cgroup2 root — a root shell
// inside WSL, for one — really can contain a child, and there is nothing to
// assert there.
//
// The previous version asserted unconditionally and was self-poisoning. On a
// host where containment worked it failed, leaking /sys/fs/cgroup/shipmates-<id>
// because it never closed the containment it did not expect to get. Every later
// run then hit EEXIST creating that same fixed-name directory, failed to
// contain, and "passed" — green because of the debris from its own earlier
// failure rather than because of anything about the kernel. A unique execution ID
// makes that impossible, and an unexpected success is now torn down and reported
// as a skip.
func TestStartContainedFailsClosedWithoutKernelPrerequisites(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	executionID := fmt.Sprintf("exe_unavailable_%d", time.Now().UnixNano())
	c, err := StartContained(cmd, executionID)
	if err != nil {
		return
	}
	_ = cmd.Wait()
	if closeErr := c.Close(); closeErr != nil {
		t.Errorf("cleanup of an unexpectedly successful containment: %v", closeErr)
	}
	t.Skip("host has a usable cgroup2 delegation, so the fail-closed path is unreachable here")
}
