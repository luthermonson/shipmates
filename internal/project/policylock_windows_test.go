//go:build windows

package project

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
)

func tryExclusive(t *testing.T, root string) error {
	t.Helper()
	dir, err := winsec.OpenDirChain(root, Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	h, err := winsec.OpenLockFile(dir.Path, policyLockName)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := winsec.Lock(h, true, false); err != nil {
		return err
	}
	return winsec.Unlock(h)
}

func TestPolicyWriteLocksDoNotOverlap(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := AcquirePolicyWriteLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryExclusive(t, root); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("second writer lock = %v, want ERROR_LOCK_VIOLATION", err)
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

// TestPolicyWriteLockRefusesReparseDirectory keeps the mutation side honest
// about the same reparse-point refusal the loader applies: a junction planted
// where .shipmates should be must not become a writable policy directory.
func TestPolicyWriteLockRefusesReparseDirectory(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, Dir)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if lock, err := AcquirePolicyWriteLock(root); err == nil {
		lock.Close()
		t.Fatal("reparse policy directory accepted for mutation")
	}
}

// TestPolicyWriteLockHardensTheLockObject asserts the lock file the Windows
// mutation path creates is readable and writable only by this user and
// LOCAL SYSTEM.
func TestPolicyWriteLockHardensTheLockObject(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquirePolicyWriteLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	h, _, err := winsec.Open(filepath.Join(root, Dir, policyLockName), false, windows.READ_CONTROL, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := winsec.VerifyPrivateDACL(h, false); err != nil {
		t.Fatalf("policy lock object is not private: %v", err)
	}
}

// TestPolicyWriteLockSerializesMutations runs many writers at the same lock
// and requires each critical section to observe only its own work.
func TestPolicyWriteLockSerializesMutations(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, Dir, "serialized.tmp")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				lock, err := AcquirePolicyWriteLock(root)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				if err := os.WriteFile(marker, []byte("held"), 0o600); err != nil {
					t.Errorf("write marker: %v", err)
				}
				if err := os.Remove(marker); err != nil {
					t.Errorf("remove marker: %v", err)
				}
				if err := lock.Close(); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
