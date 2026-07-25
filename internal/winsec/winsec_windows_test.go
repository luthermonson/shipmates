//go:build windows

package winsec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestChildRefusesAnythingButOneLiteralName(t *testing.T) {
	const dir = `\\?\C:\project\.shipmates`
	for _, bad := range []string{
		"", ".", "..",
		`..\policy.yaml`, "../policy.yaml",
		`sub\policy.yaml`, "sub/policy.yaml",
		"C:policy.yaml", "policy.yaml:stream",
		"policy.yaml.", "policy.yaml ",
		"pol\x00icy.yaml", "pol\ticy.yaml",
		"policy*.yaml", "policy?.yaml", `policy"`, "policy<", "policy>", "policy|",
	} {
		if got, err := Child(dir, bad); err == nil {
			t.Errorf("Child(%q) = %q, want refusal", bad, got)
		} else if !errors.Is(err, ErrBadComponent) {
			t.Errorf("Child(%q) error = %v, want ErrBadComponent", bad, err)
		}
	}
	got, err := Child(dir, "policy.yaml")
	if err != nil || got != dir+`\policy.yaml` {
		t.Fatalf("Child = %q, %v", got, err)
	}
	if got, err := Child(dir+`\`, "policy.yaml"); err != nil || got != dir+`\policy.yaml` {
		t.Fatalf("Child with trailing separator = %q, %v", got, err)
	}
}

func TestOpenRefusesReparsePointsAndWrongTypes(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "real.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory opened without FILE_FLAG_BACKUP_SEMANTICS is refused by
	// CreateFile itself with ERROR_ACCESS_DENIED, before Open's own type check
	// can run. The refusal is what matters; distinguishing "it is a directory"
	// from "permission denied" needs Probe, which is why callers that report
	// diagnostics use it.
	if _, _, err := Open(root, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING); err == nil {
		t.Error("directory opened as file")
	} else if id, probeErr := Probe(root); probeErr != nil || !id.IsDir() {
		t.Errorf("Probe could not classify the directory: %+v %v", id, probeErr)
	}
	if _, _, err := Open(file, true, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING); !errors.Is(err, ErrWrongType) {
		t.Errorf("file opened as directory: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(file, link); err != nil {
		t.Logf("symlink unavailable, skipping the link half: %v", err)
	} else if _, _, err := Open(link, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING); !errors.Is(err, ErrReparsePoint) {
		t.Errorf("symlink accepted: %v", err)
	}
	alias := filepath.Join(root, "alias.txt")
	if err := os.Link(file, alias); err != nil {
		t.Logf("hard links unavailable, skipping the link-count half: %v", err)
	} else if _, _, err := Open(file, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING); !errors.Is(err, ErrHardLink) {
		t.Errorf("hardlinked file accepted: %v", err)
	}
}

func TestIdentityDistinguishesObjectsAndMetadataChanges(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, idA, err := Open(a, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	_, idB, err := Open(b, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	if idA == idB {
		t.Fatal("two distinct files share an identity")
	}
	// Re-opening the same object must reproduce the identity exactly, or the
	// loader's after-the-fact revalidation would produce false positives.
	_, again, err := Open(a, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	if again != idA {
		t.Fatalf("identity is not stable across opens: %+v vs %+v", again, idA)
	}
	if err := os.WriteFile(a, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rewritten, err := Open(a, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten == idA {
		t.Fatal("rewriting the file left its identity unchanged")
	}
}

func TestFinalPathIsCanonicalAndVerified(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "real.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _, err := Open(file, false, windows.FILE_READ_ATTRIBUTES, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	got, err := FinalPath(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, `\\?\`) || !strings.HasSuffix(strings.ToLower(got), `\real.txt`) {
		t.Fatalf("FinalPath = %q", got)
	}
	if err := VerifyFinalPath(h, got); err != nil {
		t.Fatalf("VerifyFinalPath on its own answer: %v", err)
	}
	if err := VerifyFinalPath(h, got+"x"); !errors.Is(err, ErrPathMismatch) {
		t.Fatalf("VerifyFinalPath on a different path = %v", err)
	}
}

func TestOpenDirChainCanonicalizesAndRefusesMissingComponents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := OpenDirChain(root, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasPrefix(c.Path, `\\?\`) || !strings.HasSuffix(strings.ToLower(c.Path), `\a\b`) {
		t.Fatalf("chain path = %q", c.Path)
	}
	if err := VerifyFinalPath(c.Leaf(), c.Path); err != nil {
		t.Fatalf("chain leaf does not name its own path: %v", err)
	}
	if _, err := OpenDirChain(root, "a", "nope"); err == nil {
		t.Fatal("missing component accepted")
	}
	if _, err := OpenDirChain(root, "a", ".."); !errors.Is(err, ErrBadComponent) {
		t.Fatalf("dot-dot component = %v, want ErrBadComponent", err)
	}
}

func TestLockExcludesOtherHandlesAndReleases(t *testing.T) {
	root := t.TempDir()
	chain, err := OpenDirChain(root)
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Close()
	first, err := OpenLockFile(chain.Path, ".probe.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(first)
	second, err := OpenLockFile(chain.Path, ".probe.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(second)

	if err := Lock(first, true, true); err != nil {
		t.Fatal(err)
	}
	if err := Lock(second, true, false); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("second exclusive lock = %v, want ERROR_LOCK_VIOLATION", err)
	}
	if err := Lock(second, false, false); !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("shared lock under an exclusive holder = %v, want ERROR_LOCK_VIOLATION", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	// Shared locks must coexist, the way flock LOCK_SH does for readers.
	if err := Lock(first, false, false); err != nil {
		t.Fatalf("first shared lock: %v", err)
	}
	if err := Lock(second, false, false); err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if err := Lock(second, true, false); err == nil {
		t.Fatal("exclusive lock granted while shared locks are held")
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	if err := Unlock(second); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateDACLIsWrittenAndVerified(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name    string
		inherit bool
		open    func(string) (windows.Handle, error)
	}{
		{"file", false, func(p string) (windows.Handle, error) {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				return windows.InvalidHandle, err
			}
			h, _, err := Open(p, false, windows.READ_CONTROL|windows.WRITE_DAC, windows.OPEN_EXISTING)
			return h, err
		}},
		{"directory", true, func(p string) (windows.Handle, error) {
			if err := os.Mkdir(p, 0o700); err != nil {
				return windows.InvalidHandle, err
			}
			h, _, err := Open(p, true, windows.READ_CONTROL|windows.WRITE_DAC, windows.OPEN_EXISTING)
			return h, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := tc.open(filepath.Join(root, tc.name))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(h)
			if err := PrivateDACL(h, tc.inherit); err != nil {
				t.Fatalf("PrivateDACL: %v", err)
			}
			if err := VerifyPrivateDACL(h, tc.inherit); err != nil {
				t.Fatalf("VerifyPrivateDACL: %v", err)
			}
			// A world grant added afterwards must be detected, then repaired.
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
			if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
				t.Fatal(err)
			}
			if err := VerifyPrivateDACL(h, tc.inherit); !errors.Is(err, ErrDACL) {
				t.Fatalf("world grant went undetected: %v", err)
			}
			if err := PrivateDACL(h, tc.inherit); err != nil {
				t.Fatalf("repair: %v", err)
			}
		})
	}
}
