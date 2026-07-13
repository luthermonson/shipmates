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

	"golang.org/x/sys/unix"
)

type NativeEditor struct{ r *bufio.Reader }

func NewNativeEditor(in *os.File) *NativeEditor {
	return &NativeEditor{r: bufio.NewReaderSize(in, MaxInputBytes+2)}
}

func (e *NativeEditor) Next(ctx context.Context) (Input, error) {
	var b strings.Builder
	for {
		select {
		case <-ctx.Done():
			return Input{Kind: InputCancel}, nil
		default:
		}
		c, err := e.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Input{Kind: InputEOF}, nil
			}
			return Input{}, err
		}
		switch c {
		case 3:
			return Input{Kind: InputCancel}, nil
		case '\r', '\n':
			return Input{Kind: InputLine, Line: b.String()}, nil
		case 0x7f, '\b':
			if s := b.String(); s != "" {
				r := []rune(s)
				b.Reset()
				b.WriteString(string(r[:len(r)-1]))
			}
		default:
			if b.Len() <= MaxInputBytes {
				b.WriteByte(c)
			} else {
				return Input{Kind: InputLine, Line: b.String() + "x"}, nil
			}
		}
	}
}

func NativeSize(out *os.File) Size {
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return Size{Width: 80, Height: 24}
	}
	return Size{Width: int(ws.Col), Height: int(ws.Row)}
}

func NativeRenderer(out *os.File, plain bool) func(Screen) error {
	return func(s Screen) error {
		var text string
		if plain {
			text = strings.Join(s.Lines, "\n") + "\n"
		} else {
			text = "\x1b[H\x1b[2J" + strings.Join(s.Lines, "\r\n") + "\r\n"
		}
		_, err := fmt.Fprint(out, text)
		return err
	}
}
