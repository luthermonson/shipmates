//go:build unix

package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func routingMkdir(root, rel string, mode os.FileMode) error {
	fd, name, err := routingParentFD(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Mkdirat(fd, name, uint32(mode.Perm())); err != nil {
		return err
	}
	return unix.Fsync(fd)
}

func routingWriteArtifact(root, rel string, raw []byte) (retErr error) {
	fd, name, err := routingParentFD(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fileFD), name)
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	return f.Sync()
}

func routingReadArtifact(root, rel string) ([]byte, error) {
	fd, name, err := routingParentFD(root, rel)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fileFD), name)
	defer f.Close()
	return io.ReadAll(f)
}

func routingArtifactIdentity(root, rel string) ([]byte, uint64, uint64, os.FileMode, uint64, error) {
	fd, name, err := routingParentFD(root, rel)
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fileFD, &st); err != nil {
		unix.Close(fileFD)
		return nil, 0, 0, 0, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fileFD)
		return nil, 0, 0, 0, 0, errors.New("unsafe routing artifact")
	}
	f := os.NewFile(uintptr(fileFD), name)
	raw, err := io.ReadAll(io.LimitReader(f, MaxCodexAgentBytes+1))
	err = errors.Join(err, f.Close())
	if err != nil || len(raw) > MaxCodexAgentBytes {
		return nil, 0, 0, 0, 0, errors.Join(err, errors.New("unsafe routing artifact size"))
	}
	return raw, uint64(st.Dev), st.Ino, os.FileMode(st.Mode).Perm(), uint64(st.Nlink), nil
}

func routingExchangeDestination(root, stageRel, destinationRel string, expectedDevice, expectedInode uint64, expectedHash string, expectedMode os.FileMode) error {
	stageFD, stageName, err := routingParentFD(root, stageRel)
	if err != nil {
		return err
	}
	defer unix.Close(stageFD)
	destinationFD, destinationName, err := routingParentFD(root, destinationRel)
	if err != nil {
		return err
	}
	defer unix.Close(destinationFD)
	if err := routingRenameExchange(stageFD, stageName, destinationFD, destinationName); err != nil {
		return err
	}
	raw, device, inode, mode, links, verifyErr := routingArtifactIdentity(root, stageRel)
	if verifyErr == nil && device == expectedDevice && inode == expectedInode && SHA(raw) == expectedHash && mode.Perm() == expectedMode.Perm() && links == 1 {
		return nil
	}
	if err := routingRenameExchange(stageFD, stageName, destinationFD, destinationName); err != nil {
		return fmt.Errorf("%w; exchange-back failed: %v", errRoutingDestinationChanged, err)
	}
	return errRoutingDestinationChanged
}

func routingLinkCount(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 0
}

func routingParentFD(root, rel string) (int, string, error) {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || strings.Contains(rel, "\\") {
		return -1, "", errors.New("unsafe routing path")
	}
	parts := strings.Split(rel, "/")
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	for _, p := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if e != nil {
			return -1, "", e
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func routingRename(root, oldRel, newRel string) error {
	ofd, old, err := routingParentFD(root, oldRel)
	if err != nil {
		return err
	}
	defer unix.Close(ofd)
	nfd, newName, err := routingParentFD(root, newRel)
	if err != nil {
		return err
	}
	defer unix.Close(nfd)
	var st unix.Stat_t
	if e := unix.Fstatat(nfd, newName, &st, unix.AT_SYMLINK_NOFOLLOW); e == nil {
		return errors.New("unexpected routing destination")
	} else if e != unix.ENOENT {
		return e
	}
	if err = unix.Renameat(ofd, old, nfd, newName); err != nil {
		return err
	}
	if err = unix.Fsync(ofd); err != nil {
		return err
	}
	if nfd != ofd {
		err = unix.Fsync(nfd)
	}
	return err
}

func routingReplace(root, oldRel, newRel string) error {
	ofd, old, err := routingParentFD(root, oldRel)
	if err != nil {
		return err
	}
	defer unix.Close(ofd)
	nfd, newName, err := routingParentFD(root, newRel)
	if err != nil {
		return err
	}
	defer unix.Close(nfd)
	if err = unix.Renameat(ofd, old, nfd, newName); err != nil {
		return err
	}
	if err = unix.Fsync(ofd); err != nil {
		return err
	}
	return unix.Fsync(nfd)
}

func routingUnlink(root, rel string) error {
	fd, name, err := routingParentFD(root, rel)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Unlinkat(fd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
