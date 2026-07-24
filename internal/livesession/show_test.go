package livesession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/turninput"
)

func showFixtureFiles(t *testing.T, files map[string][]byte) []turninput.FileDescriptorV1 {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
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
	t.Cleanup(batch.Close)
	return batch.Files()
}

// TestShowInjectsIntoTheRunningTurn is the headline behavior: a persona with
// a turn in flight receives the attachment inside that turn.
func TestShowInjectsIntoTheRunningTurn(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work on it"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	before := s.Snapshot()

	files := showFixtureFiles(t, map[string][]byte{
		"a-shot.png": {0x89, 'P', 'N', 'G', 13, 10, 26, 10, 'x'},
		"b-log.txt":  []byte("stack trace line"),
	})
	res, err := m.ShowAttachment(context.Background(), "backend", "look at this", files)
	if err != nil {
		t.Fatalf("ShowAttachment: %v", err)
	}
	if !res.Injected {
		t.Fatal("attachment started a new turn instead of joining the running one")
	}
	if res.Snapshot.TurnID != before.TurnID {
		t.Fatalf("turn changed: %q -> %q", before.TurnID, res.Snapshot.TurnID)
	}
	if len(rt.sentTurns) != 1 {
		t.Fatalf("a new turn was sent: %+v", rt.sentTurns)
	}
	if len(rt.steerInputs) != 1 {
		t.Fatalf("steer inputs = %+v, want 1", rt.steerInputs)
	}
	in := rt.steerInputs[0]
	if len(in.Attachments) != 1 || in.Attachments[0].MediaType != "image/png" || in.Attachments[0].Base64 == "" {
		t.Fatalf("image did not ride natively: %+v", in.Attachments)
	}
	for _, want := range []string{"a-shot.png", "b-log.txt", "stack trace line", "look at this"} {
		if !strings.Contains(in.Text, want) {
			t.Errorf("steer text missing %q:\n%s", want, in.Text)
		}
	}
	feed, _ := json.Marshal(s.Feed(0))
	if !strings.Contains(string(feed), `"kind":"steering.accepted"`) || !strings.Contains(string(feed), `"image_count":1`) {
		t.Errorf("feed missing the accepted steer: %s", feed)
	}
	if strings.Contains(string(feed), "stack trace line") {
		t.Errorf("attachment content leaked into the feed: %s", feed)
	}
}

// TestShowOnIdleSessionStartsATurn covers the other live state: no turn in
// flight, so the attachment starts one on the same session.
func TestShowOnIdleSessionStartsATurn(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	files := showFixtureFiles(t, map[string][]byte{"notes.md": []byte("read me")})
	res, err := m.ShowAttachment(context.Background(), "backend", "", files)
	if err != nil {
		t.Fatalf("ShowAttachment: %v", err)
	}
	if res.Injected {
		t.Fatal("idle session reported a mid-turn injection")
	}
	if len(rt.sentTurns) != 1 || !strings.Contains(rt.sentTurns[0].Text, "read me") {
		t.Fatalf("sent turns = %+v", rt.sentTurns)
	}
	if len(rt.steerInputs) != 0 || len(rt.steers) != 0 {
		t.Fatalf("idle session was steered: %v %+v", rt.steers, rt.steerInputs)
	}
	if res.Snapshot.State != Working {
		t.Fatalf("state = %s, want working", res.Snapshot.State)
	}
}

// TestShowRefusesBinaryInliningAndNamesIt documents the binary policy: never
// base64 into a prompt, always a path reference carrying size and kind.
func TestShowRefusesBinaryInliningAndNamesIt(t *testing.T) {
	fixture(t, "backend")
	rt := newFakeRuntime()
	m := runtimeManager(t, rt)
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	files := showFixtureFiles(t, map[string][]byte{"archive.bin": {0x00, 0x01, 0x02, 0x03, 0x04}})
	res, err := m.ShowAttachment(context.Background(), "backend", "", files)
	if err != nil {
		t.Fatal(err)
	}
	text := rt.sentTurns[0].Text
	if !strings.Contains(text, "archive.bin (binary, 5 bytes)") || !strings.Contains(text, "not inlined") {
		t.Fatalf("binary not referenced by path:\n%s", text)
	}
	if strings.Contains(text, "AAECAwQ=") {
		t.Fatalf("binary was base64-inlined:\n%s", text)
	}
	if len(rt.sentTurns[0].Attachments) != 0 {
		t.Fatalf("binary became a content block: %+v", rt.sentTurns[0].Attachments)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "referenced by path") {
		t.Fatalf("notes = %v", res.Notes)
	}
}

// TestShowRefusesWhenThereIsNoLiveSession keeps the CLI's fallback contract:
// a missing session is a not_found code, which `shipmates show` turns into a
// one-shot delivery.
func TestShowRefusesWhenThereIsNoLiveSession(t *testing.T) {
	fixture(t, "backend")
	m := runtimeManager(t, newFakeRuntime())
	if _, err := m.ShowAttachment(context.Background(), "backend", "", nil); ErrorCode(err) != NotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
}

// TestCodexAdapterCannotSteerWithAttachments pins the codex limitation the
// docs describe: the app-server steer path carries text only, so images are
// referenced by path mid-turn rather than attached.
func TestCodexAdapterCannotSteerWithAttachments(t *testing.T) {
	var a Backend = (*codexapp.Adapter)(nil)
	if _, ok := a.(attachmentSteerer); ok {
		t.Fatal("codex adapter now steers with attachments; livesession should stop using attach.Reference for it")
	}
	var b Backend = (*runtimeBackend)(nil)
	if _, ok := b.(attachmentSteerer); !ok {
		t.Fatal("runtime backend lost mid-turn attachment support")
	}
}
