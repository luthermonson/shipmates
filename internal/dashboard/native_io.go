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
	"time"

	"golang.org/x/term"
)

const (
	// resizePollInterval bounds how long a terminal resize stays invisible to
	// the dashboard. SIGWINCH has no Windows equivalent and term.GetSize is a
	// single cheap syscall, so one portable poll (only while waiting for
	// input) replaces two platform-specific notification paths.
	resizePollInterval = 250 * time.Millisecond
	// escapeReadTimeout resolves a bare ESC. A terminal sends ESC alone and
	// ESC-prefixed sequences down the same byte stream with nothing but
	// arrival time to distinguish them, so a continuation byte that does not
	// show up promptly means the user simply pressed Escape.
	escapeReadTimeout = 100 * time.Millisecond
	// maxEscapeBytes bounds a malformed or unterminated CSI sequence.
	maxEscapeBytes = 16
)

// errResized and errNoInput are internal control signals from the read helper.
// Neither escapes the package: Next translates errResized into InputResize and
// escapeInput treats errNoInput as "this was a bare ESC".
var (
	errResized = errors.New("terminal resized")
	errNoInput = errors.New("no input within the escape-sequence window")
)

// NativeEditor reads a real terminal. A single consumer goroutine drives Next;
// the renderer is the only other user of the struct and it takes mu.
type NativeEditor struct {
	in             *os.File
	out            *os.File
	mu             sync.Mutex
	b              strings.Builder
	eraseSubmitted bool

	// Stdin is drained by one dedicated goroutine because no portable
	// primitive cancels a blocking console read. The unix implementation used
	// unix.Poll with a 100ms timeout, which does not exist on Windows, and
	// CancelIoEx cannot be reached through an *os.File that Go owns. So the
	// read stays blocking and cancellation is handled by abandoning the
	// channel instead of interrupting the read: Next returns immediately on
	// ctx.Done, while the pump stays parked in Read until stdin yields a byte
	// or closes. Callers already run Next on its own goroutine and only wait a
	// bounded time for it during teardown, and the process is exiting by then,
	// so the cost is at most one goroutine blocked on stdin plus any bytes it
	// had read ahead. The previous poll loop dropped read-ahead bytes on
	// cancellation too.
	//
	// One consequence to know about when writing tests: os.File.Close waits
	// for an outstanding read on that handle, so closing the *read* end while
	// the pump is parked in it blocks. Close the write end (or the console)
	// first and the parked read returns.
	pump  sync.Once
	bytes chan readResult

	// size is the resize poll's view of the terminal, nil when there is no
	// output file to measure. lastSize is the geometry already reported.
	size     func() Size
	lastSize Size
	pending  Size
}

type readResult struct {
	b   byte
	err error
}

func NewNativeEditor(in *os.File, out ...*os.File) *NativeEditor {
	e := &NativeEditor{in: in}
	if len(out) > 0 && out[0] != nil {
		e.out = out[0]
		e.size = func() Size { return NativeSize(e.out) }
		e.lastSize = e.size()
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
			if errors.Is(err, errResized) {
				return Input{Kind: InputResize, Size: e.pending}, nil
			}
			if errors.Is(err, io.EOF) {
				return Input{Kind: InputEOF}, nil
			}
			// Cancellation can land inside the blocking read just as easily as
			// before the top-of-loop select; both must yield the same
			// InputCancel so callers see one cancellation contract.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Input{Kind: InputCancel}, nil
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

// readByte waits for the next keystroke and reports a terminal resize while it
// waits. Only the top of Next uses it, so a resize can never split a
// multi-byte escape sequence.
func (e *NativeEditor) readByte(ctx context.Context) (byte, error) {
	return e.read(ctx, true, 0)
}

// readEscapeByte waits only briefly and ignores resizes: an unfinished escape
// sequence must not be interrupted, and a continuation byte that never arrives
// has to be distinguishable from one that is merely slow.
func (e *NativeEditor) readEscapeByte(ctx context.Context) (byte, error) {
	return e.read(ctx, false, escapeReadTimeout)
}

func (e *NativeEditor) read(ctx context.Context, watchResize bool, timeout time.Duration) (byte, error) {
	e.startPump()
	// A byte the pump has already handed over wins over cancellation, the
	// resize poll, and the escape timeout, preserving the old poll loop's
	// "buffered input is delivered first" behavior.
	select {
	case r, ok := <-e.bytes:
		return deliver(r, ok)
	default:
	}
	var resize <-chan time.Time
	if watchResize && e.size != nil {
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()
		resize = ticker.C
	}
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		// A nil channel blocks forever, so each case is enabled purely by
		// whether its channel was set up above.
		select {
		case r, ok := <-e.bytes:
			return deliver(r, ok)
		case <-resize:
			if size := e.size(); size != e.lastSize {
				e.lastSize, e.pending = size, size
				return 0, errResized
			}
		case <-expired:
			return 0, errNoInput
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (e *NativeEditor) startPump() {
	e.pump.Do(func() {
		e.bytes = make(chan readResult, 64)
		if e.in == nil {
			close(e.bytes)
			return
		}
		in, out := e.in, e.bytes
		go func() {
			defer close(out)
			r := bufio.NewReaderSize(in, MaxInputBytes+2)
			for {
				b, err := r.ReadByte()
				if err != nil {
					out <- readResult{err: err}
					return
				}
				out <- readResult{b: b}
			}
		}()
	})
}

func deliver(r readResult, ok bool) (byte, error) {
	if !ok {
		return 0, io.EOF
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.b, nil
}

// escapeInput consumes exactly one CSI or SS3 sequence and maps the navigation
// keys the dashboard understands. It is deliberately structured around the
// sequence grammar rather than a list of terminator bytes: arrow keys are
// ESC [ A/B/C/D, whose finals are not `~`, `H`, or `F`, and a terminator scan
// therefore blocked and then swallowed the next several keystrokes typed.
func (e *NativeEditor) escapeInput(ctx context.Context) (Input, bool) {
	c, err := e.readEscapeByte(ctx)
	if err != nil {
		// Nothing followed the ESC: the user pressed Escape.
		return Input{}, false
	}
	var sequence strings.Builder
	sequence.WriteByte(c)
	switch c {
	case '[': // CSI: parameter and intermediate bytes, then one final byte.
		for sequence.Len() < maxEscapeBytes {
			if c, err = e.readEscapeByte(ctx); err != nil {
				return Input{}, false
			}
			sequence.WriteByte(c)
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
	case 'O': // SS3: exactly one byte follows.
		if c, err = e.readEscapeByte(ctx); err != nil {
			return Input{}, false
		}
		sequence.WriteByte(c)
	default:
		// ESC followed by anything else (Alt-key chords, say) is not a
		// sequence this dashboard interprets. Discarding that one byte is
		// bounded, unlike the previous scan for a terminator.
		return Input{}, false
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
		if w, _, err := term.GetSize(int(e.out.Fd())); err == nil && w > 0 {
			width = w
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
	if out == nil {
		return Size{Width: 80, Height: 24}
	}
	width, height, err := term.GetSize(int(out.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return Size{Width: 80, Height: 24}
	}
	height-- // Keep one stable row for the local input prompt.
	if height < 1 {
		height = 1
	}
	return Size{Width: width, Height: height}
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
	var previousSize Size
	return func(s Screen) error {
		if editor != nil {
			editor.mu.Lock()
			defer editor.mu.Unlock()
		}
		var text string
		if plain {
			// The guard puts the terminal in raw mode even with --plain, which
			// clears OPOST, so a bare \n moves down a row without returning to
			// column 0 and the transcript would stairstep off the screen.
			text = strings.Join(s.Lines, "\r\n") + "\r\n> "
		} else {
			colored := make([]string, len(s.Lines))
			for i, line := range s.Lines {
				colored[i] = colorizeDashboardLine(line, s.Persona, s.Planning)
			}
			var update strings.Builder
			update.WriteString("\x1b[?25l")
			if s.Size != previousSize {
				// Geometry changed: the console has already reflowed or
				// clipped whatever was on screen, so the row cache no longer
				// describes it and every row has to be repainted. The very
				// first frame needs no clear because the alternate screen is
				// already blank.
				if previousSize != (Size{}) {
					update.WriteString("\x1b[2J")
				}
				previous, previousSize = nil, s.Size
			}
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
