package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestErrUnsupported_TypeAssertsFromTheConcreteReturn(t *testing.T) {
	inner := Unsupported("claude", "Steer")
	// A bare errors.New that merely quotes the message is NOT the error —
	// callers must get the concrete type back from the runtime method, not
	// pattern-match on text.
	quoted := errors.New("something went wrong: " + inner.Error())
	var target *ErrUnsupported
	if errors.As(quoted, &target) {
		t.Error("a bare errors.New should not satisfy errors.As(*ErrUnsupported)")
	}
	var direct error = inner
	if !errors.As(direct, &target) {
		t.Fatal("the concrete error returned by a runtime method must type-assert")
	}
	if target.Runtime != "claude" || target.Feature != "Steer" {
		t.Errorf("target = %+v", target)
	}
}

func TestErrNotConfigured_Message(t *testing.T) {
	err := &ErrNotConfigured{Runtime: "codex", Reason: "no app-server transport"}
	want := `runtime "codex" not configured: no app-server transport`
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
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
		t.Error("SelectorFunc did not delegate to the inner func")
	}
}

func TestKind_Constants_Sensible(t *testing.T) {
	// Sanity: no accidental typo in the Kind constants — they must all be
	// non-empty, unique, and snake_case, because they cross package
	// boundaries as strings.
	kinds := []Kind{
		KindText, KindToolCall, KindToolResult, KindApprovalNeeded,
		KindTurnDone, KindSessionClosed, KindError, KindBackend,
	}
	seen := map[Kind]bool{}
	for _, k := range kinds {
		if k == "" {
			t.Error("empty Kind constant")
		}
		if seen[k] {
			t.Errorf("duplicate Kind %q", k)
		}
		seen[k] = true
		for _, r := range string(k) {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("Kind %q has uppercase — expected snake_case", k)
			}
			if r == ' ' || r == '\t' {
				t.Errorf("Kind %q contains whitespace", k)
			}
		}
	}
}

// Caps' zero value must mean "supports nothing optional", so that a runtime
// opts into a capability rather than forgetting to disclaim one. A field that
// defaulted to true would make every future implementation dishonest until it
// remembered to say otherwise.
func TestCaps_ZeroValueClaimsNothing(t *testing.T) {
	var c Caps
	claims := map[string]bool{
		"Streaming": c.Streaming, "Interrupt": c.Interrupt, "Steer": c.Steer,
		"Attachments": c.Attachments, "Refusal": c.Refusal,
		"Containment": c.Containment, "Environment": c.Environment,
		"Approvals": c.Approvals, "PersonaInstall": c.PersonaInstall,
		"MemoryHook": c.MemoryHook,
	}
	for name, claimed := range claims {
		if claimed {
			t.Errorf("zero-value Caps claims %s", name)
		}
	}
}
