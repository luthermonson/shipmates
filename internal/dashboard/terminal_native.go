package dashboard

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"
)

// nativeTerminal is the only real Terminal implementation and it is portable.
// Raw mode, the pre-mutation snapshot, and restoration all go through
// golang.org/x/term, which owns the per-platform termios names (TCGETS on
// linux, TIOCGETA on darwin/BSD) and the Win32 console modes. Everything the
// dashboard emits afterwards is plain ANSI, which every supported console
// understands once virtual-terminal processing is on.
type nativeTerminal struct {
	in, out *os.File
	// restoreVT reverses the console output-mode change made by SetRaw. It is
	// nil on unix and on a Windows console that already had VT processing.
	restoreVT func() error
}

func NewNativeTerminal(in, out *os.File) Terminal { return &nativeTerminal{in: in, out: out} }

func (t *nativeTerminal) StdinTTY() bool {
	return t.in != nil && term.IsTerminal(int(t.in.Fd()))
}

func (t *nativeTerminal) StdoutTTY() bool {
	return t.out != nil && term.IsTerminal(int(t.out.Fd()))
}

// Capture snapshots the terminal without mutating it so NewGuard can still
// refuse before Enter on a platform x/term has no implementation for. That is
// where ErrNativeTTYUnsupported now comes from: a real failure to read the
// terminal state, not a build tag.
func (t *nativeTerminal) Capture() (any, error) {
	state, err := term.GetState(int(t.in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNativeTTYUnsupported, err)
	}
	return state, nil
}

func (t *nativeTerminal) SetRaw(state any) error {
	if original, ok := state.(*term.State); !ok || original == nil {
		return fmt.Errorf("invalid terminal snapshot")
	}
	// MakeRaw returns the prior state, which Capture already holds. Restore is
	// always driven from the guard's snapshot so both paths reverse the same
	// thing exactly once.
	if _, err := term.MakeRaw(int(t.in.Fd())); err != nil {
		return err
	}
	restore, err := enableVirtualTerminal(t.out)
	if err != nil {
		return err
	}
	t.restoreVT = restore
	return nil
}

func (t *nativeTerminal) write(s string) error { _, err := t.out.WriteString(s); return err }

func (t *nativeTerminal) AlternateScreen(on bool) error {
	if on {
		return t.write("\x1b[?1049h")
	}
	return t.write("\x1b[?1049l")
}

func (t *nativeTerminal) CursorVisible(visible bool) error {
	if visible {
		return t.write("\x1b[?25h")
	}
	return t.write("\x1b[?25l")
}

func (t *nativeTerminal) Restore(state any) error {
	original, ok := state.(*term.State)
	if !ok || original == nil {
		return fmt.Errorf("invalid terminal snapshot")
	}
	// Both reversals are attempted even when the first one fails, matching the
	// guard's own all-reversals-attempted contract.
	var errs []error
	if t.restoreVT != nil {
		errs = append(errs, t.restoreVT())
		t.restoreVT = nil
	}
	errs = append(errs, term.Restore(int(t.in.Fd()), original))
	return errors.Join(errs...)
}
