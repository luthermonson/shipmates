//go:build windows

package project

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsServerStateRoundTripAndOwnedRemoval(t *testing.T) {
	root := t.TempDir()
	raw := []byte("record\n")
	if err := WriteServerStateFile(root, "server.json", raw); err != nil {
		t.Fatal(err)
	}
	got, err := ReadServerStateFile(root, "server.json", 4096)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if err := RemoveOwnedServerStateFile(root, "server.json", []byte("other")); err == nil {
		t.Fatal("wrong owner removed record")
	}
	if err := RemoveOwnedServerStateFile(root, "server.json", raw); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsServerLogRepairsPermissiveInheritedACL(t *testing.T) {
	root := t.TempDir()
	if err := EnsureServerStateDirectory(root); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, Dir, SessionsDirName, "server.log")
	if err := os.WriteFile(p, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.GRANT_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeValue: windows.TrusteeValueFromSID(world)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	f, err := OpenServerLog(root)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl.AceCount != 2 {
		t.Fatalf("repaired ACE count=%d err=%v", dacl.AceCount, err)
	}
}

func TestWindowsServerStateRefusesReparseAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, Dir)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := EnsureServerStateDirectory(root); err == nil {
		t.Fatal("reparse ancestor accepted")
	}
}
