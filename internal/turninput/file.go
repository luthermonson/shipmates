// Package turninput validates local files an operator attaches to a model
// turn. Validation is deliberately paranoid: every path is confined to the
// captured project root, symlinks and Windows reparse points are refused,
// only regular files are accepted, sizes are capped per file and per batch,
// and every descriptor can be revalidated (open → fstat → compare identity)
// immediately before the bytes are handed to a runtime, so a file swapped
// between validation and use is rejected rather than sent.
//
// Content kind is sniffed from the bytes, never inferred from the file
// extension: raster images by magic header, text by "valid UTF-8 with no
// NUL in a bounded prefix", everything else binary.
package turninput

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Batch and per-file limits. Images keep the historical 20 MiB allowance
// (a phone screenshot is routinely several MiB); other files are capped
// lower because they are inlined into a prompt rather than uploaded.
const (
	// MaxFiles bounds one attachment batch.
	MaxFiles = 8
	// MaxImages is the historical spelling of MaxFiles.
	MaxImages = MaxFiles
	// MaxImageBytes caps a single image.
	MaxImageBytes uint64 = 20 * 1024 * 1024
	// MaxFileBytes caps a single non-image file.
	MaxFileBytes uint64 = 10 * 1024 * 1024
	// MaxTotalImageBytes caps the whole batch.
	MaxTotalImageBytes uint64 = 64 * 1024 * 1024
	// MaxTotalFileBytes is the historical spelling of MaxTotalImageBytes.
	MaxTotalFileBytes = MaxTotalImageBytes
	// MaxImagePathBytes caps an operator-supplied path.
	MaxImagePathBytes = 4096
	// MaxFilePathBytes is the historical spelling of MaxImagePathBytes.
	MaxFilePathBytes = MaxImagePathBytes
	// sniffBytes is the bounded prefix read for kind classification. Large
	// enough that a text file misdetects only if its first 8 KiB are
	// pathological, small enough that classification never reads a whole
	// multi-megabyte file.
	sniffBytes = 8 * 1024
)

// ImageFormat is the sniffed raster format of an image attachment.
type ImageFormat string

const (
	ImagePNG  ImageFormat = "png"
	ImageJPEG ImageFormat = "jpeg"
	ImageGIF  ImageFormat = "gif"
	ImageWebP ImageFormat = "webp"
)

// FileKind is the content classification of an attachment.
type FileKind string

const (
	// FileImage is a raster image in one of the sniffed formats.
	FileImage FileKind = "image"
	// FileText is valid UTF-8 with no NUL bytes in the sniffed prefix.
	FileText FileKind = "text"
	// FileBinary is everything else — including PDFs, archives, and
	// executables. Binary attachments are never inlined into a prompt.
	FileBinary FileKind = "binary"
)

// MediaType returns the IANA media type for an image format, or "" for a
// non-image descriptor. Runtimes that encode images as base64 content
// blocks need this.
func (f ImageFormat) MediaType() string {
	switch f {
	case ImagePNG:
		return "image/png"
	case ImageJPEG:
		return "image/jpeg"
	case ImageGIF:
		return "image/gif"
	case ImageWebP:
		return "image/webp"
	}
	return ""
}

// FileDescriptorV1 is a validated attachment: an absolute in-root path, the
// project-relative display path, the sniffed kind, the size, and the
// captured content identity used to detect replacement at revalidation
// time. Constructed only by ValidateFiles / ValidateImages.
type FileDescriptorV1 struct {
	absolute, display string
	// Kind is the sniffed content classification.
	Kind FileKind
	// Format is the raster format; empty unless Kind is FileImage.
	Format ImageFormat
	// Size is the file size in bytes at validation time.
	Size uint64
	// limit is the per-file byte cap applied when this descriptor was
	// validated; revalidation reapplies exactly the same cap.
	limit    uint64
	identity imageIdentity
	root     *capturedRoot
}

// ImageDescriptorV1 is the historical image-only spelling of
// FileDescriptorV1. Kept as an alias so existing call sites (codexapp turn
// input, livesession, the local server) compile unchanged.
type ImageDescriptorV1 = FileDescriptorV1

// AbsolutePath is the validated absolute path inside the project root.
func (d FileDescriptorV1) AbsolutePath() string { return d.absolute }

// DisplayPath is the slash-normalized project-relative path. It is the only
// form safe to show an operator or put in a prompt.
func (d FileDescriptorV1) DisplayPath() string { return d.display }

// IsImage reports whether this attachment sniffed as a raster image.
func (d FileDescriptorV1) IsImage() bool { return d.Kind == FileImage }

// revalidationLimit is the per-file byte cap to reapply on revalidation.
// Descriptors minted before the cap was recorded fall back to the image cap.
func (d FileDescriptorV1) revalidationLimit() uint64 {
	if d.limit == 0 {
		return MaxImageBytes
	}
	return d.limit
}

// RevalidateDescriptors re-opens every descriptor and refuses the batch if
// any file's identity, kind, or format changed since validation. Runtimes
// must call this immediately before reading the bytes.
func RevalidateDescriptors(files []FileDescriptorV1) error {
	for i := range files {
		if files[i].root == nil {
			return imageError(i, "", "image_descriptor_invalid")
		}
		if err := revalidateFile(files[i].root, &files[i]); err != nil {
			return imageError(i, files[i].display, "image_changed")
		}
	}
	return nil
}

// FileBatchV1 owns the captured project root for a set of descriptors. The
// root handle is held open for the batch's lifetime so the directory cannot
// be swapped underneath the descriptors; Close releases it.
type FileBatchV1 struct {
	root  *capturedRoot
	files []FileDescriptorV1
	// prefix is the error-code namespace ("image_" or "file_") this batch
	// was validated under, so Revalidate reports in the same vocabulary.
	prefix string
}

// ImageBatchV1 is the historical image-only spelling of FileBatchV1.
type ImageBatchV1 = FileBatchV1

// Files returns a copy of the validated descriptors, in the order supplied.
func (b *FileBatchV1) Files() []FileDescriptorV1 {
	return append([]FileDescriptorV1(nil), b.files...)
}

// Images returns a copy of the validated descriptors. For a batch produced
// by ValidateImages every entry is an image.
func (b *FileBatchV1) Images() []ImageDescriptorV1 {
	return append([]ImageDescriptorV1(nil), b.files...)
}

// Close releases the captured root. Safe on a zero/nil batch.
func (b *FileBatchV1) Close() {
	if b != nil && b.root != nil {
		b.root.close()
		b.root = nil
		b.files = nil
	}
}

// Revalidate re-checks every descriptor against the filesystem.
func (b *FileBatchV1) Revalidate() error {
	if b != nil && len(b.files) == 0 {
		return nil
	}
	prefix := "image_"
	if b != nil && b.prefix != "" {
		prefix = b.prefix
	}
	if b == nil || b.root == nil {
		return errors.New(prefix + "batch_closed")
	}
	for i := range b.files {
		if err := revalidateFile(b.root, &b.files[i]); err != nil {
			return imageError(i, b.files[i].display, prefix+"changed")
		}
	}
	return nil
}

// ValidateImages validates a batch that must be entirely raster images.
// Non-image content is refused with image_unsupported_format.
func ValidateImages(root string, paths []string) (*ImageBatchV1, error) {
	return validateBatch(root, paths, "image_", true)
}

// ValidateFiles validates a batch of arbitrary files. Every path gets the
// same confinement, symlink/reparse, regular-file, and size checks as an
// image; the only difference is that non-image content is accepted and
// classified as text or binary, and non-images take the lower MaxFileBytes
// cap.
func ValidateFiles(root string, paths []string) (*FileBatchV1, error) {
	return validateBatch(root, paths, "file_", false)
}

func validateBatch(root string, paths []string, prefix string, imagesOnly bool) (*FileBatchV1, error) {
	if len(paths) == 0 {
		return &FileBatchV1{prefix: prefix}, nil
	}
	if len(paths) > MaxFiles {
		return nil, errors.New(prefix + "count_invalid")
	}
	cr, err := captureRoot(root)
	if err != nil {
		return nil, errors.New(prefix + "root_invalid")
	}
	b := &FileBatchV1{root: cr, prefix: prefix}
	ok := false
	defer func() {
		if !ok {
			b.Close()
		}
	}()
	seenPath := map[string]bool{}
	seenIdentity := map[string]bool{}
	var total uint64
	for i, p := range paths {
		if p == "" || len(p) > MaxFilePathBytes || !utf8.ValidString(p) || unsafePathText(p) || unsafeTraversal(p) {
			return nil, imageError(i, "", prefix+"path_invalid")
		}
		// The per-file open enforces the largest cap either kind may take;
		// the kind-specific cap is applied below, once the bytes have been
		// sniffed.
		d, e := validateFile(cr, p, MaxImageBytes)
		if e != nil {
			return nil, imageError(i, "", prefix+e.Error())
		}
		if seenPath[d.absolute] || seenIdentity[d.identity.key()] {
			return nil, imageError(i, d.display, prefix+"duplicate")
		}
		seenPath[d.absolute] = true
		seenIdentity[d.identity.key()] = true
		if imagesOnly && d.Kind != FileImage {
			return nil, imageError(i, d.display, "image_unsupported_format")
		}
		limit := MaxImageBytes
		if d.Kind != FileImage {
			limit = MaxFileBytes
		}
		if d.Size > limit {
			return nil, imageError(i, d.display, prefix+"too_large")
		}
		if d.Size > MaxTotalFileBytes || total > MaxTotalFileBytes-d.Size {
			return nil, imageError(i, d.display, prefix+"total_too_large")
		}
		total += d.Size
		b.files = append(b.files, d)
	}
	ok = true
	return b, nil
}

func unsafeTraversal(s string) bool {
	if filepath.Clean(s) == "." {
		return true
	}
	for _, component := range strings.FieldsFunc(filepath.ToSlash(s), func(r rune) bool { return r == '/' }) {
		if component == ".." || component == "." {
			return true
		}
	}
	return false
}

func imageError(i int, display, code string) error {
	if display != "" {
		return fmt.Errorf("%s:%d:%s", code, i, display)
	}
	return fmt.Errorf("%s:%d", code, i)
}

func unsafePathText(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return true
		}
	}
	return false
}

// classifyImage sniffs a raster format from the file's magic header. An
// empty result means "not one of the supported image formats".
func classifyImage(h []byte) ImageFormat {
	if len(h) >= 8 && string(h[:8]) == "\x89PNG\r\n\x1a\n" {
		return ImagePNG
	}
	if len(h) >= 4 && h[0] == 0xff && h[1] == 0xd8 && h[2] == 0xff && h[3] != 0 && h[3] != 0xff {
		return ImageJPEG
	}
	if len(h) >= 6 && (string(h[:6]) == "GIF87a" || string(h[:6]) == "GIF89a") {
		return ImageGIF
	}
	if len(h) >= 16 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WEBP" && (string(h[12:16]) == "VP8 " || string(h[12:16]) == "VP8L" || string(h[12:16]) == "VP8X") {
		return ImageWebP
	}
	return ""
}

// classifyContent maps a bounded prefix onto a FileKind. truncated says the
// prefix stopped short of the file's end, in which case a trailing partial
// rune is not treated as invalid UTF-8.
func classifyContent(head []byte, truncated bool) (FileKind, ImageFormat) {
	if format := classifyImage(head); format != "" {
		return FileImage, format
	}
	probe := head
	if truncated {
		probe = trimPartialRune(probe)
	}
	for _, b := range probe {
		if b == 0 {
			return FileBinary, ""
		}
	}
	if !utf8.Valid(probe) {
		return FileBinary, ""
	}
	return FileText, ""
}

// trimPartialRune drops a trailing UTF-8 sequence cut off by the sniff
// boundary so a legitimate text file is not misread as binary.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && i < len(b); i++ {
		tail := b[len(b)-1-i:]
		r, size := utf8.DecodeRune(tail)
		if r != utf8.RuneError || size > 1 {
			// The last rune decodes cleanly; nothing to trim.
			return b
		}
		if !utf8.RuneStart(tail[0]) {
			continue
		}
		// tail[0] starts a rune that did not complete within the prefix.
		return b[:len(b)-1-i]
	}
	return b
}
