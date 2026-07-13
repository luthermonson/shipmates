//go:build unix

package policy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
	"golang.org/x/sys/unix"
)

const emptyPolicy = "version: 1\nallow: []\nask: []\ndeny: []\n"

func policyTree(t *testing.T, local bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".shipmates", "policies"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".shipmates/policy.yaml", ".shipmates/policies/backend.yaml"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if local {
		if err := os.WriteFile(filepath.Join(root, ".shipmates/policy.local.yaml"), []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mutateWithPolicyLock(t *testing.T, root string, mutate func()) (<-chan error, <-chan struct{}) {
	t.Helper()
	attempted := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fd, err := unix.Open(filepath.Join(root, ".shipmates"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			t.Errorf("open policy directory: %v", err)
			return
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		attempted <- err
		if err == nil {
			_ = unix.Flock(fd, unix.LOCK_UN)
		}
		_ = unix.Close(fd)
		lock, err := project.AcquirePolicyWriteLock(root)
		if err != nil {
			t.Errorf("acquire production policy write lock: %v", err)
			return
		}
		defer lock.Close()
		mutate()
	}()
	return attempted, done
}

func TestLoadAssemblesExactSourcesAndOptionalAbsence(t *testing.T) {
	root := policyTree(t, false)
	s, ds := Load(root, "backend", "root-id")
	if s == nil || len(ds) != 0 || len(s.Sources) != 3 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	for _, src := range s.Sources {
		if src.Layer == LayerProjectLocal && src.Present {
			t.Fatal("missing local source marked present")
		}
	}
	if s2, ds := Load(root, "../backend", "root-id"); s2 != nil || !hasCode(ds, "policy_outside_project") {
		t.Fatalf("unsafe persona accepted: %+v %+v", s2, ds)
	}
	if s2, ds := Load(filepath.Join(root, "."), "backend", "root-id"); s2 == nil || len(ds) != 0 {
		t.Fatalf("clean absolute root rejected: %+v", ds)
	}
}

func TestLoadRequiredMissingAndInvalidLocalFailClosed(t *testing.T) {
	root := policyTree(t, false)
	if err := os.Remove(filepath.Join(root, ".shipmates", "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_missing") {
		t.Fatalf("%+v %+v", s, ds)
	}
	root = policyTree(t, true)
	if err := os.WriteFile(filepath.Join(root, ".shipmates", "policy.local.yaml"), []byte("bad: ["), 0600); err != nil {
		t.Fatal(err)
	}
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_invalid_yaml") {
		t.Fatalf("%+v %+v", s, ds)
	}
}

func TestLoadRefusesSymlinksAncestorsAndSpecialFiles(t *testing.T) {
	t.Run("leaf symlink", func(t *testing.T) {
		root := policyTree(t, false)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, ".shipmates", "policy.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".shipmates", "policy.yaml")); err != nil {
			t.Fatal(err)
		}
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("ancestor symlink", func(t *testing.T) {
		root := t.TempDir()
		target := policyTree(t, false)
		if err := os.Symlink(filepath.Join(target, ".shipmates"), filepath.Join(root, ".shipmates")); err != nil {
			t.Fatal(err)
		}
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	for name, makeSpecial := range map[string]func(string) error{
		"directory": func(p string) error { return os.Mkdir(p, 0700) },
		"fifo":      func(p string) error { return unix.Mkfifo(p, 0600) },
	} {
		t.Run(name, func(t *testing.T) {
			root := policyTree(t, false)
			p := filepath.Join(root, ".shipmates", "policy.yaml")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := makeSpecial(p); err != nil {
				t.Fatal(err)
			}
			if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_not_regular") {
				t.Fatalf("%+v %+v", s, ds)
			}
		})
	}
}

func TestLoadBoundedReadAndDescriptorRace(t *testing.T) {
	root := policyTree(t, false)
	p := filepath.Join(root, ".shipmates", "policy.yaml")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", MaxSourceBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_too_large") {
		t.Fatalf("%+v %+v", s, ds)
	}

	root = policyTree(t, false)
	p = filepath.Join(root, ".shipmates", "policy.yaml")
	testPolicyReadHook = func(fd int) {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testPolicyReadHook = nil })
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
		t.Fatalf("%+v %+v", s, ds)
	}
}

func TestLoadRefusesRenameSubstitutionRace(t *testing.T) {
	root := policyTree(t, false)
	p := filepath.Join(root, ".shipmates", "policy.yaml")
	replacement := filepath.Join(root, ".shipmates", "replacement.yaml")
	if err := os.WriteFile(replacement, []byte(emptyPolicy), 0600); err != nil {
		t.Fatal(err)
	}
	testPolicyReadHook = func(fd int) {
		if err := os.Rename(replacement, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { testPolicyReadHook = nil })
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
		t.Fatalf("%+v %+v", s, ds)
	}
}

func TestLoadRevalidatesAllLayersAfterCapture(t *testing.T) {
	t.Run("earlier required layer replaced while later layer opens", func(t *testing.T) {
		root := policyTree(t, false)
		project := filepath.Join(root, ".shipmates", "policy.yaml")
		replacement := filepath.Join(root, ".shipmates", "replacement.yaml")
		if err := os.WriteFile(replacement, []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
		reads := 0
		testPolicyReadHook = func(int) {
			reads++
			if reads == 2 {
				if err := os.Rename(replacement, project); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Cleanup(func() { testPolicyReadHook = nil })
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})

	t.Run("optional absence replaced while later layer opens", func(t *testing.T) {
		root := policyTree(t, false)
		local := filepath.Join(root, ".shipmates", "policy.local.yaml")
		reads := 0
		testPolicyReadHook = func(int) {
			reads++
			if reads == 2 {
				if err := os.WriteFile(local, []byte(emptyPolicy), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		t.Cleanup(func() { testPolicyReadHook = nil })
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
}

func TestLoadPolicyLockDefeatsRequiredABA(t *testing.T) {
	root := policyTree(t, false)
	project := filepath.Join(root, ".shipmates", "policy.yaml")
	away := filepath.Join(root, ".shipmates", "policy.away.yaml")
	var once sync.Once
	var done <-chan struct{}
	testPolicyCaptureHook = func(layer int) {
		if layer != 0 {
			return
		}
		once.Do(func() {
			attempted, d := mutateWithPolicyLock(t, root, func() {
				if err := os.Rename(project, away); err != nil {
					t.Errorf("rename away: %v", err)
					return
				}
				if err := os.Rename(away, project); err != nil {
					t.Errorf("restore: %v", err)
				}
			})
			done = d
			if err := <-attempted; !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				t.Errorf("required ABA exclusive lock attempt = %v, want would-block", err)
			}
		})
	}
	t.Cleanup(func() { testPolicyCaptureHook = nil })
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	<-done
}

func TestLoadPolicyLockDefeatsOptionalABA(t *testing.T) {
	root := policyTree(t, false)
	local := filepath.Join(root, ".shipmates", "policy.local.yaml")
	var once sync.Once
	var done <-chan struct{}
	testPolicyCaptureHook = func(layer int) {
		if layer != 1 {
			return
		}
		once.Do(func() {
			attempted, d := mutateWithPolicyLock(t, root, func() {
				if err := os.WriteFile(local, []byte(emptyPolicy), 0600); err != nil {
					t.Errorf("create optional: %v", err)
					return
				}
				if err := os.Remove(local); err != nil {
					t.Errorf("remove optional: %v", err)
				}
			})
			done = d
			if err := <-attempted; !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				t.Errorf("optional ABA exclusive lock attempt = %v, want would-block", err)
			}
		})
	}
	t.Cleanup(func() { testPolicyCaptureHook = nil })
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	<-done
}
