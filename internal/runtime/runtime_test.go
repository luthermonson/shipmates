package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestErrUnsupported_Message(t *testing.T) {
	err := &ErrUnsupported{Runtime: "claude", Feature: "SteerTurn"}
	got := err.Error()
	want := "runtime claude: SteerTurn not supported"
	if got != want {
		t.Errorf("Error()=%q want %q", got, want)
	}
}

func TestErrUnsupported_ErrorsAs(t *testing.T) {
	inner := &ErrUnsupported{Runtime: "claude", Feature: "Steer"}
	wrapped := errors.New("something went wrong: " + inner.Error())
	// Not wrapped as unwrap-chain; consumers should type-assert on the
	// concrete error returned by runtime methods.
	var target *ErrUnsupported
	if errors.As(wrapped, &target) {
		t.Errorf("bare errors.New should not preserve *ErrUnsupported type")
	}
	// Confirm the type check works when the concrete error IS the return.
	var direct error = inner
	if !errors.As(direct, &target) {
		t.Errorf("direct *ErrUnsupported should type-assert")
	}
	if target.Runtime != "claude" || target.Feature != "Steer" {
		t.Errorf("target fields wrong: %+v", target)
	}
}

func TestErrNotConfigured_Message(t *testing.T) {
	err := &ErrNotConfigured{Runtime: "codex", Reason: "no app-server transport"}
	got := err.Error()
	want := `runtime "codex" not configured: no app-server transport`
	if got != want {
		t.Errorf("Error()=%q want %q", got, want)
	}
}

func TestSelectorFunc_Delegates(t *testing.T) {
	called := false
	f := SelectorFunc(func(_ context.Context, projectDir, cliRuntime string) (Runtime, string, error) {
		called = true
		if projectDir != "/tmp/proj" || cliRuntime != "claude" {
			t.Errorf("args not propagated: %q %q", projectDir, cliRuntime)
		}
		return nil, "test", nil
	})
	rt, src, err := f.Select(context.Background(), "/tmp/proj", "claude")
	if err != nil || rt != nil || src != "test" {
		t.Errorf("unexpected return: rt=%v src=%q err=%v", rt, src, err)
	}
	if !called {
		t.Error("SelectorFunc did not delegate to inner func")
	}
}

func TestKind_Constants_Sensible(t *testing.T) {
	// Sanity: no accidental typo in the Kind constants — they must all be
	// non-empty and lowercase-ish (no whitespace, no caps).
	kinds := []Kind{
		KindText, KindToolCall, KindToolResult, KindApprovalNeeded,
		KindTurnDone, KindSessionClosed, KindError, KindBackend,
	}
	seen := map[Kind]bool{}
	for _, k := range kinds {
		if k == "" {
			t.Errorf("empty Kind constant")
		}
		if seen[k] {
			t.Errorf("duplicate Kind %q", k)
		}
		seen[k] = true
		for _, r := range string(k) {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("Kind %q has uppercase — expected snake_case", k)
			}
		}
	}
}
