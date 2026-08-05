package codexapp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func writeFixture(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func png(padding int) []byte {
	return append(append([]byte(nil), pngMagic...), bytes.Repeat([]byte{0x42}, padding)...)
}

func TestNewLocalImageClassifiesAndRoots(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "shots/screen.png", png(128))
	image, err := NewLocalImage(root, "shots/screen.png")
	if err != nil {
		t.Fatal(err)
	}
	if image.DisplayPath != "shots/screen.png" {
		t.Errorf("DisplayPath = %q", image.DisplayPath)
	}
	if !filepath.IsAbs(image.Path) {
		t.Errorf("Path = %q, want absolute", image.Path)
	}
	if image.MediaType != "image/png" {
		t.Errorf("MediaType = %q", image.MediaType)
	}
	if image.Size != uint64(len(pngMagic)+128) {
		t.Errorf("Size = %d", image.Size)
	}
	if err := RevalidateLocalImages([]LocalImage{image}); err != nil {
		t.Fatalf("revalidate a file that did not change: %v", err)
	}
}

func TestSniffImageMediaTypeCoversCodexFormats(t *testing.T) {
	cases := map[string][]byte{
		"image/png":  pngMagic,
		"image/jpeg": {0xFF, 0xD8, 0xFF, 0xE0},
		"image/gif":  []byte("GIF89a...."),
		"image/webp": []byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
	}
	for want, prefix := range cases {
		if got := sniffImageMediaType(prefix); got != want {
			t.Errorf("sniff(%q) = %q, want %q", prefix, got, want)
		}
	}
	for _, prefix := range [][]byte{[]byte("RIFF\x00\x00\x00\x00WAVEfmt "), []byte("%PDF-1.7"), []byte("plain text"), nil} {
		if got := sniffImageMediaType(prefix); got != "" {
			t.Errorf("sniff(%q) = %q, want no match", prefix, got)
		}
	}
}

func TestNewLocalImageRefusesUnusableAttachments(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "notes.txt", []byte("not an image at all"))
	writeFixture(t, root, "empty.png", nil)
	outsideRoot := t.TempDir()
	writeFixture(t, outsideRoot, "elsewhere.png", png(16))

	for _, tc := range []struct{ name, path string }{
		{"not an image", "notes.txt"},
		{"empty", "empty.png"},
		{"missing", "nope.png"},
		{"outside project", filepath.Join(outsideRoot, "elsewhere.png")},
		{"directory", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewLocalImage(root, tc.path); err == nil {
				t.Fatal("expected refusal")
			} else {
				var invalid *ErrLocalImageInvalid
				if !errors.As(err, &invalid) {
					t.Fatalf("error = %v, want *ErrLocalImageInvalid", err)
				}
			}
		})
	}
}

func TestNewLocalImageRefusesOversizeAttachment(t *testing.T) {
	root := t.TempDir()
	oversize := append(append([]byte(nil), pngMagic...), make([]byte, MaxLocalImageBytes)...)
	writeFixture(t, root, "huge.png", oversize)
	if _, err := NewLocalImage(root, "huge.png"); err == nil {
		t.Fatal("expected an oversize refusal")
	}
}

// The backend opens the file itself, so a file swapped between validation and
// the turn going out would be read in its new form. Revalidation is the only
// thing standing between "the operator approved this image" and "the model saw
// whatever is on disk now".
func TestRevalidateLocalImagesRefusesChangedFile(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, "screen.png", png(64))
	image, err := NewLocalImage(root, "screen.png")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, png(128), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateLocalImages([]LocalImage{image}); err == nil {
		t.Fatal("a resized file must fail revalidation")
	}

	// Same size, different format: the size check alone would miss this.
	sameSize := append([]byte("%PDF-1.7"), bytes.Repeat([]byte{0x42}, int(image.Size)-8)...)
	if err := os.WriteFile(path, sameSize, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateLocalImages([]LocalImage{image}); err == nil {
		t.Fatal("a file whose format changed must fail revalidation")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := RevalidateLocalImages([]LocalImage{image}); err == nil {
		t.Fatal("a deleted file must fail revalidation")
	}
}

func TestRevalidateLocalImagesRefusesUnvalidatedDescriptor(t *testing.T) {
	for _, image := range []LocalImage{
		{},
		{Path: "relative.png", MediaType: "image/png"},
		{Path: filepath.Join(t.TempDir(), "x.png")}, // no MediaType: never validated
	} {
		if err := RevalidateLocalImages([]LocalImage{image}); err == nil {
			t.Fatalf("descriptor %+v must not be accepted", image)
		}
	}
}

// StartTurn revalidates before anything reaches the wire, and reports a changed
// attachment as a backend rejection rather than a transport fault.
func TestStartTurnRefusesChangedAttachmentBeforeWriting(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, "screen.png", png(32))
	image, err := NewLocalImage(root, "screen.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, png(64), 0o644); err != nil {
		t.Fatal(err)
	}
	// No stdin: if StartTurn tried to write, this would panic rather than return.
	a := &Adapter{nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{})}
	_, err = a.StartTurn(context.Background(), "thread-1", TurnInput{Text: "look", Images: []LocalImage{image}})
	if ErrorCode(err) != BackendRejected {
		t.Fatalf("StartTurn error = %v, want backend_rejected", err)
	}
}

// An image attachment must reach the wire as a localImage item carrying the
// absolute path, alongside the text item.
func TestStartTurnSendsLocalImageItems(t *testing.T) {
	root := t.TempDir()
	image, err := NewLocalImage(root, writeFixture(t, root, "screen.png", png(32)))
	if err != nil {
		t.Fatal(err)
	}
	serverRead, clientWrite := ioPipe(t)
	a := &Adapter{stdin: clientWrite, nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{})}
	go func() {
		_, _ = a.StartTurn(context.Background(), "thread-1", TurnInput{Text: "look", Images: []LocalImage{image}})
	}()
	var request struct {
		Method string `json:"method"`
		Params struct {
			Input []struct {
				Type string `json:"type"`
				Path string `json:"path"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	line, err := bufio.NewReader(serverRead).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "turn/start" || len(request.Params.Input) != 2 {
		t.Fatalf("turn/start request = %s", line)
	}
	if request.Params.Input[0].Type != "text" || request.Params.Input[0].Text != "look" {
		t.Fatalf("text item = %+v", request.Params.Input[0])
	}
	if request.Params.Input[1].Type != "localImage" || request.Params.Input[1].Path != image.Path {
		t.Fatalf("image item = %+v, want localImage %q", request.Params.Input[1], image.Path)
	}
}
