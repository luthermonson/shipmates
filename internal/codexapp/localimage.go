package codexapp

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxLocalImageBytes caps a single attachment. The app-server reads the file
// itself and forwards it to the provider, so this is a guard on what shipmates
// is willing to hand over rather than a protocol limit.
const MaxLocalImageBytes uint64 = 10 << 20

// localImageSniffBytes is enough for every magic number in imageMediaTypes,
// including the 12-byte RIFF/WEBP pair.
const localImageSniffBytes = 16

// LocalImage is a validated image attachment the app-server can read off disk
// (Codex's "localImage" content item). The backend, not shipmates, opens the
// file, so what shipmates can promise is only that the path named a regular
// image file of exactly this size and format a moment before the turn was sent —
// which is why RevalidateLocalImages runs inside StartTurn rather than at
// construction time.
type LocalImage struct {
	// Path is the absolute, symlink-resolved path handed to the backend.
	Path string
	// DisplayPath is the project-relative, slash-normalized form. It is the only
	// spelling safe to show an operator, and the only one that appears in errors.
	DisplayPath string
	// MediaType is the IANA media type detected from the file's magic bytes.
	MediaType string
	// Size is the file size in bytes at validation time.
	Size uint64
}

var imageMediaTypes = []struct {
	prefix    []byte
	offset    int
	suffixAt  int
	suffix    []byte
	mediaType string
}{
	{prefix: []byte("\x89PNG\r\n\x1a\n"), mediaType: "image/png"},
	{prefix: []byte{0xFF, 0xD8, 0xFF}, mediaType: "image/jpeg"},
	{prefix: []byte("GIF87a"), mediaType: "image/gif"},
	{prefix: []byte("GIF89a"), mediaType: "image/gif"},
	{prefix: []byte("RIFF"), suffixAt: 8, suffix: []byte("WEBP"), mediaType: "image/webp"},
}

// sniffImageMediaType classifies a bounded prefix of file content. Extension is
// deliberately ignored: the backend will read the bytes, so the bytes decide.
func sniffImageMediaType(prefix []byte) string {
	for _, candidate := range imageMediaTypes {
		if !bytes.HasPrefix(prefix, candidate.prefix) {
			continue
		}
		if len(candidate.suffix) > 0 {
			end := candidate.suffixAt + len(candidate.suffix)
			if len(prefix) < end || !bytes.Equal(prefix[candidate.suffixAt:end], candidate.suffix) {
				continue
			}
		}
		return candidate.mediaType
	}
	return ""
}

// ErrLocalImageInvalid is returned when an attachment cannot be handed to the
// backend. It carries the project-relative path and a stable reason code, never
// the absolute path or file content.
type ErrLocalImageInvalid struct {
	DisplayPath string
	Reason      string
}

func (e *ErrLocalImageInvalid) Error() string {
	if e.DisplayPath == "" {
		return "local image: " + e.Reason
	}
	return "local image " + e.DisplayPath + ": " + e.Reason
}

// NewLocalImage validates path as an image attachment rooted inside root and
// returns the descriptor to carry on a TurnInput.
//
// root is required and must be absolute: without it there is no way to produce a
// DisplayPath, and no way to refuse a path that escaped the project. Symlinks
// are resolved before the containment check so a link inside the project cannot
// be used to reach a file outside it.
func NewLocalImage(root, path string) (LocalImage, error) {
	if root == "" || !filepath.IsAbs(root) {
		return LocalImage{}, &ErrLocalImageInvalid{Reason: "project root is not absolute"}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return LocalImage{}, &ErrLocalImageInvalid{Reason: "project root unreadable"}
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(resolvedRoot, absolute)
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return LocalImage{}, &ErrLocalImageInvalid{DisplayPath: filepath.ToSlash(path), Reason: "unreadable"}
	}
	relative, err := filepath.Rel(resolvedRoot, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return LocalImage{}, &ErrLocalImageInvalid{DisplayPath: filepath.ToSlash(path), Reason: "outside project"}
	}
	display := filepath.ToSlash(relative)
	image := LocalImage{Path: absolute, DisplayPath: display}
	mediaType, size, err := inspectLocalImage(absolute)
	if err != nil {
		return LocalImage{}, &ErrLocalImageInvalid{DisplayPath: display, Reason: err.Error()}
	}
	image.MediaType, image.Size = mediaType, size
	return image, nil
}

// RevalidateLocalImages re-inspects every attachment and refuses the whole batch
// if any file's size or detected format changed since it was validated.
// StartTurn calls this immediately before the paths go on the wire.
func RevalidateLocalImages(images []LocalImage) error {
	for i := range images {
		image := images[i]
		if image.Path == "" || !filepath.IsAbs(image.Path) || image.MediaType == "" {
			return &ErrLocalImageInvalid{DisplayPath: image.DisplayPath, Reason: "descriptor invalid"}
		}
		mediaType, size, err := inspectLocalImage(image.Path)
		if err != nil {
			return &ErrLocalImageInvalid{DisplayPath: image.DisplayPath, Reason: err.Error()}
		}
		if mediaType != image.MediaType || size != image.Size {
			return &ErrLocalImageInvalid{DisplayPath: image.DisplayPath, Reason: "changed"}
		}
	}
	return nil
}

// inspectLocalImage opens path once, confirms it is a regular file within the
// size cap, and classifies its leading bytes.
//
// The single os.Open is deliberate. os.File owns the underlying descriptor or
// handle from the moment it exists, so releasing it exactly once — via f.Close
// and nothing else — is the whole discipline. A previous version of the
// equivalent Windows code paired a raw CloseHandle with an os.File wrapping the
// same handle and closed it twice; Windows recycles handle values aggressively,
// so the second close can land on an unrelated object opened in between.
func inspectLocalImage(path string) (string, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, errors.New("unreadable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, errors.New("unreadable")
	}
	// Stat through the open file, not the path: this describes the object the
	// backend will be pointed at, and a regular-file check here also rejects the
	// directories and devices a path-based check could be raced into accepting.
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("not a regular file")
	}
	size := info.Size()
	if size <= 0 {
		return "", 0, errors.New("empty")
	}
	if uint64(size) > MaxLocalImageBytes {
		return "", 0, errors.New("too large")
	}
	prefix := make([]byte, localImageSniffBytes)
	n, err := io.ReadFull(f, prefix)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", 0, errors.New("unreadable")
	}
	mediaType := sniffImageMediaType(prefix[:n])
	if mediaType == "" {
		return "", 0, errors.New("not an image")
	}
	return mediaType, uint64(size), nil
}
