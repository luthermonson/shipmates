//go:build windows

package policy

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
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

// mkJunction plants a directory junction — a mount-point reparse point — at
// link pointing at target.
//
// Junctions are the realistic Windows attack that symlinks are on unix:
// unlike CreateSymbolicLink they need no SeCreateSymbolicLinkPrivilege and no
// developer mode, so an unprivileged local process can plant one in any
// directory it can write. Every reparse test that can use a junction does, so
// coverage does not depend on the runner's privilege level.
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open junction placeholder: %v", err)
	}
	defer windows.CloseHandle(h)

	substitute := utf16.Encode([]rune(`\??\` + target))
	printName := utf16.Encode([]rune(target))
	subBytes := len(substitute) * 2
	printBytes := len(printName) * 2
	// MountPointReparseBuffer: four uint16 fields then the path buffer, which
	// holds the substitute name and the print name, each NUL terminated.
	dataLen := 8 + subBytes + 2 + printBytes + 2
	buf := make([]byte, 8+dataLen)
	binary.LittleEndian.PutUint32(buf[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buf[4:], uint16(dataLen))
	binary.LittleEndian.PutUint16(buf[8:], 0)
	binary.LittleEndian.PutUint16(buf[10:], uint16(subBytes))
	binary.LittleEndian.PutUint16(buf[12:], uint16(subBytes+2))
	binary.LittleEndian.PutUint16(buf[14:], uint16(printBytes))
	off := 16
	for _, c := range substitute {
		binary.LittleEndian.PutUint16(buf[off:], c)
		off += 2
	}
	off += 2
	for _, c := range printName {
		binary.LittleEndian.PutUint16(buf[off:], c)
		off += 2
	}
	var returned uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &returned, nil); err != nil {
		t.Fatalf("set reparse point: %v", err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable on this runner: %v", err)
	}
}

// tryExclusivePolicyLock takes the non-blocking exclusive side of the policy
// lock object, the LOCK_EX|LOCK_NB analogue used by the unix loader tests.
func tryExclusivePolicyLock(root string) error {
	dir, err := winsec.OpenDirChain(root, ".shipmates")
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

func mutateWithPolicyLock(t *testing.T, root string, mutate func()) (<-chan error, <-chan struct{}) {
	t.Helper()
	attempted := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		attempted <- tryExclusivePolicyLock(root)
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
	if s2, ds := Load(root, `..\backend`, "root-id"); s2 != nil || !hasCode(ds, "policy_outside_project") {
		t.Fatalf("backslash persona accepted: %+v %+v", s2, ds)
	}
	if s2, ds := Load(root, "c:backend", "root-id"); s2 != nil || !hasCode(ds, "policy_outside_project") {
		t.Fatalf("drive-relative persona accepted: %+v %+v", s2, ds)
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

func TestLoadRefusesReparsePointsAncestorsAndSpecialFiles(t *testing.T) {
	t.Run("leaf symlink", func(t *testing.T) {
		root := policyTree(t, false)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, ".shipmates", "policy.yaml")); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, outside, filepath.Join(root, ".shipmates", "policy.yaml"))
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("ancestor symlink", func(t *testing.T) {
		root := t.TempDir()
		target := policyTree(t, false)
		mustSymlink(t, filepath.Join(target, ".shipmates"), filepath.Join(root, ".shipmates"))
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("ancestor junction", func(t *testing.T) {
		root := t.TempDir()
		target := policyTree(t, false)
		mkJunction(t, filepath.Join(root, ".shipmates"), filepath.Join(target, ".shipmates"))
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("persona directory junction", func(t *testing.T) {
		root := policyTree(t, false)
		target := policyTree(t, false)
		policies := filepath.Join(root, ".shipmates", "policies")
		if err := os.RemoveAll(policies); err != nil {
			t.Fatal(err)
		}
		mkJunction(t, policies, filepath.Join(target, ".shipmates", "policies"))
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("project root junction", func(t *testing.T) {
		outer := t.TempDir()
		target := policyTree(t, false)
		root := filepath.Join(outer, "root")
		mkJunction(t, root, target)
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_symlink") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("directory in place of policy", func(t *testing.T) {
		root := policyTree(t, false)
		p := filepath.Join(root, ".shipmates", "policy.yaml")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_not_regular") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
	t.Run("hardlinked policy", func(t *testing.T) {
		// A second directory entry for the same file object lets anyone who
		// can write the other name rewrite policy without ever touching
		// .shipmates, so a policy source with NumberOfLinks != 1 is refused.
		root := policyTree(t, false)
		p := filepath.Join(root, ".shipmates", "policy.yaml")
		alias := filepath.Join(t.TempDir(), "alias.yaml")
		if err := os.Link(p, alias); err != nil {
			t.Skipf("hard links unavailable on this filesystem: %v", err)
		}
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_not_regular") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
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
	testPolicyReadHook = func(int) {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testPolicyReadHook = nil })
	if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
		t.Fatalf("%+v %+v", s, ds)
	}
}

// TestLoadPinsPolicyAgainstRenameSubstitution is the Windows counterpart of
// the unix TestLoadRefusesRenameSubstitutionRace. Unix allows the rename and
// catches it with the post-read fstatat identity check; Windows makes the
// rename impossible for as long as the loader holds the descriptor, because
// the handle is opened without FILE_SHARE_DELETE. Both properties are
// asserted here: the substitution is refused by the kernel, and the snapshot
// the loader returns is the original file.
func TestLoadPinsPolicyAgainstRenameSubstitution(t *testing.T) {
	root := policyTree(t, false)
	p := filepath.Join(root, ".shipmates", "policy.yaml")
	replacement := filepath.Join(root, ".shipmates", "replacement.yaml")
	hostile := "version: 1\nallow:\n  - id: pwn\n    kind: process.exec\n    command_exact: rm -rf /\n"
	if err := os.WriteFile(replacement, []byte(hostile), 0600); err != nil {
		t.Fatal(err)
	}
	var renameErr error
	var deleteErr error
	testPolicyReadHook = func(int) {
		if renameErr == nil && deleteErr == nil {
			renameErr = os.Rename(replacement, p)
			deleteErr = os.Remove(p)
		}
	}
	t.Cleanup(func() { testPolicyReadHook = nil })
	s, ds := Load(root, "backend", "r")
	if renameErr == nil {
		t.Fatal("policy file was replaced while the loader held it open")
	}
	if deleteErr == nil {
		t.Fatal("policy file was deleted while the loader held it open")
	}
	if s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	if len(s.Rules) != 0 {
		t.Fatalf("hostile replacement entered the snapshot: %+v", s.Rules)
	}
}

func TestLoadRevalidatesAllLayersAfterCapture(t *testing.T) {
	t.Run("earlier required layer rewritten while later layer opens", func(t *testing.T) {
		root := policyTree(t, false)
		base := filepath.Join(root, ".shipmates", "policy.yaml")
		reads := 0
		testPolicyReadHook = func(int) {
			reads++
			if reads == 2 {
				if err := os.WriteFile(base, []byte(emptyPolicy+"\n"), 0600); err != nil {
					t.Error(err)
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
					t.Error(err)
				}
			}
		}
		t.Cleanup(func() { testPolicyReadHook = nil })
		if s, ds := Load(root, "backend", "r"); s != nil || !hasCode(ds, "policy_changed_during_load") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})

	t.Run("optional absence filled by a reparse point", func(t *testing.T) {
		root := policyTree(t, false)
		local := filepath.Join(root, ".shipmates", "policy.local.yaml")
		outside := filepath.Join(t.TempDir(), "local.yaml")
		if err := os.WriteFile(outside, []byte(emptyPolicy), 0600); err != nil {
			t.Fatal(err)
		}
		reads := 0
		var linked bool
		testPolicyReadHook = func(int) {
			reads++
			if reads == 2 && !linked {
				linked = os.Symlink(outside, local) == nil
			}
		}
		t.Cleanup(func() { testPolicyReadHook = nil })
		s, ds := Load(root, "backend", "r")
		if !linked {
			t.Skip("symlink unavailable on this runner")
		}
		if s != nil || !hasCode(ds, "policy_changed_during_load") {
			t.Fatalf("%+v %+v", s, ds)
		}
	})
}

func TestLoadPolicyLockDefeatsRequiredABA(t *testing.T) {
	root := policyTree(t, false)
	base := filepath.Join(root, ".shipmates", "policy.yaml")
	away := filepath.Join(root, ".shipmates", "policy.away.yaml")
	var once sync.Once
	var done <-chan struct{}
	testPolicyCaptureHook = func(layer int) {
		if layer != 0 {
			return
		}
		once.Do(func() {
			attempted, d := mutateWithPolicyLock(t, root, func() {
				if err := os.Rename(base, away); err != nil {
					t.Errorf("rename away: %v", err)
					return
				}
				if err := os.Rename(away, base); err != nil {
					t.Errorf("restore: %v", err)
				}
			})
			done = d
			if err := <-attempted; !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				t.Errorf("required ABA exclusive lock attempt = %v, want ERROR_LOCK_VIOLATION", err)
			}
		})
	}
	t.Cleanup(func() { testPolicyCaptureHook = nil })
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	<-done
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("policy not restored by the serialized writer: %v", err)
	}
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
			if err := <-attempted; !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				t.Errorf("optional ABA exclusive lock attempt = %v, want ERROR_LOCK_VIOLATION", err)
			}
		})
	}
	t.Cleanup(func() { testPolicyCaptureHook = nil })
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	<-done
}

// TestLoadRepairsAndHardensTheLockObject asserts the one file the Windows
// loader creates is private to this user, and that a lock object planted with
// a world-writable ACL is repaired rather than trusted.
func TestLoadRepairsAndHardensTheLockObject(t *testing.T) {
	root := policyTree(t, false)
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	lockPath := filepath.Join(root, ".shipmates", policyLockName)
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(world)},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(lockPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if s, ds := Load(root, "backend", "r"); s == nil || len(ds) != 0 {
		t.Fatalf("snapshot=%+v diagnostics=%+v", s, ds)
	}
	h, _, err := winsec.Open(lockPath, false, windows.READ_CONTROL, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if err := winsec.VerifyPrivateDACL(h, false); err != nil {
		t.Fatalf("lock object is not private after load: %v", err)
	}
}

// TestConcurrentLoadsAndMutationsSerialize drives readers and writers at the
// same lock object from many goroutines. Every snapshot must be internally
// consistent: no loader may observe a half-applied mutation.
func TestConcurrentLoadsAndMutationsSerialize(t *testing.T) {
	root := policyTree(t, false)
	local := filepath.Join(root, ".shipmates", "policy.local.yaml")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 15; n++ {
				if s, ds := Load(root, "backend", "r"); s == nil {
					t.Errorf("concurrent load failed: %+v", ds)
					return
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 15; n++ {
				lock, err := project.AcquirePolicyWriteLock(root)
				if err != nil {
					t.Errorf("concurrent write lock failed: %v", err)
					return
				}
				if err := os.WriteFile(local, []byte(emptyPolicy), 0600); err != nil {
					t.Errorf("write optional: %v", err)
				}
				if err := os.Remove(local); err != nil {
					t.Errorf("remove optional: %v", err)
				}
				if err := lock.Close(); err != nil {
					t.Errorf("release write lock: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
