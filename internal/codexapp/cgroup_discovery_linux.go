//go:build linux

package codexapp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const cgroup2SuperMagic = 0x63677270

type Cgroup2Layout struct {
	Mountpoint string
	// MountRoot is the cgroupfs root field from mountinfo.  A cgroup2 mount
	// can be a bind mount of a non-root hierarchy; mapping /proc/*/cgroup
	// entries through it must account for that prefix.
	MountRoot string
	Hierarchy string
}

func DiscoverCgroup2Layout(mountInfo, selfCgroup []byte) (Cgroup2Layout, error) {
	mountpoint, mountRoot, err := parseCgroup2Mount(string(mountInfo))
	if err != nil {
		return Cgroup2Layout{}, err
	}
	hierarchy, err := parseUnifiedHierarchy(string(selfCgroup))
	if err != nil {
		return Cgroup2Layout{}, err
	}
	if !filepath.IsAbs(mountpoint) || !filepath.IsAbs(mountRoot) || !filepath.IsAbs(hierarchy) {
		return Cgroup2Layout{}, errors.New("cgroup layout is not absolute")
	}
	mountpoint, mountRoot, hierarchy = filepath.Clean(mountpoint), filepath.Clean(mountRoot), filepath.Clean(hierarchy)
	location, err := cgroupPathAt(mountpoint, mountRoot, hierarchy)
	if err != nil {
		return Cgroup2Layout{}, err
	}
	return Cgroup2Layout{Mountpoint: mountpoint, MountRoot: mountRoot, Hierarchy: location}, nil
}

func parseCgroup2Mountpoint(input string) (string, error) {
	mount, _, err := parseCgroup2Mount(input)
	return mount, err
}

func parseCgroup2Mount(input string) (mountpoint, mountRoot string, err error) {
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 5 || separator+1 >= len(fields) || fields[separator+1] != "cgroup2" {
			continue
		}
		return decodeMountField(fields[4]), decodeMountField(fields[3]), nil
	}
	return "", "", errors.New("active cgroup2 mount not found")
}

// cgroupPathAt translates a unified-hierarchy path (which is relative to the
// cgroup namespace root) to the visible cgroupfs mount.  It rejects an entry
// that is outside a bind-mounted cgroup root instead of silently comparing a
// different hierarchy with the same leaf name.
func cgroupPathAt(mountpoint, mountRoot, hierarchy string) (string, error) {
	mountpoint, mountRoot, hierarchy = filepath.Clean(mountpoint), filepath.Clean(mountRoot), filepath.Clean(hierarchy)
	if !filepath.IsAbs(mountpoint) || !filepath.IsAbs(mountRoot) || !filepath.IsAbs(hierarchy) {
		return "", errors.New("cgroup path is not absolute")
	}
	rel, err := filepath.Rel(mountRoot, hierarchy)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("unified hierarchy is outside cgroup mount root")
	}
	if rel == "." {
		return mountpoint, nil
	}
	return filepath.Join(mountpoint, rel), nil
}

func parseUnifiedHierarchy(input string) (string, error) {
	for _, line := range strings.Split(input, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" && filepath.IsAbs(fields[2]) {
			return filepath.Clean(fields[2]), nil
		}
	}
	return "", errors.New("unified cgroup hierarchy not found")
}

func decodeMountField(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`).Replace(s)
}

// OpenDelegatedCgroup validates the explicit root without following any path
// component. The returned descriptor is the only authority used by probes.
func OpenDelegatedCgroup(root string, layout Cgroup2Layout) (*os.File, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("cgroup root must be absolute and clean")
	}
	root = filepath.Clean(root)
	mount := filepath.Clean(layout.Mountpoint)
	hierarchy := filepath.Clean(layout.Hierarchy)
	if root == mount || root == "/" {
		return nil, errors.New("cgroup root is mount root")
	}
	if !within(root, mount) || !within(root, hierarchy) {
		return nil, errors.New("cgroup root is outside delegated hierarchy")
	}
	if root != hierarchy && within(hierarchy, root) {
		return nil, errors.New("cgroup root is an ancestor of current hierarchy")
	}
	fd, err := openDirBeneath(mount, root)
	if err != nil {
		return nil, err
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(fd.Fd()), &stat); err != nil || uint64(stat.Type) != cgroup2SuperMagic {
		_ = fd.Close()
		return nil, errors.New("delegated root is not cgroup2")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(fd.Fd()), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || st.Mode&0022 != 0 {
		_ = fd.Close()
		return nil, errors.New("delegated root has unsafe ownership or mode")
	}
	return fd, nil
}

func within(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func openDirBeneath(base, target string) (*os.File, error) {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, errors.New("unsafe delegated path")
	}
	fd, err := unix.Open(base, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			_ = unix.Close(fd)
			return nil, errors.New("unsafe delegated path")
		}
		next, openErr := unix.Openat(fd, part, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return nil, openErr
		}
		_ = unix.Close(fd)
		fd = next
	}
	file := os.NewFile(uintptr(fd), target)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("delegated descriptor unavailable")
	}
	return file, nil
}

func (l Cgroup2Layout) String() string {
	return fmt.Sprintf("mount=%s hierarchy=%s", l.Mountpoint, l.Hierarchy)
}
