//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestLauncherRejectsGenericOrMalformedArguments(t *testing.T) {
	if _, err := parseFlags([]string{"--version=" + version, "--command=sh"}); err == nil {
		t.Fatal("generic command argument accepted")
	}
	if _, err := parseFlags([]string{"--version=" + version}); err == nil {
		t.Fatal("incomplete descriptor set accepted")
	}
	if _, err := descriptor(map[string]string{"x": "2"}, "x"); err == nil {
		t.Fatal("unsafe descriptor accepted")
	}
}

func TestLauncherReadsClosedLaunchSpec(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "launch-spec-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var header [20]byte
	copy(header[:], magic)
	binary.LittleEndian.PutUint32(header[16:], 3)
	if _, err := f.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"codex", "app-server", "--stdio"} {
		binary.LittleEndian.PutUint32(header[:4], uint32(len(arg)))
		if _, err := f.Write(header[:4]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(arg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	argv, err := readLaunchSpec(int(f.Fd()))
	if err != nil || len(argv) != 3 || argv[2] != "--stdio" {
		t.Fatalf("argv=%v err=%v", argv, err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("not-launch"), 0600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.Open(bad)
	defer b.Close()
	if _, err := readLaunchSpec(int(b.Fd())); err == nil {
		t.Fatal("malformed launch spec accepted")
	}
}

func TestLauncherReadsLaunchSpecFromPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer w.Close()
		var header [20]byte
		copy(header[:], magic)
		binary.LittleEndian.PutUint32(header[16:], 1)
		_, _ = w.Write(header[:])
		binary.LittleEndian.PutUint32(header[:4], 5)
		_, _ = w.Write(header[:4])
		_, _ = w.Write([]byte("codex"))
	}()
	got, err := readLaunchSpec(int(r.Fd()))
	_ = r.Close()
	if err != nil || len(got) != 1 || got[0] != "codex" {
		t.Fatalf("readLaunchSpec() = %q, %v", got, err)
	}
}

func TestLauncherRequiresELFTargetAndFixedVersion(t *testing.T) {
	if version != "shipmates-cgroup-launcher-v1" {
		t.Fatal("launcher version changed")
	}
	f, err := os.Open("/bin/sh")
	if err != nil {
		t.Skip("host has no /bin/sh")
	}
	defer f.Close()
	if !elfFD(int(f.Fd())) {
		t.Fatal("known ELF target rejected")
	}
	bad, err := os.CreateTemp(t.TempDir(), "script-")
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := bad.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	if elfFD(int(bad.Fd())) {
		t.Fatal("script accepted as ELF")
	}
}

func TestLauncherMapsFullCgroupIdentity(t *testing.T) {
	got, err := cgroupPath("/sys/fs/cgroup", "/", "/shipmates/delegated/probe")
	if err != nil || got != "/sys/fs/cgroup/shipmates/delegated/probe" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = cgroupPath("/sandbox/cgroup", "/delegated", "/delegated/shipmates/probe")
	if err != nil || got != "/sandbox/cgroup/shipmates/probe" {
		t.Fatalf("bind mapping got %q err %v", got, err)
	}
	if _, err := cgroupPath("/sandbox/cgroup", "/delegated", "/another-tree/probe"); err == nil {
		t.Fatal("outside hierarchy accepted")
	}
}
