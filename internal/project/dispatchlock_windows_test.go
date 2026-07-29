//go:build windows

package project

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Windows byte-range locks are mandatory, so a lock placed over the PID line
// makes that payload unreadable through every other handle — even handles in
// the process holding the lock. Locking byte 0 therefore broke stale-PID
// reclamation and any external reader. Ownership must be exclusive while the
// payload stays readable, which is what advisory flock gives us on unix.
func TestDispatchLockKeepsPayloadReadableWhileHeldOnWindows(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireDispatchLockAt(root, "security")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()
	path := filepath.Join(root, SessionsDir(), "security.dispatch.lock")

	// A completely independent handle, as diagnostics or external tooling use.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read whole lock file while held: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock payload = %q, want pid %d", raw, os.Getpid())
	}

	// The first byte specifically: that is the range the defect locked, and it
	// is the first byte of the PID that reclamation has to parse.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open lock file while held: %v", err)
	}
	defer f.Close()
	var first [1]byte
	if _, err := f.ReadAt(first[:], 0); err != nil {
		t.Fatalf("read first payload byte while held: %v", err)
	}
	if first[0] < '1' || first[0] > '9' {
		t.Fatalf("first payload byte = %q, want a PID digit", first[0])
	}
}

// Moving the lock off the payload must not weaken mutual exclusion: the
// sentinel range still has to make a second holder impossible, including from
// a second handle inside this same process.
func TestDispatchLockSentinelRangeStillExcludesSecondHolderOnWindows(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireDispatchLockAt(root, "security")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	if _, err := AcquireDispatchLockAt(root, "security"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("second acquisition = %v, want busy", err)
	}

	// Directly at the primitive: a fresh handle on the same file must be
	// refused the sentinel range rather than reporting success.
	path := filepath.Join(root, SessionsDir(), "security.dispatch.lock")
	other, err := openDispatchLockFile(path)
	if err != nil {
		t.Fatalf("reopen lock file: %v", err)
	}
	defer other.Close()
	locked, err := tryDispatchFileLock(other)
	if err != nil {
		t.Fatalf("second tryDispatchFileLock error = %v", err)
	}
	if locked {
		_ = unlockDispatchFile(other)
		t.Fatal("second handle acquired the dispatch lock")
	}
}

// The release path unlinks the lock file while ownership is still held, so the
// handle must share delete. os.OpenFile does not on Windows.
func TestDispatchLockFileIsUnlinkableWhileHeldOnWindows(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireDispatchLockAt(root, "security")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	path := filepath.Join(root, SessionsDir(), "security.dispatch.lock")
	if err := os.Remove(path); err != nil {
		t.Fatalf("unlink lock file while held: %v", err)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock file survived release: %v", err)
	}
}
