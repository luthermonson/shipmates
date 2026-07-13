//go:build unix

package turninput

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImageValidationOrderDuplicatesLinksAndReplacement(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10}
	for _, n := range []string{"space name.png", "-dash.png", "雪.png"} {
		if e := os.WriteFile(filepath.Join(root, n), png, 0o600); e != nil {
			t.Fatal(e)
		}
	}
	b, e := ValidateImages(root, []string{"space name.png", "-dash.png", "雪.png"})
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if len(b.Images()) != 3 || b.Images()[1].DisplayPath() != "-dash.png" {
		t.Fatal("order changed")
	}
	if _, e = ValidateImages(root, []string{"雪.png", "./雪.png"}); e == nil {
		t.Fatal("duplicate accepted")
	}
	if e = os.Symlink(filepath.Join(root, "雪.png"), filepath.Join(root, "link.png")); e != nil {
		t.Fatal(e)
	}
	if _, e = ValidateImages(root, []string{"link.png"}); e == nil {
		t.Fatal("link accepted")
	}
	if e = os.Rename(filepath.Join(root, "space name.png"), filepath.Join(root, "old")); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "space name.png"), png, 0o600); e != nil {
		t.Fatal(e)
	}
	if e = b.Revalidate(); e == nil {
		t.Fatal("replacement accepted")
	}
}
func TestImageFormatsAndCount(t *testing.T) {
	root := t.TempDir()
	formats := map[string][]byte{"p": {0x89, 'P', 'N', 'G', 13, 10, 26, 10}, "j": {0xff, 0xd8, 0xff, 0xe0}, "g": []byte("GIF87a"), "w": []byte("RIFF0000WEBPVP8X")}
	for n, h := range formats {
		if e := os.WriteFile(filepath.Join(root, n), h, 0o600); e != nil {
			t.Fatal(e)
		}
	}
	for n := range formats {
		b, e := ValidateImages(root, []string{n})
		if e != nil {
			t.Fatal(e)
		}
		b.Close()
	}
	if _, e := ValidateImages(root, make([]string, 9)); e == nil {
		t.Fatal("nine accepted")
	}
}
