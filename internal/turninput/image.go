package turninput

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxImages = 8
const MaxImageBytes uint64 = 20 * 1024 * 1024
const MaxTotalImageBytes uint64 = 64 * 1024 * 1024
const MaxImagePathBytes = 4096

type ImageFormat string

const (
	ImagePNG  ImageFormat = "png"
	ImageJPEG ImageFormat = "jpeg"
	ImageGIF  ImageFormat = "gif"
	ImageWebP ImageFormat = "webp"
)

type ImageDescriptorV1 struct {
	absolute, display string
	Format            ImageFormat
	Size              uint64
	identity          imageIdentity
	root              *capturedRoot
}

func RevalidateDescriptors(images []ImageDescriptorV1) error {
	for i := range images {
		if images[i].root == nil {
			return imageError(i, "", "image_descriptor_invalid")
		}
		if err := revalidateImage(images[i].root, &images[i]); err != nil {
			return imageError(i, images[i].display, "image_changed")
		}
	}
	return nil
}

func (d ImageDescriptorV1) AbsolutePath() string { return d.absolute }
func (d ImageDescriptorV1) DisplayPath() string  { return d.display }

type ImageBatchV1 struct {
	root   *capturedRoot
	images []ImageDescriptorV1
}

func (b *ImageBatchV1) Images() []ImageDescriptorV1 {
	return append([]ImageDescriptorV1(nil), b.images...)
}
func (b *ImageBatchV1) Close() {
	if b != nil && b.root != nil {
		b.root.close()
		b.root = nil
		b.images = nil
	}
}
func (b *ImageBatchV1) Revalidate() error {
	if b != nil && len(b.images) == 0 {
		return nil
	}
	if b == nil || b.root == nil {
		return errors.New("image_batch_closed")
	}
	for i := range b.images {
		if err := revalidateImage(b.root, &b.images[i]); err != nil {
			return imageError(i, b.images[i].display, "image_changed")
		}
	}
	return nil
}

func ValidateImages(root string, paths []string) (*ImageBatchV1, error) {
	if len(paths) == 0 {
		return &ImageBatchV1{}, nil
	}
	if len(paths) > MaxImages {
		return nil, errors.New("image_count_invalid")
	}
	cr, err := captureRoot(root)
	if err != nil {
		return nil, errors.New("image_root_invalid")
	}
	b := &ImageBatchV1{root: cr}
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
		if p == "" || len(p) > MaxImagePathBytes || !utf8.ValidString(p) || unsafePathText(p) || unsafeTraversal(p) {
			return nil, imageError(i, "", "image_path_invalid")
		}
		d, e := validateImage(cr, p)
		if e != nil {
			return nil, imageError(i, "", e.Error())
		}
		if seenPath[d.absolute] || seenIdentity[d.identity.key()] {
			return nil, imageError(i, d.display, "image_duplicate")
		}
		seenPath[d.absolute] = true
		seenIdentity[d.identity.key()] = true
		if d.Size > MaxImageBytes {
			return nil, imageError(i, d.display, "image_too_large")
		}
		if d.Size > MaxTotalImageBytes || total > MaxTotalImageBytes-d.Size {
			return nil, imageError(i, d.display, "image_total_too_large")
		}
		total += d.Size
		b.images = append(b.images, d)
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
func classify(h []byte) (ImageFormat, error) {
	if len(h) >= 8 && string(h[:8]) == "\x89PNG\r\n\x1a\n" {
		return ImagePNG, nil
	}
	if len(h) >= 4 && h[0] == 0xff && h[1] == 0xd8 && h[2] == 0xff && h[3] != 0 && h[3] != 0xff {
		return ImageJPEG, nil
	}
	if len(h) >= 6 && (string(h[:6]) == "GIF87a" || string(h[:6]) == "GIF89a") {
		return ImageGIF, nil
	}
	if len(h) >= 16 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WEBP" && (string(h[12:16]) == "VP8 " || string(h[12:16]) == "VP8L" || string(h[12:16]) == "VP8X") {
		return ImageWebP, nil
	}
	return "", errors.New("image_unsupported_format")
}
