//go:build linux

package installer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const qualifierLifecycleLock = "var/lib/shipmates/qualifier.lock"

type FenceLease struct {
	file       *os.File
	path       string
	device     uint64
	inode      uint64
	owner      uint32
	allowOwner bool
}

func acquireQualifierLifecycleLease(root string, allowCurrentOwner bool) (*FenceLease, error) {
	if root == "" {
		root = "/"
	}
	path := filepath.Join(root, qualifierLifecycleLock)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, errors.New("qualifier_lock_unavailable")
	}
	for p := parent; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("qualifier_lock_unsafe")
		}
		if p == root || p == "/" {
			break
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errors.New("qualifier_lock_unavailable")
	}
	closeOnError := func() (*FenceLease, error) {
		_ = f.Close()
		return nil, errors.New("qualifier_lock_unsafe")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return closeOnError()
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return closeOnError()
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 || st.Mode&0077 != 0 || (st.Uid != 0 && (!allowCurrentOwner || int(st.Uid) != os.Getuid())) {
		return closeOnError()
	}
	return &FenceLease{file: f, path: path, device: uint64(st.Dev), inode: uint64(st.Ino), owner: st.Uid, allowOwner: allowCurrentOwner}, nil
}

// AcquireQualifierLifecycleLease is shared by installer and the unprivileged
// runner. Production callers must use allowCurrentOwner=false.
func AcquireQualifierLifecycleLease(root string, allowCurrentOwner bool) (*FenceLease, error) {
	return acquireQualifierLifecycleLease(root, allowCurrentOwner)
}

func (l *FenceLease) Recheck() error {
	if l == nil || l.file == nil {
		return errors.New("qualifier_lock_invalid")
	}
	var fdst syscall.Stat_t
	var pst syscall.Stat_t
	if syscall.Fstat(int(l.file.Fd()), &fdst) != nil || syscall.Stat(l.path, &pst) != nil {
		return errors.New("qualifier_lock_replaced")
	}
	if uint64(fdst.Dev) != l.device || uint64(fdst.Ino) != l.inode || uint64(pst.Dev) != l.device || uint64(pst.Ino) != l.inode || fdst.Nlink != 1 || pst.Nlink != 1 || fdst.Mode&0077 != 0 || pst.Mode&0077 != 0 || fdst.Uid != l.owner || pst.Uid != l.owner {
		return errors.New("qualifier_lock_replaced")
	}
	return nil
}

func (l *FenceLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.Recheck()
	if unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err == nil {
		err = unlockErr
	}
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
