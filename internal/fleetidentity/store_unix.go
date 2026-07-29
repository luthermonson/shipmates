//go:build unix

package fleetidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const authorityLockFile = ".fleet-authority.lock"

// lockAuthorityStore serializes load-mutate-commit against every other process
// that opens the same authority store. Without it a long-running server never
// observes a CLI revocation, and its next commit rewrites the whole file from
// its own stale snapshot, un-revoking the credential on disk.
func lockAuthorityStore(dir string) (func(), error) {
	dirfd, err := openDirectoryPath(dir, true)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)
	f, e := unix.Openat(dirfd, authorityLockFile, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if e != nil {
		return nil, storageError()
	}
	if unix.Flock(f, unix.LOCK_EX) != nil {
		unix.Close(f)
		return nil, storageError()
	}
	return func() {
		_ = unix.Flock(f, unix.LOCK_UN)
		unix.Close(f)
	}, nil
}

// uniqueTempName keeps two writers from creating, unlinking, and renaming each
// other's half-written temporary file into place under one fixed name.
func uniqueTempName(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", storageError()
	}
	return prefix + hex.EncodeToString(b[:]) + ".tmp", nil
}

func readAuthority(dir string) ([]byte, bool, error) {
	fd, err := openDirectoryPath(dir, false)
	if errors.Is(err, errStatePathMissing) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer unix.Close(fd)
	f, e := unix.Openat(fd, authorityFile, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(e, syscall.ENOENT) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, storageError()
	}
	defer unix.Close(f)
	var st unix.Stat_t
	if unix.Fstat(f, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 || st.Size > 4<<20 {
		return nil, false, storageError()
	}
	b := make([]byte, st.Size)
	n, e := unix.Read(f, b)
	if e != nil && e != io.EOF || n != len(b) {
		return nil, false, storageError()
	}
	return b, true, nil
}
func writeAuthority(dir string, b []byte) error {
	fd, err := openDirectoryPath(dir, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	tmp, err := uniqueTempName(".fleet-authority.")
	if err != nil {
		return err
	}
	f, e := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if e != nil {
		return storageError()
	}
	// Closing a raw descriptor twice can close an unrelated descriptor that a
	// concurrent goroutine has already been given the same number.
	closed, renamed := false, false
	defer func() {
		if !closed {
			unix.Close(f)
		}
		if !renamed {
			_ = unix.Unlinkat(fd, tmp, 0)
		}
	}()
	n, e := unix.Write(f, b)
	if e != nil || n != len(b) || unix.Fsync(f) != nil {
		return storageError()
	}
	closeErr := unix.Close(f)
	closed = true
	if closeErr != nil {
		return storageError()
	}
	if unix.Renameat(fd, tmp, fd, authorityFile) != nil {
		return storageError()
	}
	renamed = true
	if unix.Fsync(fd) != nil {
		return &CommitUncertainError{Err: storageError()}
	}
	return nil
}

var errStatePathMissing = errors.New("ship identity state path missing")

func openDirectoryPath(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, storageError()
	}
	parts := splitAbs(path)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, storageError()
	}
	for _, p := range parts {
		next, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(e, syscall.ENOENT) && create {
			if unix.Mkdirat(fd, p, 0o700) != nil {
				unix.Close(fd)
				return -1, storageError()
			}
			next, e = unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		unix.Close(fd)
		if e != nil {
			if errors.Is(e, syscall.ENOENT) {
				return -1, errStatePathMissing
			}
			return -1, storageError()
		}
		fd = next
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o077 != 0 {
		unix.Close(fd)
		return -1, storageError()
	}
	return fd, nil
}
func splitAbs(path string) []string {
	v := []string{}
	for p := path; p != "/"; {
		d, b := filepath.Split(p)
		if b != "" {
			v = append([]string{b}, v...)
		}
		p = filepath.Clean(d)
	}
	return v
}
func planShipState(dir string) (StatePlan, error) {
	fd, err := openDirectoryPath(dir, false)
	if err != nil {
		if errors.Is(err, errStatePathMissing) {
			return StatePlan{Path: filepath.Join(dir, shipStateFile)}, nil
		}
		return StatePlan{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	e := unix.Fstatat(fd, shipStateFile, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(e, syscall.ENOENT) {
		return StatePlan{Path: filepath.Join(dir, shipStateFile)}, nil
	}
	if e != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 {
		return StatePlan{}, storageError()
	}
	return StatePlan{Path: filepath.Join(dir, shipStateFile), Exists: true}, nil
}
func readShipState(dir string) ([]byte, error) {
	fd, err := openDirectoryPath(dir, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	leaf, err := unix.Openat(fd, shipStateFile, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, storageError()
	}
	defer unix.Close(leaf)
	var st unix.Stat_t
	if unix.Fstat(leaf, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 || st.Size > 4096 {
		return nil, storageError()
	}
	b := make([]byte, st.Size)
	n, err := unix.Read(leaf, b)
	if err != nil || n != len(b) {
		return nil, storageError()
	}
	return b, nil
}
func writeShipState(dir string, b []byte, replace bool) error {
	fd, err := openDirectoryPath(dir, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	e := unix.Fstatat(fd, shipStateFile, &st, unix.AT_SYMLINK_NOFOLLOW)
	exists := e == nil
	if e != nil && !errors.Is(e, syscall.ENOENT) {
		return storageError()
	}
	if exists && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0) {
		return storageError()
	}
	if exists && !replace {
		return fail(AlreadyExists)
	}
	if !exists && replace {
		return fail(NotFound)
	}
	tmp, err := uniqueTempName(".identity.")
	if err != nil {
		return err
	}
	t, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return storageError()
	}
	closed, renamed := false, false
	defer func() {
		if !closed {
			unix.Close(t)
		}
		if !renamed {
			_ = unix.Unlinkat(fd, tmp, 0)
		}
	}()
	n, e := unix.Write(t, b)
	if e != nil || shortWrite(n, len(b)) != nil || unix.Fsync(t) != nil {
		return storageError()
	}
	closeErr := unix.Close(t)
	closed = true
	if closeErr != nil {
		return storageError()
	}
	if unix.Renameat(fd, tmp, fd, shipStateFile) != nil {
		return storageError()
	}
	renamed = true
	if unix.Fsync(fd) != nil {
		return &CommitUncertainError{Err: storageError()}
	}
	return nil
}
