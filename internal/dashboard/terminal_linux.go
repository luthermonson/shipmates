//go:build linux

package dashboard

import (
	"fmt"
	"os"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

type nativeTerminal struct{ in, out *os.File }

func NewNativeTerminal(in, out *os.File) Terminal { return &nativeTerminal{in: in, out: out} }
func (t *nativeTerminal) StdinTTY() bool          { return t.in != nil && isatty.IsTerminal(t.in.Fd()) }
func (t *nativeTerminal) StdoutTTY() bool         { return t.out != nil && isatty.IsTerminal(t.out.Fd()) }
func (t *nativeTerminal) Capture() (any, error) {
	return unix.IoctlGetTermios(int(t.in.Fd()), unix.TCGETS)
}
func (t *nativeTerminal) SetRaw(state any) error {
	original, ok := state.(*unix.Termios)
	if !ok || original == nil {
		return fmt.Errorf("invalid terminal snapshot")
	}
	raw := *original
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	return unix.IoctlSetTermios(int(t.in.Fd()), unix.TCSETS, &raw)
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
	original, ok := state.(*unix.Termios)
	if !ok || original == nil {
		return fmt.Errorf("invalid terminal snapshot")
	}
	return unix.IoctlSetTermios(int(t.in.Fd()), unix.TCSETS, original)
}
