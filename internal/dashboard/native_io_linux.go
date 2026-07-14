//go:build linux

package dashboard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type NativeEditor struct {
	in             *os.File
	r              *bufio.Reader
	out            *os.File
	mu             sync.Mutex
	b              strings.Builder
	eraseSubmitted bool
}

func NewNativeEditor(in *os.File, out ...*os.File) *NativeEditor {
	e := &NativeEditor{in: in, r: bufio.NewReaderSize(in, MaxInputBytes+2)}
	if len(out) > 0 {
		e.out = out[0]
	}
	return e
}

func (e *NativeEditor) Next(ctx context.Context) (Input, error) {
	for {
		select {
		case <-ctx.Done():
			return Input{Kind: InputCancel}, nil
		default:
		}
		c, err := e.readByte(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Input{Kind: InputEOF}, nil
			}
			return Input{}, err
		}
		switch c {
		case 3:
			return Input{Kind: InputCancel}, nil
		case 0x1b:
			if input, ok := e.escapeInput(ctx); ok {
				return input, nil
			}
		case '\r', '\n':
			e.mu.Lock()
			line := e.b.String()
			e.b.Reset()
			if e.eraseSubmitted {
				e.writeLocked("\r\x1b[2K")
			} else {
				e.writeLocked("\r\n")
			}
			e.mu.Unlock()
			return Input{Kind: InputLine, Line: line}, nil
		case 0x7f, '\b':
			e.mu.Lock()
			if s := e.b.String(); s != "" {
				r := []rune(s)
				e.b.Reset()
				e.b.WriteString(string(r[:len(r)-1]))
				if e.eraseSubmitted {
					e.redrawInputLocked()
				} else {
					e.writeLocked("\b \b")
				}
			}
			e.mu.Unlock()
		default:
			e.mu.Lock()
			if e.b.Len() <= MaxInputBytes {
				e.b.WriteByte(c)
				if e.eraseSubmitted {
					e.redrawInputLocked()
				} else {
					e.writeLocked(string([]byte{c}))
				}
			} else {
				line := e.b.String() + "x"
				e.b.Reset()
				e.mu.Unlock()
				return Input{Kind: InputLine, Line: line}, nil
			}
			e.mu.Unlock()
		}
	}
}

func (e *NativeEditor) readByte(ctx context.Context) (byte, error) {
	for e.r.Buffered() == 0 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		fds := []unix.PollFd{{Fd: int32(e.in.Fd()), Events: unix.POLLIN | unix.POLLHUP}}
		if _, err := unix.Poll(fds, 100); err != nil && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
		if fds[0].Revents == 0 {
			continue
		}
		break
	}
	return e.r.ReadByte()
}

func (e *NativeEditor) escapeInput(ctx context.Context) (Input, bool) {
	var sequence strings.Builder
	for sequence.Len() < 8 {
		c, err := e.readByte(ctx)
		if err != nil {
			return Input{}, false
		}
		sequence.WriteByte(c)
		if c == '~' || c == 'H' || c == 'F' {
			break
		}
	}
	switch sequence.String() {
	case "[5~":
		return Input{Kind: InputScroll, Scroll: -1}, true
	case "[6~":
		return Input{Kind: InputScroll, Scroll: 1}, true
	case "[H", "OH", "[1~":
		return Input{Kind: InputScroll, Scroll: -2}, true
	case "[F", "OF", "[4~":
		return Input{Kind: InputScroll, Scroll: 2}, true
	default:
		return Input{}, false
	}
}

func (e *NativeEditor) writeLocked(s string) {
	if e.out != nil {
		_, _ = e.out.WriteString(s)
	}
}

func (e *NativeEditor) inputDisplayLocked() string {
	value := e.b.String()
	if !e.eraseSubmitted {
		return value
	}
	width := 80
	if e.out != nil {
		if ws, err := unix.IoctlGetWinsize(int(e.out.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
			width = int(ws.Col)
		}
	}
	// Reserve the prompt and the final terminal column. Writing into the final
	// column can trigger an automatic wrap on otherwise well-behaved terminals.
	available := width - 4
	if available < 1 {
		available = 1
	}
	runes := []rune(value)
	if len(runes) > available {
		runes = runes[len(runes)-available:]
	}
	return string(runes)
}

func (e *NativeEditor) redrawInputLocked() {
	e.writeLocked("\r\x1b[2K\x1b[1;32m>\x1b[0m " + e.inputDisplayLocked())
}

func NativeSize(out *os.File) Size {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return Size{Width: 80, Height: 24}
	}
	height := int(ws.Row) - 1 // Keep one stable row for the local input prompt.
	if height < 1 {
		height = 1
	}
	return Size{Width: int(ws.Col), Height: height}
}

func NativeRenderer(out *os.File, plain bool, editors ...*NativeEditor) func(Screen) error {
	var editor *NativeEditor
	if len(editors) > 0 {
		editor = editors[0]
		editor.mu.Lock()
		editor.eraseSubmitted = !plain
		editor.mu.Unlock()
	}
	var previous []string
	return func(s Screen) error {
		if editor != nil {
			editor.mu.Lock()
			defer editor.mu.Unlock()
		}
		var text string
		if plain {
			text = strings.Join(s.Lines, "\n") + "\n> "
		} else {
			colored := make([]string, len(s.Lines))
			for i, line := range s.Lines {
				colored[i] = colorizeDashboardLine(line, s.Persona, s.Planning)
			}
			var update strings.Builder
			update.WriteString("\x1b[?25l")
			rows := len(s.Lines)
			if len(previous) > rows {
				rows = len(previous)
			}
			for i := 0; i < rows; i++ {
				var current, old string
				if i < len(s.Lines) {
					current = s.Lines[i]
				}
				if i < len(previous) {
					old = previous[i]
				}
				if current == old {
					continue
				}
				fmt.Fprintf(&update, "\x1b[%d;1H\x1b[2K", i+1)
				if i < len(colored) {
					update.WriteString(colored[i])
				}
			}
			if len(previous) > len(s.Lines) {
				fmt.Fprintf(&update, "\x1b[%d;1H\x1b[2K", len(previous)+1)
			}
			fmt.Fprintf(&update, "\x1b[%d;1H\x1b[2K\x1b[1;32m>\x1b[0m ", len(s.Lines)+1)
			text = update.String()
			previous = append(previous[:0], s.Lines...)
		}
		if editor != nil {
			text += editor.inputDisplayLocked()
		}
		if !plain {
			text += "\x1b[?25h"
		}
		_, err := fmt.Fprint(out, text)
		return err
	}
}

func colorizeDashboardLine(line, persona string, planning bool) string {
	const reset = "\x1b[0m"
	left, right, hasSidebar := strings.Cut(line, " │ ")
	color := personaANSI(persona)
	if planning {
		color = "\x1b[1;96m"
	}
	switch {
	case strings.HasPrefix(left, "shipmates open"):
		color = "\x1b[1;36m"
	case strings.HasPrefix(left, "activity:"):
		color = "\x1b[33m"
	case strings.HasPrefix(left, "session "), strings.HasPrefix(left, "turn "):
		color = "\x1b[2;37m"
	case strings.HasPrefix(left, "/"):
		color = "\x1b[36m"
	}
	styled := color + left + reset
	if hasSidebar {
		styled += " \x1b[2;37m│\x1b[0m \x1b[97m" + right + reset
	}
	return styled
}

func personaANSI(persona string) string {
	palette := [...]string{"\x1b[32m", "\x1b[35m", "\x1b[34m", "\x1b[36m", "\x1b[92m", "\x1b[95m", "\x1b[94m", "\x1b[96m"}
	var hash uint32
	for _, r := range persona {
		hash = hash*33 + uint32(r)
	}
	return palette[hash%uint32(len(palette))]
}
