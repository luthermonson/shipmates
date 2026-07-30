package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestSessionEnvironmentUnsupported verifies the codex runtime rejects
// SessionSpec.Environment with ErrUnsupported instead of silently dropping
// it: the app-server transport (codexapp.ThreadOptions) cannot carry a
// per-session process environment. The check runs before any adapter call,
// so a zero-value Runtime is enough to exercise it.
func TestSessionEnvironmentUnsupported(t *testing.T) {
	r := &Runtime{}
	spec := runtime.SessionSpec{
		Persona:     "tester",
		ProjectDir:  t.TempDir(),
		Environment: map[string]string{"FOO": "bar"},
	}

	var unsupported *runtime.ErrUnsupported
	if _, err := r.StartSession(context.Background(), spec); !errors.As(err, &unsupported) {
		t.Fatalf("StartSession error = %v, want ErrUnsupported", err)
	}
	if _, err := r.ResumeSession(context.Background(), "thread-1", spec); !errors.As(err, &unsupported) {
		t.Fatalf("ResumeSession error = %v, want ErrUnsupported", err)
	}
	if r.Capabilities().Environment {
		t.Fatal("codex Caps.Environment must be false")
	}
}

// Every capability this runtime reports must be backed by code that does the
// work. Attachments and Approvals are called out individually because both were
// previously reported true while the implementation discarded the input.
func TestCapabilitiesAreBackedByImplementation(t *testing.T) {
	r := &Runtime{caps: codexapp.Capabilities{
		ThreadStart: true, ThreadResume: true, TurnStart: true,
		Steer: true, Interrupt: true, RequestRefusal: true, LocalImage: true,
	}}
	caps := r.Capabilities()

	if !caps.Streaming || !caps.Interrupt || !caps.Steer || !caps.Refusal {
		t.Errorf("caps = %+v; transport supports all four", caps)
	}
	// Attachments: true requires SendTurn to translate them. localImages is that
	// translation; if it ever returns nothing for a valid image, the capability is
	// a lie again.
	if !caps.Attachments {
		t.Error("Attachments should be true when the transport advertises LocalImage")
	}
	images, err := localImages([]runtime.Attachment{{
		Path: filepath.Join(t.TempDir(), "shot.png"), DisplayPath: "shot.png",
		Kind: "image", MediaType: "image/png", Size: 128,
	}})
	if err != nil || len(images) != 1 {
		t.Fatalf("localImages dropped a valid image attachment: %v %+v", err, images)
	}
	if !caps.Approvals {
		t.Error("Approvals should be true: the adapter mediates requests")
	}
	// Containment tracks what the transport was actually started with.
	if caps.Containment {
		t.Error("Containment must be false when the transport was not contained")
	}
	if (&Runtime{contained: true}).Capabilities().Containment != true {
		t.Error("Containment must be true when the transport was started contained")
	}
}

func TestLocalImagesRefusesWhatCodexCannotCarry(t *testing.T) {
	dir := t.TempDir()
	valid := runtime.Attachment{Path: filepath.Join(dir, "a.png"), DisplayPath: "a.png", Kind: "image", MediaType: "image/png", Size: 10}

	text := valid
	text.Kind, text.MediaType = "text", "text/plain"
	binary := valid
	binary.Kind, binary.MediaType = "binary", "application/pdf"
	badMedia := valid
	badMedia.MediaType = "image/bmp"
	noPath := valid
	noPath.Path = ""

	for name, attachment := range map[string]runtime.Attachment{
		"text":           text,
		"binary":         binary,
		"unknown format": badMedia,
		"missing path":   noPath,
		"no media type":  {Path: valid.Path, Kind: "image"},
	} {
		if _, err := localImages([]runtime.Attachment{attachment}); err == nil {
			t.Errorf("%s attachment was accepted", name)
		}
	}
	// Silence is the failure mode this guards against: a rejected attachment must
	// produce an error, never an empty image list that looks like success.
	if images, err := localImages([]runtime.Attachment{text}); err == nil || images != nil {
		t.Errorf("rejected attachment returned images=%v err=%v", images, err)
	}
	if images, err := localImages(nil); err != nil || images != nil {
		t.Errorf("no attachments = %v, %v", images, err)
	}
}

// A KindApprovalNeeded event must carry the exact runtime.ApprovalResponse that
// ResolveApproval expects back. Without it a consumer would have to import
// codexapp to answer, and Approvals would be unusable through the interface.
func TestNormalizeApprovalCarriesResolvableResponse(t *testing.T) {
	ev := codexapp.Event{
		Kind: codexapp.ApprovalRequested, ThreadID: "thread-1", TurnID: "turn-1",
		BackendRequestID: "42", Category: "command", CommandExact: "rm -rf build",
	}
	out := normalize(ev)
	if out.Kind != runtime.KindApprovalNeeded {
		t.Fatalf("kind = %q", out.Kind)
	}
	resp, ok := out.Payload.(runtime.ApprovalResponse)
	if !ok {
		t.Fatalf("payload = %T, want runtime.ApprovalResponse", out.Payload)
	}
	if resp.ID != "42" || resp.SessionID != "thread-1" || resp.TurnID != "turn-1" {
		t.Fatalf("approval response = %+v", resp)
	}
	if resp.Kind != "tool_use" || resp.Reason != "rm -rf build" {
		t.Fatalf("approval detail = %+v", resp)
	}

	fileChange := normalize(codexapp.Event{
		Kind: codexapp.ApprovalRequested, ThreadID: "t", TurnID: "u",
		BackendRequestID: "43", Category: "file_change", Detail: "update main.go",
	})
	if resp := fileChange.Payload.(runtime.ApprovalResponse); resp.Kind != "file_write" || resp.Reason != "update main.go" {
		t.Fatalf("file change approval = %+v", resp)
	}
}

func TestNormalizeMapsEveryAdapterEventKind(t *testing.T) {
	for _, tc := range []struct {
		in   codexapp.EventKind
		want runtime.Kind
	}{
		{codexapp.AgentMessage, runtime.KindText},
		{codexapp.Activity, runtime.KindToolCall},
		{codexapp.ApprovalRequested, runtime.KindApprovalNeeded},
		{codexapp.RequestRefused, runtime.KindError},
		{codexapp.TurnCompleted, runtime.KindTurnDone},
		{codexapp.TurnFailed, runtime.KindError},
		{codexapp.AdapterFault, runtime.KindError},
		{codexapp.EventKind("something/new"), runtime.KindBackend},
	} {
		if got := normalize(codexapp.Event{Kind: tc.in, ThreadID: "t", TurnID: "u"}); got.Kind != tc.want {
			t.Errorf("normalize(%q).Kind = %q, want %q", tc.in, got.Kind, tc.want)
		}
	}
}

// WorkingDir defaults to ProjectDir, matching the SessionSpec contract. An empty
// cwd would be rejected by the app-server, so the default is load-bearing.
func TestThreadOptsFromSpecDefaultsWorkingDirToProjectDir(t *testing.T) {
	proj := t.TempDir()
	if got := threadOptsFromSpec(runtime.SessionSpec{ProjectDir: proj}); got.WorkingDirectory != proj {
		t.Errorf("WorkingDirectory = %q, want %q", got.WorkingDirectory, proj)
	}
	work := filepath.Join(proj, "sub")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := threadOptsFromSpec(runtime.SessionSpec{ProjectDir: proj, WorkingDir: work}); got.WorkingDirectory != work {
		t.Errorf("WorkingDirectory = %q, want %q", got.WorkingDirectory, work)
	}
}

func TestNameAndSessionBookkeeping(t *testing.T) {
	r := &Runtime{sessions: map[string]*session{}}
	if r.Name() != "codex" {
		t.Fatalf("Name = %q", r.Name())
	}
	s := r.rememberSession("thread-9", runtime.SessionSpec{Persona: "bosun", ProjectDir: "/proj"})
	if s.ID() != "thread-9" || s.Persona() != "bosun" || s.ProjectDir() != "/proj" {
		t.Fatalf("session = %+v", s)
	}
	if err := r.CloseSession(context.Background(), "thread-9"); err != nil {
		t.Fatal(err)
	}
	if len(r.sessions) != 0 {
		t.Fatalf("CloseSession left %d sessions", len(r.sessions))
	}
}
