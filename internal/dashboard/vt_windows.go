//go:build windows

package dashboard

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// enableVirtualTerminal turns on ANSI escape interpretation for the console
// output handle and returns the reversal.
//
// x/term's MakeRaw only touches the *input* handle. It does set
// ENABLE_VIRTUAL_TERMINAL_INPUT there, so arrow, page, and home/end keys
// arrive as the same escape sequences a unix terminal sends and the editor's
// sequence parser needs no Windows-specific branch. Output is deliberately
// left alone, and without ENABLE_VIRTUAL_TERMINAL_PROCESSING every cursor
// address and color the renderer emits is printed as literal text. That makes
// this a hard requirement rather than an enhancement, so a console that
// refuses it (conhost older than Windows 10 1809) gets an honest error instead
// of a screen full of escape-sequence garbage.
func enableVirtualTerminal(out *os.File) (func() error, error) {
	if out == nil {
		return nil, nil
	}
	handle := windows.Handle(out.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return nil, fmt.Errorf("read console output mode: %w", err)
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return nil, nil
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, fmt.Errorf("this console cannot interpret ANSI escape sequences (ENABLE_VIRTUAL_TERMINAL_PROCESSING): %w; use Windows Terminal, or Windows 10 1809 or newer", err)
	}
	return func() error { return windows.SetConsoleMode(handle, mode) }, nil
}
