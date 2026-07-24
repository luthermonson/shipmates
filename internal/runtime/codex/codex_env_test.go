package codex

import (
	"context"
	"errors"
	"testing"

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
