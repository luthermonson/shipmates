//go:build unix

package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveOwnedServerStateNeverDeletesSuccessor(t *testing.T) {
	root := t.TempDir()
	old, next := []byte("old\n"), []byte("next\n")
	if err := WriteServerStateFile(root, "server.json", old); err != nil {
		t.Fatal(err)
	}
	if err := WriteServerStateFile(root, "server.json", next); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedServerStateFile(root, "server.json", old); err == nil {
		t.Fatal("old owner removed successor")
	}
	got, err := ReadServerStateFile(root, "server.json", 4096)
	if err != nil || string(got) != string(next) {
		t.Fatalf("successor=%q err=%v", got, err)
	}
	if err := RemoveOwnedServerStateFile(root, "server.json", next); err != nil {
		t.Fatal(err)
	}
}

func TestOpenServerLogRefusesSymlinkAndHardlink(t *testing.T) {
	root := t.TempDir()
	if err := EnsureServerStateDirectory(root); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, Dir, SessionsDirName, "server.log")
	if err := os.Symlink(external, logPath); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenServerLog(root); err == nil {
		f.Close()
		t.Fatal("symlink log accepted")
	}
	if got, _ := os.ReadFile(external); string(got) != "canary" {
		t.Fatal("external target changed")
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, logPath); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenServerLog(root); err == nil {
		f.Close()
		t.Fatal("hardlinked log accepted")
	}
}

func TestOpenServerLogPrivatizesExistingFileBeforeWriting(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			root := t.TempDir()
			if err := EnsureServerStateDirectory(root); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, Dir, SessionsDirName, "server.log")
			if err := os.WriteFile(path, []byte("existing"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			f, err := OpenServerLog(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("next")); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%o err=%v", info.Mode().Perm(), err)
			}
		})
	}
}
