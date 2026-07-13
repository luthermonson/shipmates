//go:build unix

package project

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPolicyWriteLocksDoNotOverlap(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := AcquirePolicyWriteLock(root)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(filepath.Join(root, Dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("second writer lock = %v, want would-block", err)
	}
	acquired := make(chan *PolicyWriteLock, 1)
	go func() { lock, _ := AcquirePolicyWriteLock(root); acquired <- lock }()
	select {
	case <-acquired:
		t.Fatal("second writer entered the first writer's interval")
	default:
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := <-acquired
	if second == nil {
		t.Fatal("second writer did not acquire after release")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
