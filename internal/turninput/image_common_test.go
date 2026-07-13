package turninput

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var imageHeaders = map[ImageFormat][]byte{
	ImagePNG:  {0x89, 'P', 'N', 'G', 13, 10, 26, 10},
	ImageJPEG: {0xff, 0xd8, 0xff, 0xe0},
	ImageGIF:  []byte("GIF89a"),
	ImageWebP: []byte("RIFF0000WEBPVP8X"),
}

func writeImageFixture(t *testing.T, root, name string, header []byte, extra int64) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(header); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if extra > 0 {
		if err = f.Truncate(int64(len(header)) + extra); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func requireImageCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), code) {
		t.Fatalf("error=%v want %s", err, code)
	}
}

func TestImageEmptyHeadersLimitsContainmentAndSanitization(t *testing.T) {
	root := t.TempDir()
	b, err := ValidateImages(root, nil)
	if err != nil || len(b.Images()) != 0 || b.Revalidate() != nil {
		t.Fatalf("empty batch=%+v err=%v", b, err)
	}
	b.Close()
	for name, raw := range map[string][]byte{"empty": {}, "truncated": {0x89, 'P'}, "unknown": []byte("secret-header-canary")} {
		t.Run(name, func(t *testing.T) {
			path := writeImageFixture(t, root, name, raw, 0)
			_, err := ValidateImages(root, []string{path})
			if name == "empty" {
				requireImageCode(t, err, "image_empty")
			} else {
				requireImageCode(t, err, "image_unsupported_format")
			}
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "secret-header-canary") {
				t.Fatalf("error leaked path/content: %v", err)
			}
		})
	}
	large := writeImageFixture(t, root, "large.png", imageHeaders[ImagePNG], int64(MaxImageBytes))
	_, err = ValidateImages(root, []string{large})
	requireImageCode(t, err, "image_too_large")
	var total []string
	for i := 0; i < 4; i++ {
		total = append(total, writeImageFixture(t, root, string(rune('a'+i))+".png", imageHeaders[ImagePNG], int64(MaxImageBytes)-int64(len(imageHeaders[ImagePNG]))))
	}
	_, err = ValidateImages(root, total)
	requireImageCode(t, err, "image_total_too_large")
	_, err = ValidateImages(root, make([]string, MaxImages+1))
	requireImageCode(t, err, "image_count_invalid")
	for _, raw := range []string{"", ".", "../x.png", "bad\n.png", "bad\u202e.png", strings.Repeat("x", MaxImagePathBytes+1)} {
		_, err = ValidateImages(root, []string{raw})
		requireImageCode(t, err, "image_path_invalid")
	}
	outsideRoot := t.TempDir()
	outside := writeImageFixture(t, outsideRoot, "outside.png", imageHeaders[ImagePNG], 0)
	_, err = ValidateImages(root, []string{outside})
	requireImageCode(t, err, "image_outside_project")
}

func TestImageDistinctSameBytesAndAbsoluteRelativePaths(t *testing.T) {
	root := t.TempDir()
	a := writeImageFixture(t, root, "space name.png", imageHeaders[ImagePNG], 0)
	b := writeImageFixture(t, root, "雪-leading-.png", imageHeaders[ImagePNG], 0)
	batch, err := ValidateImages(root, []string{"space name.png", b})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	got := batch.Images()
	if len(got) != 2 || got[0].AbsolutePath() != a || !filepath.IsAbs(got[1].AbsolutePath()) {
		t.Fatalf("descriptors=%+v", got)
	}
}
