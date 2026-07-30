//go:build linux

package codexapp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverCgroup2Layout(t *testing.T) {
	mounts := "31 23 0:28 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n"
	layout, err := DiscoverCgroup2Layout([]byte(mounts), []byte("0::/shipmates/delegated\n"))
	if err != nil {
		t.Fatal(err)
	}
	if layout.Mountpoint != "/sys/fs/cgroup" || layout.Hierarchy != "/sys/fs/cgroup/shipmates/delegated" {
		t.Fatalf("layout=%+v", layout)
	}
	if _, err := DiscoverCgroup2Layout([]byte("1 2 0:1 / /proc rw - proc proc rw\n"), []byte("0::/\n")); err == nil {
		t.Fatal("missing cgroup2 mount accepted")
	}
}

func TestDiscoverCgroup2LayoutHonorsBindMountRoot(t *testing.T) {
	mounts := "31 23 0:28 /delegated /sandbox/cgroup rw,nosuid,nodev - cgroup2 cgroup rw\n"
	layout, err := DiscoverCgroup2Layout([]byte(mounts), []byte("0::/delegated/shipmates/probe\n"))
	if err != nil {
		t.Fatal(err)
	}
	if layout.MountRoot != "/delegated" || layout.Hierarchy != "/sandbox/cgroup/shipmates/probe" {
		t.Fatalf("layout=%+v", layout)
	}
	if _, err := cgroupPathAt("/sandbox/cgroup", "/delegated", "/outside"); err == nil {
		t.Fatal("hierarchy outside bind-mounted root accepted")
	}
}

func TestDelegatedRootRejectsUnsafeLocationsBeforeStatfs(t *testing.T) {
	layout := Cgroup2Layout{Mountpoint: "/sys/fs/cgroup", Hierarchy: "/sys/fs/cgroup/shipmates/delegated"}
	for _, root := range []string{"/sys/fs/cgroup", "/sys/fs", "/tmp/outside", "/sys/fs/cgroup/shipmates/../delegated"} {
		if _, err := OpenDelegatedCgroup(root, layout); err == nil {
			t.Fatalf("unsafe root accepted: %s", root)
		}
	}
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink("/tmp", link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDelegatedCgroup(link, Cgroup2Layout{Mountpoint: root, Hierarchy: root}); err == nil {
		t.Fatal("symlink delegated root accepted")
	}
}
