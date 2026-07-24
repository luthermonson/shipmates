//go:build windows

package turninput

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type imageIdentity struct {
	volume, index, size uint64
	write               windows.Filetime
	links               uint32
}

func (i imageIdentity) key() string { return fmt.Sprintf("%d:%d", i.volume, i.index) }

type capturedRoot struct {
	path     string
	handles  []windows.Handle
	identity imageIdentity
}

// openWin opens one path component with FILE_FLAG_OPEN_REPARSE_POINT so a
// junction/symlink is opened as the reparse point itself rather than
// followed, then refuses it outright — that is how the Windows path
// enforces "no links anywhere in the chain". It also refuses a
// file-where-directory-expected (and vice versa).
func openWin(path string, dir bool) (windows.Handle, imageIdentity, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return windows.InvalidHandle, imageIdentity{}, e
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if dir {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, e := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if e != nil {
		return windows.InvalidHandle, imageIdentity{}, e
	}
	var i windows.ByHandleFileInformation
	if e = windows.GetFileInformationByHandle(h, &i); e != nil || i.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (i.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != dir {
		windows.CloseHandle(h)
		return windows.InvalidHandle, imageIdentity{}, errors.New("unsafe")
	}
	return h, imageIdentity{uint64(i.VolumeSerialNumber), uint64(i.FileIndexHigh)<<32 | uint64(i.FileIndexLow), uint64(i.FileSizeHigh)<<32 | uint64(i.FileSizeLow), i.LastWriteTime, i.NumberOfLinks}, nil
}

func captureRoot(root string) (*capturedRoot, error) {
	p, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	h, id, e := openWin(p, true)
	if e != nil {
		return nil, e
	}
	return &capturedRoot{p, []windows.Handle{h}, id}, nil
}

func (r *capturedRoot) close() {
	for _, h := range r.handles {
		windows.CloseHandle(h)
	}
	r.handles = nil
}

// validateFile walks each path component from the captured root with
// reparse-point refusal, holds every component handle open for the batch's
// lifetime so the chain cannot be swapped, then re-opens the leaf by
// absolute path and requires the two handles to name the same file object
// before reading the sniff prefix (TOCTOU).
func validateFile(r *capturedRoot, raw string, maxBytes uint64) (FileDescriptorV1, error) {
	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.path, raw)
	}
	abs = filepath.Clean(abs)
	rel, e := filepath.Rel(r.path, abs)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == "." {
		return FileDescriptorV1{}, errors.New("outside_project")
	}
	p := r.path
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		p = filepath.Join(p, part)
		dir := p != abs
		h, _, x := openWin(p, dir)
		if x != nil {
			if _, statErr := os.Lstat(p); errors.Is(statErr, os.ErrNotExist) {
				return FileDescriptorV1{}, errors.New("not_found")
			}
			return FileDescriptorV1{}, errors.New("reparse_refused")
		}
		r.handles = append(r.handles, h)
	}
	leaf := r.handles[len(r.handles)-1]
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(leaf, &info) != nil || info.NumberOfLinks == 0 {
		return FileDescriptorV1{}, errors.New("not_regular")
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size == 0 {
		return FileDescriptorV1{}, errors.New("empty")
	}
	if size > maxBytes {
		return FileDescriptorV1{}, errors.New("too_large")
	}
	id := imageIdentity{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), size, info.LastWriteTime, info.NumberOfLinks}
	readHandle, readID, e := openWin(abs, false)
	if e != nil || readID != id {
		if readHandle != windows.InvalidHandle {
			windows.CloseHandle(readHandle)
		}
		return FileDescriptorV1{}, errors.New("changed")
	}
	head, e := sniffHandle(readHandle, abs)
	if e != nil {
		return FileDescriptorV1{}, e
	}
	kind, format := classifyContent(head, uint64(len(head)) < size)
	return FileDescriptorV1{absolute: abs, display: filepath.ToSlash(rel), Kind: kind, Format: format, Size: size, limit: maxBytes, identity: id, root: r}, nil
}

// revalidateFile re-opens the leaf by absolute path (reparse points still
// refused) and requires identity, kind, and format to be unchanged.
func revalidateFile(_ *capturedRoot, d *FileDescriptorV1) error {
	h, id, e := openWin(d.absolute, false)
	if e != nil {
		return e
	}
	if id != d.identity {
		windows.CloseHandle(h)
		return errors.New("changed")
	}
	if id.size > d.revalidationLimit() {
		windows.CloseHandle(h)
		return errors.New("changed")
	}
	head, e := sniffHandle(h, d.absolute)
	if e != nil {
		return e
	}
	kind, format := classifyContent(head, uint64(len(head)) < id.size)
	if kind != d.Kind || format != d.Format {
		return errors.New("changed")
	}
	return nil
}

// sniffHandle takes ownership of h, reads the bounded classification prefix,
// and always releases the handle exactly once (os.File owns it after
// NewFile, so the raw handle must not be closed separately).
func sniffHandle(h windows.Handle, name string) ([]byte, error) {
	f := os.NewFile(uintptr(h), name)
	if f == nil {
		windows.CloseHandle(h)
		return nil, errors.New("unreadable")
	}
	buf := make([]byte, sniffBytes)
	n, readErr := f.ReadAt(buf, 0)
	closeErr := f.Close()
	if (readErr != nil && readErr != io.EOF) || closeErr != nil {
		return nil, errors.New("unreadable")
	}
	return buf[:n], nil
}
