package turninput

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeFileFixture(t *testing.T, root, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateFilesClassifiesImageTextAndBinary(t *testing.T) {
	root := t.TempDir()
	writeFileFixture(t, root, "shot.png", append(append([]byte(nil), imageHeaders[ImagePNG]...), []byte("trailing")...))
	writeFileFixture(t, root, "notes.md", []byte("# heading\n\nplain UTF-8 text with é and 雪\n"))
	writeFileFixture(t, root, "blob.bin", []byte{0x01, 0x02, 0x00, 0x03, 0xff})
	// A PDF is deliberately classified binary: it has no image magic and
	// carries NUL bytes in its object streams.
	writeFileFixture(t, root, "doc.pdf", append([]byte("%PDF-1.7\n"), 0x00, 0x01, 0x02))

	batch, err := ValidateFiles(root, []string{"shot.png", "notes.md", "blob.bin", "doc.pdf"})
	if err != nil {
		t.Fatalf("ValidateFiles: %v", err)
	}
	defer batch.Close()
	got := batch.Files()
	if len(got) != 4 {
		t.Fatalf("descriptors=%d want 4", len(got))
	}
	want := []struct {
		display string
		kind    FileKind
		format  ImageFormat
	}{
		{"shot.png", FileImage, ImagePNG},
		{"notes.md", FileText, ""},
		{"blob.bin", FileBinary, ""},
		{"doc.pdf", FileBinary, ""},
	}
	for i, w := range want {
		if got[i].DisplayPath() != w.display || got[i].Kind != w.kind || got[i].Format != w.format {
			t.Errorf("descriptor %d = %s/%s/%s want %s/%s/%s", i, got[i].DisplayPath(), got[i].Kind, got[i].Format, w.display, w.kind, w.format)
		}
		if !filepath.IsAbs(got[i].AbsolutePath()) {
			t.Errorf("descriptor %d absolute path %q is not absolute", i, got[i].AbsolutePath())
		}
	}
	if !got[0].IsImage() || got[1].IsImage() {
		t.Errorf("IsImage misreported: %v %v", got[0].IsImage(), got[1].IsImage())
	}
	if got[0].Format.MediaType() != "image/png" || got[1].Format.MediaType() != "" {
		t.Errorf("media types = %q %q", got[0].Format.MediaType(), got[1].Format.MediaType())
	}
	if err := batch.Revalidate(); err != nil {
		t.Fatalf("Revalidate: %v", err)
	}
}

func TestValidateFilesRejectsNonImageOverFileCapAndKeepsImageCap(t *testing.T) {
	root := t.TempDir()
	// A text file just over MaxFileBytes is refused even though it is well
	// under the image cap.
	big := filepath.Join(root, "big.txt")
	f, err := os.OpenFile(big, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("text")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileBytes) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFiles(root, []string{"big.txt"}); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("oversized text err=%v want file_too_large", err)
	}
	// An image of the same size is accepted: images keep the larger cap.
	img := writeImageFixture(t, root, "big.png", imageHeaders[ImagePNG], int64(MaxFileBytes)+1)
	batch, err := ValidateFiles(root, []string{img})
	if err != nil {
		t.Fatalf("oversized image err=%v want accepted", err)
	}
	batch.Close()
}

func TestValidateFilesPreservesConfinementAndSanitization(t *testing.T) {
	root := t.TempDir()
	outside := writeFileFixture(t, t.TempDir(), "outside.txt", []byte("secret-outside-canary"))
	_, err := ValidateFiles(root, []string{outside})
	if err == nil || !strings.Contains(err.Error(), "file_outside_project") {
		t.Fatalf("outside err=%v want file_outside_project", err)
	}
	for _, raw := range []string{"", ".", "../x.txt", "bad\n.txt", "bad‮.txt", strings.Repeat("x", MaxFilePathBytes+1)} {
		if _, err := ValidateFiles(root, []string{raw}); err == nil || !strings.Contains(err.Error(), "file_path_invalid") {
			t.Fatalf("path %q err=%v want file_path_invalid", raw, err)
		}
	}
	if _, err := ValidateFiles(root, make([]string, MaxFiles+1)); err == nil || !strings.Contains(err.Error(), "file_count_invalid") {
		t.Fatalf("overlong batch err=%v want file_count_invalid", err)
	}
	writeFileFixture(t, root, "dup.txt", []byte("dup"))
	if _, err := ValidateFiles(root, []string{"dup.txt", "dup.txt"}); err == nil || !strings.Contains(err.Error(), "file_duplicate") {
		t.Fatalf("duplicate err=%v want file_duplicate", err)
	}
	writeFileFixture(t, root, "empty.txt", nil)
	if _, err := ValidateFiles(root, []string{"empty.txt"}); err == nil || !strings.Contains(err.Error(), "file_empty") {
		t.Fatalf("empty err=%v want file_empty", err)
	}
	if _, err := ValidateFiles(root, []string{"missing.txt"}); err == nil || !strings.Contains(err.Error(), "file_not_found") {
		t.Fatalf("missing err=%v want file_not_found", err)
	}
}

func TestValidateImagesStillRefusesNonImages(t *testing.T) {
	root := t.TempDir()
	writeFileFixture(t, root, "notes.md", []byte("plain text, definitely not a raster image"))
	_, err := ValidateImages(root, []string{"notes.md"})
	if err == nil || !strings.Contains(err.Error(), "image_unsupported_format") {
		t.Fatalf("text-as-image err=%v want image_unsupported_format", err)
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("error leaked project root: %v", err)
	}
}

func TestFileRevalidateRefusesReplacedContent(t *testing.T) {
	root := t.TempDir()
	writeFileFixture(t, root, "notes.md", []byte("original text"))
	batch, err := ValidateFiles(root, []string{"notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if err := batch.Revalidate(); err != nil {
		t.Fatalf("clean revalidate: %v", err)
	}
	// Replace the file with binary content of a different length: identity
	// (size + write time) and kind both move, so revalidation must refuse.
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte{0x00, 0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := batch.Revalidate(); err == nil {
		t.Fatal("replaced content accepted")
	}
	if err := RevalidateDescriptors(batch.Files()); err == nil {
		t.Fatal("RevalidateDescriptors accepted replaced content")
	}
}

func TestFileBytesReadsAndRefusesSwappedContent(t *testing.T) {
	root := t.TempDir()
	body := []byte("attachment body with é and 雪\n")
	writeFileFixture(t, root, "notes.md", body)
	batch, err := ValidateFiles(root, []string{"notes.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	d := batch.Files()[0]
	raw, err := d.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(raw, body) {
		t.Fatalf("Bytes=%q want %q", raw, body)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("swapped for something else entirely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Bytes(); err == nil {
		t.Fatal("Bytes accepted swapped content")
	}
	var zero FileDescriptorV1
	if _, err := zero.Bytes(); err == nil {
		t.Fatal("Bytes accepted an unvalidated descriptor")
	}
}

func TestClassifyContentBoundedPrefix(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		truncated bool
		kind      FileKind
	}{
		{"ascii", []byte("hello"), false, FileText},
		{"utf8", []byte("héllo 雪"), false, FileText},
		{"nul", []byte("he\x00llo"), false, FileBinary},
		{"invalid utf8", []byte{0xff, 0xfe, 0x41}, false, FileBinary},
		{"png", imageHeaders[ImagePNG], false, FileImage},
		{"tab and newline are text", []byte("a\tb\r\nc"), false, FileText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, _ := classifyContent(tc.body, tc.truncated)
			if kind != tc.kind {
				t.Fatalf("kind=%s want %s", kind, tc.kind)
			}
		})
	}
	// A multi-byte rune straddling the sniff boundary must not make a text
	// file look binary.
	snow := []byte("雪")
	body := append(bytes.Repeat([]byte("a"), 5), snow[:2]...)
	if kind, _ := classifyContent(body, true); kind != FileText {
		t.Fatalf("straddling rune kind=%s want text", kind)
	}
	if kind, _ := classifyContent(body, false); kind != FileBinary {
		t.Fatalf("complete file with invalid tail kind=%s want binary", kind)
	}
}

func TestValidateFilesSniffsRealFileStraddlingPrefixBoundary(t *testing.T) {
	root := t.TempDir()
	body := append(bytes.Repeat([]byte("x"), sniffBytes-1), []byte("雪 tail")...)
	if !utf8.Valid(body) {
		t.Fatal("fixture is not valid UTF-8")
	}
	writeFileFixture(t, root, "long.txt", body)
	batch, err := ValidateFiles(root, []string{"long.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if got := batch.Files()[0].Kind; got != FileText {
		t.Fatalf("kind=%s want text", got)
	}
}
