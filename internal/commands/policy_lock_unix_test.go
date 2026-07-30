//go:build unix

package commands

import (
	"os"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
	"golang.org/x/sys/unix"
)

func holdPolicyReader(t *testing.T) (release func()) {
	t.Helper()
	fd, err := unix.Open(project.Dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }
}

func TestLifecyclePolicyMutationsWaitForReaders(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(project.PoliciesDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&project.Manifest{Version: project.ManifestVersion, Files: map[string]string{}}).Save(); err != nil {
			t.Fatal(err)
		}
		release := holdPolicyReader(t)
		attempted := make(chan struct{})
		original := acquirePolicyWriteLock
		acquirePolicyWriteLock = func(root string) (*project.PolicyWriteLock, error) { close(attempted); return original(root) }
		t.Cleanup(func() { acquirePolicyWriteLock = original })
		done := make(chan error, 1)
		go func() { done <- addPersona(lifecycleCatalog("role", lifecyclePolicy("safe")), "security") }()
		<-attempted
		if _, err := os.Lstat(project.PolicyPath("security")); !os.IsNotExist(err) {
			t.Fatalf("policy changed during reader interval: %v", err)
		}
		select {
		case err := <-done:
			t.Fatalf("add overlapped reader: %v", err)
		default:
		}
		release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("update and remove", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(project.PoliciesDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (&project.Manifest{Version: project.ManifestVersion, Files: map[string]string{}}).Save(); err != nil {
			t.Fatal(err)
		}
		if err := addPersona(lifecycleCatalog("old", lifecyclePolicy("old")), "security"); err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			name      string
			mutate    func() error
			unchanged func()
		}{
			{"update", func() error { return runUpdate(lifecycleCatalog("new", lifecyclePolicy("new")), "security", "theirs") }, func() {
				b, err := os.ReadFile(project.PolicyPath("security"))
				if err != nil || string(b) != lifecyclePolicy("old") {
					t.Fatalf("policy changed during update contention: %q %v", b, err)
				}
			}},
			{"remove", func() error { return runRemove("security", false, false) }, func() {
				if _, err := os.Lstat(project.PolicyPath("security")); err != nil {
					t.Fatalf("policy removed during contention: %v", err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				release := holdPolicyReader(t)
				attempted := make(chan struct{})
				original := acquirePolicyWriteLock
				acquirePolicyWriteLock = func(root string) (*project.PolicyWriteLock, error) { close(attempted); return original(root) }
				t.Cleanup(func() { acquirePolicyWriteLock = original })
				done := make(chan error, 1)
				go func() { done <- tc.mutate() }()
				<-attempted
				tc.unchanged()
				select {
				case err := <-done:
					t.Fatalf("%s overlapped reader: %v", tc.name, err)
				default:
				}
				release()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}
