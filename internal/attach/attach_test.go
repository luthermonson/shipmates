package attach

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/turninput"
)

func fixtures(t *testing.T, files map[string][]byte) (string, []turninput.FileDescriptorV1, func()) {
	t.Helper()
	root := t.TempDir()
	names := make([]string, 0, len(files))
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	// Deterministic order for assertions.
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	batch, err := turninput.ValidateFiles(root, names)
	if err != nil {
		t.Fatal(err)
	}
	return root, batch.Files(), batch.Close
}

func TestRenderInlinesTextReferencesBinaryAndPassesImages(t *testing.T) {
	_, files, done := fixtures(t, map[string][]byte{
		"a-notes.md": []byte("hello attachment\n"),
		"b-shot.png": {0x89, 'P', 'N', 'G', 13, 10, 26, 10, 'x'},
		"c-blob.bin": {0x00, 0xff, 0x00},
	})
	defer done()
	plan, err := Render("please review", files, Native)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 1 || plan.Images[0].DisplayPath() != "b-shot.png" {
		t.Fatalf("images = %+v", plan.Images)
	}
	for _, want := range []string{
		"attached 3 files",
		"a-notes.md (text, 17 bytes)",
		"hello attachment",
		"b-shot.png (png image",
		"c-blob.bin (binary, 3 bytes) — not inlined",
		"please review",
	} {
		if !strings.Contains(plan.Text, want) {
			t.Errorf("text missing %q:\n%s", want, plan.Text)
		}
	}
	if len(plan.Notes) != 3 {
		t.Errorf("notes = %v", plan.Notes)
	}
	atts, err := RuntimeAttachments(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].MediaType != "image/png" || atts[0].Base64 == "" {
		t.Fatalf("runtime attachments = %+v", atts)
	}
}

func TestRenderReferenceModeKeepsImagesOutOfTheAttachmentList(t *testing.T) {
	_, files, done := fixtures(t, map[string][]byte{
		"a-notes.md": []byte("still inlined"),
		"b-shot.png": {0x89, 'P', 'N', 'G', 13, 10, 26, 10, 'x'},
	})
	defer done()
	plan, err := Render("", files, Reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 0 {
		t.Fatalf("reference mode still produced attachments: %+v", plan.Images)
	}
	if !strings.Contains(plan.Text, "b-shot.png") || !strings.Contains(plan.Text, "not attached inline") {
		t.Fatalf("image not referenced by path:\n%s", plan.Text)
	}
	if !strings.Contains(plan.Text, "still inlined") {
		t.Fatalf("text attachment stopped being inlined:\n%s", plan.Text)
	}
}

func TestRenderTruncatesLargeTextWithNotice(t *testing.T) {
	body := strings.Repeat("x", MaxInlineTextBytes+512)
	_, files, done := fixtures(t, map[string][]byte{"big.txt": []byte(body)})
	defer done()
	plan, err := Render("", files, Native)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Text, "truncated to") || !strings.Contains(plan.Text, "read big.txt for the rest") {
		t.Fatalf("missing truncation notice:\n%s", plan.Text[len(plan.Text)-400:])
	}
	if strings.Count(plan.Text, "x") > MaxInlineTextBytes+16 {
		t.Fatalf("inlined more than the cap")
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "truncated") {
		t.Fatalf("notes = %v", plan.Notes)
	}
}

func TestRenderFenceCannotBeClosedByContent(t *testing.T) {
	_, files, done := fixtures(t, map[string][]byte{"md.md": []byte("```\nnot the end\n```\n")})
	defer done()
	plan, err := Render("", files, Native)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Text, "````") {
		t.Fatalf("fence not widened:\n%s", plan.Text)
	}
}

func TestRenderEmptyBatchIsJustTheCaption(t *testing.T) {
	plan, err := Render("  just words  ", nil, Native)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Text != "just words" || len(plan.Images) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
}
