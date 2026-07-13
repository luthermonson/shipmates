//go:build unix

package dashboard

import (
	"github.com/luthermonson/shipmates/internal/livesession"
	"os"
	"strings"
	"testing"
)

func TestImageCommandsSelectionRenderAndClear(t *testing.T) {
	t.Chdir(t.TempDir())
	if e := os.WriteFile("space image.png", []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10}, 0o600); e != nil {
		t.Fatal(e)
	}
	m := &Model{State: livesession.Idle}
	p, e := ParseLine("/image add space image.png")
	if e != nil || p.Kind != ParsedImageAdd {
		t.Fatal(p, e)
	}
	if e = m.AddImage(p.Text); e != nil {
		t.Fatal(e)
	}
	screen := Render(m, Size{80, 10})
	joined := strings.Join(screen.Lines, "\n")
	if !strings.Contains(joined, "space image.png") || strings.Contains(joined, m.Images[0].path) {
		t.Fatalf("render leak=%q", joined)
	}
	if e = m.RemoveImage(1); e != nil || len(m.Images) != 0 {
		t.Fatal(e)
	}
	m.AddImage("space image.png")
	m.ClearImages()
	if len(m.Images) != 0 {
		t.Fatal("not cleared")
	}
}
