package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/turninput"
)

var showPNG = []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}

func writeShowFixture(t *testing.T, name string, body []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestShowInlinesTextAndReferencesBinaryOnRuntimePath asserts the delivered
// turn text inlines a text attachment and refers to a binary one by path
// rather than base64-ing it into the prompt.
func TestShowInlinesTextAndReferencesBinaryOnRuntimePath(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	writeShowFixture(t, "notes.md", []byte("line one\nline two\n"))
	writeShowFixture(t, "blob.bin", []byte{0x00, 0x01, 0x02, 0x03})
	rt := newFakeRuntime(turnScript("ack"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := runShow(context.Background(), "claude", "security", []string{"notes.md", "blob.bin"}, "have a look", false, &stdout, &stderr); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if len(rt.sentTurns) != 1 {
		t.Fatalf("SendTurn calls = %d, want 1", len(rt.sentTurns))
	}
	text := rt.sentTurns[0].Text
	for _, want := range []string{"notes.md", "line one", "blob.bin", "binary", "not inlined", "have a look"} {
		if !strings.Contains(text, want) {
			t.Errorf("turn text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02, 0x03})) {
		t.Errorf("binary attachment was base64-inlined:\n%s", text)
	}
	if len(rt.sentTurns[0].Attachments) != 0 {
		t.Errorf("non-image attachments became content blocks: %+v", rt.sentTurns[0].Attachments)
	}
}

// TestShowSendsImageAsRuntimeAttachment asserts an image becomes a typed,
// base64 content block carrying the sniffed media type.
func TestShowSendsImageAsRuntimeAttachment(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	writeShowFixture(t, filepath.Join("shots", "screen.png"), showPNG)
	rt := newFakeRuntime(turnScript("ack"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := runShow(context.Background(), "claude", "security", []string{filepath.Join("shots", "screen.png")}, "", false, &stdout, &stderr); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	atts := rt.sentTurns[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("attachments = %+v, want 1", atts)
	}
	if atts[0].Kind != "image" || atts[0].MediaType != "image/png" {
		t.Errorf("attachment kind/media = %q/%q", atts[0].Kind, atts[0].MediaType)
	}
	if atts[0].DisplayPath != "shots/screen.png" {
		t.Errorf("display path = %q", atts[0].DisplayPath)
	}
	if atts[0].Base64 != base64.StdEncoding.EncodeToString(showPNG) {
		t.Errorf("attachment bytes mismatch")
	}
	if !strings.Contains(rt.sentTurns[0].Text, "shots/screen.png") {
		t.Errorf("turn text does not name the image: %q", rt.sentTurns[0].Text)
	}
}

// TestShowRefusesRuntimeWithoutAttachmentSupport keeps the capability check
// honest rather than silently dropping the image.
func TestShowRefusesRuntimeWithoutAttachmentSupport(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	writeShowFixture(t, "screen.png", showPNG)
	rt := newFakeRuntime(turnScript("ack"))
	rt.caps = runtime.Caps{Streaming: true}
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	err := runShow(context.Background(), "claude", "security", []string{"screen.png"}, "", false, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "cannot carry image attachments") {
		t.Fatalf("err = %v, want capability refusal", err)
	}
}

// TestShowFallsBackToCodexNativePath asserts the codex dispatcher receives
// the rendered text plus only the image descriptors.
func TestShowFallsBackToCodexNativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	writeShowFixture(t, "screen.png", showPNG)
	writeShowFixture(t, "notes.md", []byte("inline me"))
	swapSelectorErr(t, &runtime.ErrNotConfigured{Runtime: "codex", Reason: "needs app-server transport"})

	var gotImages []turninput.ImageDescriptorV1
	var gotPrompt string
	prev := codexTurnDispatcher
	codexTurnDispatcher = func(_ context.Context, installed *project.InstalledPersona, prompt string, _ bool, _ project.PersonaConfig, images []turninput.ImageDescriptorV1, stdout, _ io.Writer) error {
		gotImages, gotPrompt = images, prompt
		_, _ = io.WriteString(stdout, "codex ack\n")
		return nil
	}
	t.Cleanup(func() { codexTurnDispatcher = prev })

	var stdout, stderr bytes.Buffer
	if err := runShow(context.Background(), "", "security", []string{"screen.png", "notes.md"}, "caption here", false, &stdout, &stderr); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if len(gotImages) != 1 || gotImages[0].DisplayPath() != "screen.png" {
		t.Fatalf("codex images = %+v, want only screen.png", gotImages)
	}
	if !strings.Contains(gotPrompt, "inline me") || !strings.Contains(gotPrompt, "caption here") {
		t.Errorf("codex prompt = %q", gotPrompt)
	}
}

// TestShowRejectsOutOfProjectAndReservedPersona keeps the turninput
// confinement and the captain guard wired into the command.
func TestShowRejectsOutOfProjectAndReservedPersona(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := newFakeRuntime(turnScript("ack"))
	swapSelector(t, rt)

	var stdout, stderr bytes.Buffer
	if err := runShow(context.Background(), "claude", "security", []string{outside}, "", false, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "file_outside_project") {
		t.Fatalf("out-of-project err = %v", err)
	}
	writeShowFixture(t, "notes.md", []byte("hi"))
	if err := runShow(context.Background(), "claude", "captain", []string{"notes.md"}, "", false, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("captain err = %v", err)
	}
	if len(rt.sentTurns) != 0 {
		t.Fatalf("a refused show still dispatched a turn: %+v", rt.sentTurns)
	}
}
