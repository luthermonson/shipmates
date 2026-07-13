//go:build unix

package turninput

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestValidateImagesUnixRefusesLinksAndNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	target := writeImageFixture(t, root, "real.png", imageHeaders[ImagePNG], 0)
	if err := os.Symlink(target, filepath.Join(root, "leaf.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real-dir"), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo.png"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, "socket.png"), Net: "unix"})
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Logf("socket fixture unavailable in sandbox: %v", err)
		} else {
			t.Fatal(err)
		}
	} else {
		defer listener.Close()
	}
	cases := map[string][2]string{
		"leaf-link":     {"leaf.png", "image_link_refused"},
		"ancestor-link": {"linked-dir/x.png", "image_link_refused"},
		"directory":     {"directory.png", "image_not_regular"},
		"fifo":          {"fifo.png", "image_not_regular"},
	}
	if listener != nil {
		cases["socket"] = [2]string{"socket.png", "image_not_regular"}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateImages(root, []string{tc[0]})
			requireImageCode(t, err, tc[1])
		})
	}
}

func TestValidateImagesUnixDuplicatesPermissionsAndRevalidation(t *testing.T) {
	root := t.TempDir()
	one := writeImageFixture(t, root, "one.png", imageHeaders[ImagePNG], 0)
	two := filepath.Join(root, "two.png")
	if err := os.Link(one, two); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateImages(root, []string{one, two})
	requireImageCode(t, err, "image_duplicate")

	unreadable := writeImageFixture(t, root, "unreadable.png", imageHeaders[ImagePNG], 0)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateImages(root, []string{unreadable})
	requireImageCode(t, err, "image_unreadable")

	stable := writeImageFixture(t, root, "stable.png", imageHeaders[ImagePNG], 0)
	batch, err := ValidateImages(root, []string{stable})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	if err := os.WriteFile(stable, imageHeaders[ImageGIF], 0o600); err != nil {
		t.Fatal(err)
	}
	requireImageCode(t, batch.Revalidate(), "image_changed")
}

func TestValidateImagesUnixDetectsRenameReplacementAndLinkCount(t *testing.T) {
	for _, mutation := range []string{"rename-replace", "hardlink", "chmod", "ancestor-replace"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			path := writeImageFixture(t, root, "dir/image.png", imageHeaders[ImagePNG], 0)
			batch, err := ValidateImages(root, []string{path})
			if err != nil {
				t.Fatal(err)
			}
			defer batch.Close()
			switch mutation {
			case "rename-replace":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				writeImageFixture(t, root, "dir/image.png", imageHeaders[ImagePNG], 0)
			case "hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "chmod":
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
			case "ancestor-replace":
				if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "old-dir")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
					t.Fatal(err)
				}
				writeImageFixture(t, root, "dir/image.png", imageHeaders[ImagePNG], 0)
			}
			requireImageCode(t, batch.Revalidate(), "image_changed")
		})
	}
}

func TestValidateImagesUnixRefusesSymlinkRoot(t *testing.T) {
	real := t.TempDir()
	writeImageFixture(t, real, "image.png", imageHeaders[ImagePNG], 0)
	parent := t.TempDir()
	link := filepath.Join(parent, "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateImages(link, []string{"image.png"})
	requireImageCode(t, err, "image_root_invalid")
}
