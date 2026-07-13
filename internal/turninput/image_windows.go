//go:build windows

package turninput

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"strings"
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
func validateImage(r *capturedRoot, raw string) (ImageDescriptorV1, error) {
	abs := raw
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(r.path, raw)
	}
	abs = filepath.Clean(abs)
	rel, e := filepath.Rel(r.path, abs)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == "." {
		return ImageDescriptorV1{}, errors.New("image_outside_project")
	}
	p := r.path
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		p = filepath.Join(p, part)
		dir := p != abs
		h, _, x := openWin(p, dir)
		if x != nil {
			if _, statErr := os.Lstat(p); errors.Is(statErr, os.ErrNotExist) {
				return ImageDescriptorV1{}, errors.New("image_not_found")
			}
			return ImageDescriptorV1{}, errors.New("image_reparse_refused")
		}
		r.handles = append(r.handles, h)
	}
	leaf := r.handles[len(r.handles)-1]
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(leaf, &info) != nil || info.NumberOfLinks == 0 {
		return ImageDescriptorV1{}, errors.New("image_not_regular")
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size == 0 {
		return ImageDescriptorV1{}, errors.New("image_empty")
	}
	if size > MaxImageBytes {
		return ImageDescriptorV1{}, errors.New("image_too_large")
	}
	readHandle, readID, e := openWin(abs, false)
	if e != nil || readID != (imageIdentity{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), size, info.LastWriteTime, info.NumberOfLinks}) {
		if readHandle != windows.InvalidHandle {
			windows.CloseHandle(readHandle)
		}
		return ImageDescriptorV1{}, errors.New("image_changed")
	}
	f := os.NewFile(uintptr(readHandle), abs)
	hbuf := make([]byte, 16)
	n, e := f.ReadAt(hbuf, 0)
	closeErr := f.Close()
	if e != nil && e != io.EOF {
		return ImageDescriptorV1{}, errors.New("image_unreadable")
	}
	if closeErr != nil {
		return ImageDescriptorV1{}, errors.New("image_unreadable")
	}
	format, e := classify(hbuf[:n])
	if e != nil {
		return ImageDescriptorV1{}, e
	}
	id := imageIdentity{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), size, info.LastWriteTime, info.NumberOfLinks}
	return ImageDescriptorV1{absolute: abs, display: filepath.ToSlash(rel), Format: format, Size: size, identity: id, root: r}, nil
}
func revalidateImage(r *capturedRoot, d *ImageDescriptorV1) error {
	h, id, e := openWin(d.absolute, false)
	if e != nil {
		return e
	}
	defer windows.CloseHandle(h)
	if id != d.identity {
		return errors.New("changed")
	}
	buf := make([]byte, 16)
	f := os.NewFile(uintptr(h), d.absolute)
	n, e := f.ReadAt(buf, 0)
	f.Close()
	if e != nil && e != io.EOF {
		return e
	}
	format, e := classify(buf[:n])
	if e != nil || format != d.Format {
		return errors.New("changed")
	}
	return nil
}
