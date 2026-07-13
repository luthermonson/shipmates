//go:build unix

package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func readCanonicalPersonaInventory(root, wanted string) ([]InstalledPersona, error) {
	rfd, e := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory root")
	}
	defer unix.Close(rfd)
	cfd, e := unix.Openat(rfd, ".codex", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory directory")
	}
	defer unix.Close(cfd)
	afd, e := unix.Openat(cfd, "agents", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(e, unix.ENOENT) {
		return nil, nil
	}
	if e != nil {
		return nil, fmt.Errorf("unsafe Codex persona inventory directory")
	}
	defer unix.Close(afd)
	dup, e := unix.Dup(afd)
	if e != nil {
		return nil, e
	}
	dir := os.NewFile(uintptr(dup), "agents")
	entries, e := dir.ReadDir(MaxInstalledPersonas + 1)
	_ = dir.Close()
	if e != nil && e != io.EOF {
		return nil, fmt.Errorf("read Codex persona inventory: %w", e)
	}
	if len(entries) > MaxInstalledPersonas {
		return nil, fmt.Errorf("Codex persona inventory exceeds limit of %d", MaxInstalledPersonas)
	}
	out := make([]InstalledPersona, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".toml" || strings.TrimSuffix(name, ".toml") == "" {
			return nil, fmt.Errorf("unsafe entry in Codex persona inventory")
		}
		persona := strings.TrimSuffix(name, ".toml")
		if wanted != "" && persona != wanted {
			continue
		}
		fd, e := unix.Openat(afd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if e != nil {
			return nil, fmt.Errorf("persona %q has an unsafe Codex artifact", persona)
		}
		var before, after, named unix.Stat_t
		e = unix.Fstat(fd, &before)
		if e == nil && before.Mode&unix.S_IFMT != unix.S_IFREG {
			e = fs.ErrInvalid
		}
		if e == nil && before.Nlink != 1 {
			e = fs.ErrInvalid
		}
		if e == nil && os.FileMode(before.Mode).Perm()&0o133 != 0 {
			e = fs.ErrPermission
		}
		if e == nil && before.Size > MaxCodexAgentBytes {
			e = fs.ErrInvalid
		}
		var raw []byte
		if e == nil {
			f := os.NewFile(uintptr(fd), name)
			raw, e = io.ReadAll(io.LimitReader(f, MaxCodexAgentBytes+1))
			fd = -1
			_ = f.Close()
			if len(raw) > MaxCodexAgentBytes {
				e = fs.ErrInvalid
			}
		}
		if fd >= 0 {
			unix.Close(fd)
		}
		if e == nil {
			e = unix.Fstatat(afd, name, &named, unix.AT_SYMLINK_NOFOLLOW)
		}
		if e == nil {
			tmp, e2 := unix.Openat(afd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if e2 == nil {
				e2 = unix.Fstat(tmp, &after)
				unix.Close(tmp)
			}
			e = e2
		}
		if e != nil || before.Dev != named.Dev || before.Ino != named.Ino || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Mode != named.Mode || before.Mode != after.Mode || named.Nlink != 1 || after.Nlink != 1 {
			return nil, fmt.Errorf("persona %q changed during inventory read", persona)
		}
		out = append(out, InstalledPersona{Name: persona, Raw: raw, Mode: os.FileMode(before.Mode).Perm(), Device: uint64(before.Dev), Inode: before.Ino})
	}
	return out, nil
}
