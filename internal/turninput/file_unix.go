//go:build unix

package turninput

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type imageIdentity struct {
	dev, ino, size, nlink uint64
	mtime, ctime          unix.Timespec
	mode                  uint32
	ancestors             string
}

func (i imageIdentity) key() string { return fmt.Sprintf("%d:%d", i.dev, i.ino) }

type capturedRoot struct {
	path     string
	fd       int
	identity imageIdentity
}

func captureRoot(root string) (*capturedRoot, error) {
	p, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	fd, e := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		unix.Close(fd)
		return nil, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return nil, errors.New("root not directory")
	}
	return &capturedRoot{p, fd, unixIdentity(&st, "")}, nil
}

func (r *capturedRoot) close() {
	if r.fd >= 0 {
		unix.Close(r.fd)
		r.fd = -1
	}
}

// validateFile walks the path one component at a time with openat +
// O_NOFOLLOW from the captured root fd, so no symlink anywhere in the chain
// can escape the project, then fstats the leaf, sniffs a bounded prefix for
// the content kind, and re-fstats to prove the file did not change between
// the first stat and the read (TOCTOU).
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
	parts := strings.Split(filepath.ToSlash(rel), "/")
	fd, e := unix.Dup(r.fd)
	if e != nil {
		return FileDescriptorV1{}, e
	}
	ancestorKey := r.identity.rootKey()
	current := r.path
	for _, p := range parts[:len(parts)-1] {
		current = filepath.Join(current, p)
		next, x := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if x != nil {
			return FileDescriptorV1{}, classifyUnixPathFailure(current)
		}
		var dst unix.Stat_t
		if unix.Fstat(next, &dst) != nil || dst.Mode&unix.S_IFMT != unix.S_IFDIR {
			unix.Close(next)
			return FileDescriptorV1{}, errors.New("not_regular")
		}
		ancestorKey += "/" + unixIdentity(&dst, "").keyFull()
		fd = next
	}
	leafPath := filepath.Join(current, parts[len(parts)-1])
	leaf, e := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	unix.Close(fd)
	if e != nil {
		return FileDescriptorV1{}, classifyUnixPathFailure(leafPath)
	}
	defer unix.Close(leaf)
	var st unix.Stat_t
	if unix.Fstat(leaf, &st) != nil {
		return FileDescriptorV1{}, errors.New("unreadable")
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return FileDescriptorV1{}, errors.New("not_regular")
	}
	if st.Mode&0o444 == 0 {
		return FileDescriptorV1{}, errors.New("unreadable")
	}
	if st.Size == 0 {
		return FileDescriptorV1{}, errors.New("empty")
	}
	if uint64(st.Size) > maxBytes {
		return FileDescriptorV1{}, errors.New("too_large")
	}
	h := make([]byte, sniffBytes)
	n, e := unix.Pread(leaf, h, 0)
	if e != nil {
		return FileDescriptorV1{}, errors.New("unreadable")
	}
	kind, format := classifyContent(h[:n], uint64(n) < uint64(st.Size))
	var after unix.Stat_t
	if unix.Fstat(leaf, &after) != nil || unixIdentity(&st, "") != unixIdentity(&after, "") {
		return FileDescriptorV1{}, errors.New("changed")
	}
	id := unixIdentity(&st, ancestorKey)
	return FileDescriptorV1{absolute: abs, display: filepath.ToSlash(rel), Kind: kind, Format: format, Size: uint64(st.Size), limit: maxBytes, identity: id, root: r}, nil
}

// revalidateFile re-pins the project root, then re-runs the full validation
// and refuses if the file's identity, kind, or format moved.
func revalidateFile(r *capturedRoot, d *FileDescriptorV1) error {
	fd, e := unix.Open(r.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return e
	}
	var rootStat unix.Stat_t
	e = unix.Fstat(fd, &rootStat)
	unix.Close(fd)
	if e != nil || unixIdentity(&rootStat, "").rootKey() != r.identity.rootKey() {
		return errors.New("changed")
	}
	now, e := validateFile(r, d.absolute, d.revalidationLimit())
	if e != nil {
		return e
	}
	if now.identity != d.identity || now.Format != d.Format || now.Kind != d.Kind {
		return errors.New("changed")
	}
	return nil
}

func unixIdentity(st *unix.Stat_t, ancestors string) imageIdentity {
	return imageIdentity{dev: uint64(st.Dev), ino: st.Ino, size: uint64(st.Size), nlink: uint64(st.Nlink), mtime: st.Mtim, ctime: st.Ctim, mode: uint32(st.Mode), ancestors: ancestors}
}

func (i imageIdentity) keyFull() string {
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d:%d", i.dev, i.ino, i.size, i.nlink, i.mode, i.mtime.Sec, i.mtime.Nsec, i.ctime.Sec, i.ctime.Nsec)
}

// rootKey pins the directory object without treating ordinary child updates
// as replacement of the captured project root.
func (i imageIdentity) rootKey() string {
	return fmt.Sprintf("%d:%d:%d", i.dev, i.ino, i.mode)
}

func classifyUnixPathFailure(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("not_found")
	}
	if err != nil {
		return errors.New("unreadable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("link_refused")
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return errors.New("not_regular")
	}
	return errors.New("unreadable")
}
