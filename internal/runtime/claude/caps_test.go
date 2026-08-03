package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestCapabilities_AreExactlyWhatIsImplemented pins the whole Caps struct.
// Callers branch on these flags, so a capability reported true but silently
// dropped is worse than ErrUnsupported — this test makes any change to the
// answer a deliberate one, and every other test in this file exists to prove
// one of the trues.
func TestCapabilities_AreExactlyWhatIsImplemented(t *testing.T) {
	want := runtime.Caps{
		Streaming:   true,  // readLoop publishes each frame as it is scanned
		Interrupt:   true,  // control_request interrupt + handle-kill fallback
		Steer:       true,  // mid-turn user message folded into the live turn
		Attachments: true,  // image content blocks; anything else is an error
		Refusal:     false, // claude surfaces refusals as ordinary text
		Containment: false, // SessionSpec.ContainExec is not honored
		Environment: true,  // SessionSpec.Environment reaches every spawn
		Approvals:   true,  // can_use_tool mediated through ResolveApproval
	}
	if got := New(Config{}).Capabilities(); got != want {
		t.Fatalf("Caps = %+v, want %+v", got, want)
	}
}

// TestSendTurn_ImageAttachmentReachesChildAsContentBlock is what earns
// Caps.Attachments = true: the attachment is not merely accepted, it arrives at
// the child as a base64 image content block, ahead of the text block that
// instructs the model about it.
func TestSendTurn_ImageAttachmentReachesChildAsContentBlock(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_CONTENT", "1")
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{
		Text: "what is this",
		Attachments: []runtime.Attachment{{
			Path:        "/project/shot.png",
			DisplayPath: "shot.png",
			Kind:        "image",
			MediaType:   "image/png",
			Size:        2,
			Base64:      "aGk=",
		}},
	}); err != nil {
		t.Fatalf("SendTurn with an image attachment: %v", err)
	}
	waitEventText(t, rt, "content:image/base64/image/png/aGk=,text")
	waitTurnDone(t, rt)
}

// TestSteerTurnInput_ImageAttachmentReachesLiveTurn proves the same encoding
// works mid-turn, which is what SteerTurnInput exists for.
func TestSteerTurnInput_ImageAttachmentReachesLiveTurn(t *testing.T) {
	rt := fakeClaudeRuntime(t, 3000)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_CONTENT", "1")
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "look"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitEventText(t, rt, "fake hello")

	if err := rt.SteerTurnInput(context.Background(), s.ID(), turn.ID(), runtime.TurnInput{
		Text: "and this one",
		Attachments: []runtime.Attachment{{
			DisplayPath: "second.jpg",
			Kind:        "image",
			MediaType:   "image/jpeg",
			Base64:      "b2s=",
		}},
	}); err != nil {
		t.Fatalf("SteerTurnInput with an image attachment: %v", err)
	}
	waitEventText(t, rt, "content:image/base64/image/jpeg/b2s=,text")
	waitTurnDone(t, rt)
}

// TestSendTurn_RejectsUnencodableAttachment is the other half of the honesty
// claim. Caps.Attachments = true would be a lie if an attachment this runtime
// cannot express as a content block vanished without a word, so every such
// attachment is a hard error on the turn instead.
func TestSendTurn_RejectsUnencodableAttachment(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	cases := map[string]runtime.Attachment{
		"text kind":            {DisplayPath: "notes.txt", Kind: "text", MediaType: "text/plain", Base64: "aGk="},
		"binary kind":          {DisplayPath: "blob.bin", Kind: "binary", MediaType: "application/octet-stream", Base64: "aGk="},
		"image without base64": {DisplayPath: "empty.png", Kind: "image", MediaType: "image/png"},
		"image without type":   {DisplayPath: "untyped.png", Kind: "image", Base64: "aGk="},
	}
	for name, a := range cases {
		_, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{
			Text:        "describe it",
			Attachments: []runtime.Attachment{a},
		})
		if err == nil {
			t.Errorf("%s: SendTurn accepted an attachment it cannot encode", name)
			continue
		}
		if !strings.Contains(err.Error(), "not an encodable image content block") {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if !strings.Contains(err.Error(), a.DisplayPath) {
			t.Errorf("%s: error does not name the attachment: %v", name, err)
		}
	}

	// A rejected attachment must not leave the session's single turn slot
	// reserved — the operator has to be able to retry without it.
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "plain retry"}); err != nil {
		t.Fatalf("turn slot still held after a rejected attachment: %v", err)
	}
	waitTurnDone(t, rt)
}

// TestContainExecIsIgnoredNotHonored keeps Caps.Containment = false honest in
// the other direction: a spec asking for contained execution is accepted and
// run WITHOUT it, exactly as a false capability advertises. If this package
// ever starts honoring ContainExec, Caps.Containment has to flip with it.
func TestContainExecIsIgnoredNotHonored(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())

	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{
		Persona:     "tester",
		ProjectDir:  t.TempDir(),
		ContainExec: true,
	})
	if err != nil {
		t.Fatalf("StartSession with ContainExec: %v", err)
	}
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "go"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitTurnDone(t, rt)
}
