//go:build unix

package project

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func secureServerStateDir(root string, create bool) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, name := range []string{Dir, SessionsDirName} {
		next, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if e == unix.ENOENT && !create {
			unix.Close(fd)
			return -1, e
		}
		if e == unix.ENOENT && create {
			if e = unix.Mkdirat(fd, name, 0o700); e == nil {
				next, e = unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
		}
		unix.Close(fd)
		if e != nil {
			return -1, errors.New("unsafe server state directory")
		}
		fd = next
	}
	return fd, nil
}

func EnsureServerStateDirectory(root string) error {
	fd, err := secureServerStateDir(root, true)
	if err == nil {
		err = unix.Close(fd)
	}
	return err
}

func ReadServerStateFile(root, name string, limit int64) ([]byte, error) {
	if name != "server.json" || limit <= 0 {
		return nil, errors.New("invalid server state file")
	}
	dfd, err := secureServerStateDir(root, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var before, after, named unix.Stat_t
	if err = unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Mode&0o077 != 0 || before.Size <= 0 || before.Size > limit {
		unix.Close(fd)
		return nil, errors.New("unsafe server state file")
	}
	f := os.NewFile(uintptr(fd), name)
	b, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	statErr := unix.Fstat(int(f.Fd()), &after)
	closeErr := f.Close()
	nameErr := unix.Fstatat(dfd, name, &named, unix.AT_SYMLINK_NOFOLLOW)
	if readErr != nil || statErr != nil || closeErr != nil || nameErr != nil || int64(len(b)) > limit || before.Dev != after.Dev || before.Ino != after.Ino || before.Dev != named.Dev || before.Ino != named.Ino || before.Size != after.Size || before.Mtim != after.Mtim || named.Nlink != 1 {
		return nil, errors.New("server state changed during read")
	}
	return b, nil
}

func WriteServerStateFile(root, name string, raw []byte) error {
	if name != "server.json" || len(raw) == 0 || len(raw) > 4096 {
		return errors.New("invalid server state file")
	}
	dfd, err := secureServerStateDir(root, true)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Flock(dfd, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(dfd, unix.LOCK_UN)
	var existing unix.Stat_t
	if e := unix.Fstatat(dfd, name, &existing, unix.AT_SYMLINK_NOFOLLOW); e == nil && (existing.Mode&unix.S_IFMT != unix.S_IFREG || existing.Nlink != 1) {
		return errors.New("unsafe existing server state file")
	} else if e != nil && e != unix.ENOENT {
		return e
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	tmp := ".server-ready-" + hex.EncodeToString(id[:])
	fd, err := unix.Openat(dfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = unix.Unlinkat(dfd, tmp, 0)
		}
	}()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = unix.Renameat(dfd, tmp, dfd, name); err != nil {
		return err
	}
	if err = unix.Fsync(dfd); err != nil {
		return err
	}
	ok = true
	return nil
}

func RemoveOwnedServerStateFile(root, name string, owned []byte) error {
	if name != "server.json" || len(owned) == 0 {
		return errors.New("invalid server state file")
	}
	dfd, err := secureServerStateDir(root, false)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	if err := unix.Flock(dfd, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(dfd, unix.LOCK_UN)
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var held, named unix.Stat_t
	if err = unix.Fstat(fd, &held); err != nil || held.Mode&unix.S_IFMT != unix.S_IFREG || held.Nlink != 1 {
		unix.Close(fd)
		return errors.New("unsafe server state file")
	}
	f := os.NewFile(uintptr(fd), name)
	b, readErr := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || string(b) != string(owned) {
		return errors.New("server state ownership changed")
	}
	if err := unix.Fstatat(dfd, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil || named.Dev != held.Dev || named.Ino != held.Ino || named.Nlink != 1 {
		return errors.New("server state ownership changed")
	}
	if err := unix.Unlinkat(dfd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(dfd)
}

func OpenServerLog(root string) (*os.File, error) {
	dfd, err := secureServerStateDir(root, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, "server.log", unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		unix.Close(fd)
		return nil, errors.New("unsafe server log")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&0o077 != 0 {
		unix.Close(fd)
		return nil, errors.New("unsafe server log")
	}
	return os.NewFile(uintptr(fd), "server.log"), nil
}
