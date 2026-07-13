//go:build windows

package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const routingAtomicExchangeSupported = false

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func routingMkdir(root, rel string, mode os.FileMode) error {
	p, err := checkedRoutingPath(root, rel)
	if err != nil {
		return err
	}
	return os.Mkdir(p, mode)
}

func routingWriteArtifact(root, rel string, raw []byte) (retErr error) {
	p, err := checkedRoutingPath(root, rel)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	n, err := f.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	return f.Sync()
}

func routingReadArtifact(root, rel string) ([]byte, error) {
	p, err := checkedRoutingPath(root, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func routingExchangeDestination(root, stageRel, destinationRel string, expectedDevice, expectedInode uint64, expectedHash string, expectedMode os.FileMode) error {
	stage, err := checkedRoutingPath(root, stageRel)
	if err != nil {
		return err
	}
	destination, err := checkedRoutingPath(root, destinationRel)
	if err != nil {
		return err
	}
	temporaryBackup := stage + ".exchange-backup"
	if _, err := os.Lstat(temporaryBackup); !errors.Is(err, os.ErrNotExist) {
		return errors.New("unexpected routing exchange backup")
	}
	if err := windowsReplaceFile(destination, stage, temporaryBackup); err != nil {
		return err
	}
	raw, device, inode, mode, links, verifyErr := windowsRoutingArtifactIdentity(temporaryBackup)
	if verifyErr == nil && device == expectedDevice && inode == expectedInode && SHA(raw) == expectedHash && mode.Perm() == expectedMode.Perm() && links == 1 {
		return os.Rename(temporaryBackup, stage)
	}
	// Restore the displaced destination atomically. The current destination
	// (our staged output) becomes stage again as part of the same operation.
	if err := windowsReplaceFile(destination, temporaryBackup, stage); err != nil {
		return fmt.Errorf("%w; exchange-back failed: %v", errRoutingDestinationChanged, err)
	}
	return errRoutingDestinationChanged
}

func windowsReplaceFile(destination, replacement, backup string) error {
	d, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	r, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	ok, _, callErr := replaceFileW.Call(uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(r)), uintptr(unsafe.Pointer(b)), 0, 0, 0)
	if ok == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return errors.New("ReplaceFileW failed")
	}
	return nil
}

func windowsRoutingArtifactIdentity(path string) ([]byte, uint64, uint64, os.FileMode, uint64, error) {
	h, id, err := openWindowsInventoryHandle(path, false)
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	f := os.NewFile(uintptr(h), path)
	raw, readErr := io.ReadAll(io.LimitReader(f, MaxCodexAgentBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > MaxCodexAgentBytes {
		return nil, 0, 0, 0, 0, errors.Join(readErr, closeErr, errors.New("unsafe routing artifact size"))
	}
	return raw, id.volume, id.index, 0o600, uint64(id.links), nil
}

func routingLinkCount(os.FileInfo) uint64 { return 1 }

func checkedRoutingPath(root, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", errors.New("unsafe routing path")
	}
	p := root
	for _, c := range filepath.SplitList(filepath.Clean(rel)) {
		_ = c
	}
	full := filepath.Join(root, rel)
	for p = filepath.Dir(full); p != root; p = filepath.Dir(p) {
		i, err := os.Lstat(p)
		if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("unsafe routing ancestor")
		}
	}
	return full, nil
}
func routingRename(root, a, b string) error {
	x, e := checkedRoutingPath(root, a)
	if e != nil {
		return e
	}
	y, e := checkedRoutingPath(root, b)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(y); e == nil {
		return errors.New("unexpected routing destination")
	}
	return os.Rename(x, y)
}
func routingReplace(root, a, b string) error {
	x, e := checkedRoutingPath(root, a)
	if e != nil {
		return e
	}
	y, e := checkedRoutingPath(root, b)
	if e != nil {
		return e
	}
	i, e := os.Lstat(y)
	if e != nil || !i.Mode().IsRegular() || i.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe routing destination")
	}
	return os.Rename(x, y)
}
func routingUnlink(root, rel string) error {
	p, e := checkedRoutingPath(root, rel)
	if e != nil {
		return e
	}
	return os.Remove(p)
}
