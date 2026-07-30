package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRuntime is a Runtime whose Caps and whose behavior can be set
// independently, so VerifyCaps can be shown to catch a runtime that lies about
// itself — which is the only thing it exists to do.
type fakeRuntime struct {
	name string
	caps Caps
	// honest makes every unsupported method return *ErrUnsupported. When
	// false, they return nil instead — the exact dishonesty VerifyCaps hunts.
	honest bool
	// wrongName makes ErrUnsupported name a different runtime.
	wrongName bool
	// emptyFeature makes ErrUnsupported omit the feature.
	emptyFeature bool

	closedSessions []string
}

func (f *fakeRuntime) unsupported(feature string) error {
	if !f.honest {
		return nil
	}
	name := f.name
	if f.wrongName {
		name = "someone-else"
	}
	if f.emptyFeature {
		feature = ""
	}
	return Unsupported(name, feature)
}

func (f *fakeRuntime) Name() string         { return f.name }
func (f *fakeRuntime) Capabilities() Caps   { return f.caps }
func (f *fakeRuntime) Events() <-chan Event { return nil }

func (f *fakeRuntime) StartSession(_ context.Context, spec SessionSpec) (Session, error) {
	if len(spec.Environment) > 0 && !f.caps.Environment {
		if err := f.unsupported("StartSession"); err != nil {
			return nil, err
		}
	}
	return fakeSession{id: "session-1"}, nil
}

func (f *fakeRuntime) ResumeSession(context.Context, string, SessionSpec) (Session, error) {
	return fakeSession{id: "session-1"}, nil
}

func (f *fakeRuntime) CloseSession(_ context.Context, id string) error {
	f.closedSessions = append(f.closedSessions, id)
	return nil
}

func (f *fakeRuntime) SendTurn(context.Context, string, TurnInput) (Turn, error) {
	return nil, nil
}

func (f *fakeRuntime) InterruptTurn(context.Context, string, string) error {
	if f.caps.Interrupt {
		return nil
	}
	return f.unsupported("InterruptTurn")
}

func (f *fakeRuntime) SteerTurn(context.Context, string, string, string) error {
	if f.caps.Steer {
		return nil
	}
	return f.unsupported("SteerTurn")
}

func (f *fakeRuntime) ResolveApproval(context.Context, ApprovalResponse, ApprovalDecision) (bool, error) {
	if f.caps.Approvals {
		return true, nil
	}
	return false, f.unsupported("ResolveApproval")
}

func (f *fakeRuntime) InstallPersona(context.Context, string, PersonaSpec) error {
	if f.caps.PersonaInstall {
		return nil
	}
	return f.unsupported("InstallPersona")
}

func (f *fakeRuntime) UninstallPersona(context.Context, string, string) error {
	if f.caps.PersonaInstall {
		return nil
	}
	return f.unsupported("UninstallPersona")
}

func (f *fakeRuntime) InstallMemoryHook(context.Context, string) error {
	if f.caps.MemoryHook {
		return nil
	}
	return f.unsupported("InstallMemoryHook")
}

func (f *fakeRuntime) Close(context.Context) error { return nil }

type fakeSession struct{ id string }

func (s fakeSession) ID() string         { return s.id }
func (s fakeSession) Persona() string    { return "captain" }
func (s fakeSession) ProjectDir() string { return "" }

var _ Runtime = (*fakeRuntime)(nil)

func TestVerifyCaps_HonestRuntimePasses(t *testing.T) {
	rt := &fakeRuntime{name: "honest", honest: true}
	if errs := VerifyCaps(context.Background(), rt, t.TempDir()); len(errs) != 0 {
		for _, err := range errs {
			t.Errorf("unexpected: %v", err)
		}
	}
}

// A runtime that supports everything is asked nothing: VerifyCaps only probes
// the capabilities reported false.
func TestVerifyCaps_FullyCapableRuntimePasses(t *testing.T) {
	rt := &fakeRuntime{name: "capable", caps: Caps{
		Streaming: true, Interrupt: true, Steer: true, Attachments: true,
		Refusal: true, Containment: true, Environment: true, Approvals: true,
		PersonaInstall: true, MemoryHook: true,
	}}
	if errs := VerifyCaps(context.Background(), rt, t.TempDir()); len(errs) != 0 {
		for _, err := range errs {
			t.Errorf("unexpected: %v", err)
		}
	}
}

// The point of the whole exercise: a runtime whose Caps say "no" while its
// methods quietly succeed is caught, with one error per feature.
func TestVerifyCaps_CatchesSilentlySucceedingMethods(t *testing.T) {
	rt := &fakeRuntime{name: "liar", honest: false}
	errs := VerifyCaps(context.Background(), rt, t.TempDir())
	wantFeatures := []string{
		"SteerTurn", "InterruptTurn", "ResolveApproval",
		"StartSession", "InstallPersona", "UninstallPersona", "InstallMemoryHook",
	}
	if len(errs) != len(wantFeatures) {
		t.Fatalf("got %d errors, want %d:\n%v", len(errs), len(wantFeatures), errs)
	}
	joined := joinErrors(errs)
	for _, f := range wantFeatures {
		if !strings.Contains(joined, f) {
			t.Errorf("no error mentions %s:\n%s", f, joined)
		}
	}
}

// A session accepted despite a false Environment capability must not be left
// running just because the runtime was wrong about itself.
func TestVerifyCaps_ClosesTheSessionItHadToOpen(t *testing.T) {
	rt := &fakeRuntime{name: "liar", honest: false}
	VerifyCaps(context.Background(), rt, t.TempDir())
	if len(rt.closedSessions) != 1 || rt.closedSessions[0] != "session-1" {
		t.Errorf("closed sessions = %v, want the one session VerifyCaps opened", rt.closedSessions)
	}
}

func TestVerifyCaps_CatchesWrongRuntimeName(t *testing.T) {
	rt := &fakeRuntime{name: "claude", honest: true, wrongName: true}
	errs := VerifyCaps(context.Background(), rt, t.TempDir())
	if len(errs) == 0 {
		t.Fatal("expected errors for an ErrUnsupported naming another runtime")
	}
	if !strings.Contains(joinErrors(errs), "someone-else") {
		t.Errorf("errors do not mention the wrong name:\n%s", joinErrors(errs))
	}
}

func TestVerifyCaps_CatchesEmptyFeature(t *testing.T) {
	rt := &fakeRuntime{name: "claude", honest: true, emptyFeature: true}
	errs := VerifyCaps(context.Background(), rt, t.TempDir())
	if len(errs) == 0 {
		t.Fatal("expected errors for an ErrUnsupported with no feature named")
	}
	if !strings.Contains(joinErrors(errs), "empty Feature") {
		t.Errorf("errors do not explain the problem:\n%s", joinErrors(errs))
	}
}

func TestVerifyCaps_CatchesEmptyName(t *testing.T) {
	rt := &fakeRuntime{name: "", honest: true}
	errs := VerifyCaps(context.Background(), rt, t.TempDir())
	if len(errs) == 0 {
		t.Fatal("expected an error for a Runtime that does not identify itself")
	}
}

func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "\n")
}

// --- error plumbing -------------------------------------------------------

func TestUnsupported_MatchesSentinelAndConcreteType(t *testing.T) {
	err := Unsupported("claude", "SteerTurn")
	if !errors.Is(err, ErrFeatureUnsupported) {
		t.Error("errors.Is(err, ErrFeatureUnsupported) should hold so callers can ask the general question")
	}
	var target *ErrUnsupported
	if !errors.As(err, &target) || target.Runtime != "claude" || target.Feature != "SteerTurn" {
		t.Errorf("errors.As gave %+v", target)
	}
	// Two distinct ErrUnsupported values are not each other.
	if errors.Is(err, Unsupported("codex", "SteerTurn")) {
		t.Error("distinct ErrUnsupported values should not compare equal")
	}
	// And it survives wrapping, which is how it will actually travel.
	wrapped := errors.Join(errors.New("ask captain"), err)
	if !errors.Is(wrapped, ErrFeatureUnsupported) {
		t.Error("a wrapped ErrUnsupported should still match the sentinel")
	}
}

func TestErrUnsupported_Message(t *testing.T) {
	if got := Unsupported("claude", "SteerTurn").Error(); got != "runtime claude: SteerTurn not supported" {
		t.Errorf("Error() = %q", got)
	}
}

// Turn and Session are interfaces callers hold across goroutines; make sure the
// timestamps they expose are usable values, not a zero-value trap. (Compile
// guard as much as an assertion.)
func TestTurnInterface_Shape(t *testing.T) {
	var tr Turn = fakeTurn{id: "t1", session: "s1", started: time.Unix(1, 0)}
	if tr.ID() != "t1" || tr.SessionID() != "s1" || tr.StartedAt().IsZero() {
		t.Errorf("turn = %+v", tr)
	}
}

type fakeTurn struct {
	id      string
	session string
	started time.Time
}

func (f fakeTurn) ID() string           { return f.id }
func (f fakeTurn) SessionID() string    { return f.session }
func (f fakeTurn) StartedAt() time.Time { return f.started }
