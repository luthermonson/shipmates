package dashboard

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeEditorCancellationInterruptsIdleRead(t *testing.T) {
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	editor := NewNativeEditor(in)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Input, 1)
	go func() {
		input, _ := editor.Next(ctx)
		done <- input
	}()
	cancel()
	select {
	case input := <-done:
		if input.Kind != InputCancel {
			t.Fatalf("input kind = %v", input.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle native input reader did not stop after cancellation")
	}
}

func TestNativeEditorReportsEOFWhenInputCloses(t *testing.T) {
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	editor := NewNativeEditor(in)
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	input, err := editor.Next(context.Background())
	if err != nil || input.Kind != InputEOF {
		t.Fatalf("input=%+v err=%v", input, err)
	}
}

func TestNativeRendererUpdatesChangedRowsWithoutFullScreenClear(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "renderer")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	render := NativeRenderer(file, false)
	size := Size{Width: 40, Height: 3}
	if err := render(Screen{Lines: []string{"one", "two"}, Persona: "skipper", Planning: true, Size: size}); err != nil {
		t.Fatal(err)
	}
	if err := render(Screen{Lines: []string{"one", "changed"}, Persona: "skipper", Planning: true, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	output := string(b)
	if strings.Contains(output, "\x1b[2J") {
		t.Fatalf("renderer cleared the full screen: %q", output)
	}
	if strings.Count(output, "\x1b[1;1H") != 1 || strings.Count(output, "\x1b[2;1H") != 2 {
		t.Fatalf("renderer did not retain unchanged rows: %q", output)
	}
}

// A resize invalidates the per-row cache: the console has already reflowed or
// clipped the old frame, so the next paint has to clear and redraw everything.
func TestNativeRendererRepaintsEveryRowAfterResize(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "renderer-resize")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	render := NativeRenderer(file, false)
	if err := render(Screen{Lines: []string{"one", "two"}, Size: Size{Width: 40, Height: 3}}); err != nil {
		t.Fatal(err)
	}
	before, err := file.Seek(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := render(Screen{Lines: []string{"one", "two"}, Size: Size{Width: 20, Height: 3}}); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4096)
	n, err := file.ReadAt(b, before)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	resized := string(b[:n])
	if !strings.Contains(resized, "\x1b[2J") {
		t.Fatalf("resize did not clear the stale frame: %q", resized)
	}
	if !strings.Contains(resized, "\x1b[1;1H") || !strings.Contains(resized, "\x1b[2;1H") {
		t.Fatalf("resize did not repaint unchanged rows: %q", resized)
	}
}

func TestNativeEditorMapsPlanNavigationKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		sequence string
		scroll   int
	}{
		"page-up": {"\x1b[5~", -1}, "page-down": {"\x1b[6~", 1},
		"home": {"\x1b[H", -2}, "end": {"\x1b[F", 2},
		"application-home": {"\x1bOH", -2}, "application-end": {"\x1bOF", 2},
	} {
		t.Run(name, func(t *testing.T) {
			in, out, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			defer out.Close()
			editor := NewNativeEditor(in)
			if _, err := out.WriteString(tc.sequence); err != nil {
				t.Fatal(err)
			}
			input, err := editor.Next(context.Background())
			if err != nil || input.Kind != InputScroll || input.Scroll != tc.scroll {
				t.Fatalf("input=%+v err=%v", input, err)
			}
		})
	}
}

// Arrow keys are ESC [ A/B/C/D. Their final byte is none of `~`, `H`, or `F`,
// so the old terminator scan blocked and then ate the next six characters the
// user typed. An unmapped sequence must be consumed whole and dropped.
func TestNativeEditorUnmappedEscapeSequenceDoesNotSwallowInput(t *testing.T) {
	for name, sequence := range map[string]string{
		"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
		"shift-tab": "\x1b[Z", "parameterized": "\x1b[1;5D",
	} {
		t.Run(name, func(t *testing.T) {
			in, out, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			defer out.Close()
			editor := NewNativeEditor(in)
			if _, err := out.WriteString(sequence + "hello\r"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			input, err := editor.Next(ctx)
			if err != nil || input.Kind != InputLine || input.Line != "hello" {
				t.Fatalf("input=%+v err=%v", input, err)
			}
		})
	}
}

// A bare ESC has no terminator at all; only the absence of a prompt
// continuation byte distinguishes it, so it is resolved by a read timeout.
func TestNativeEditorBareEscapeDoesNotSwallowInput(t *testing.T) {
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	editor := NewNativeEditor(in)
	// Drain one complete line first so the reader goroutine is provably parked
	// in Read before the lone ESC is written; otherwise ESC and the following
	// keystrokes could be delivered in the same read and the test would be
	// exercising the Alt-chord path instead.
	if _, err := out.WriteString("warm\r"); err != nil {
		t.Fatal(err)
	}
	if input, err := editor.Next(context.Background()); err != nil || input.Line != "warm" {
		t.Fatalf("warmup input=%+v err=%v", input, err)
	}
	if _, err := out.WriteString("\x1b"); err != nil {
		t.Fatal(err)
	}
	done := make(chan Input, 1)
	go func() {
		input, _ := editor.Next(context.Background())
		done <- input
	}()
	time.Sleep(3 * escapeReadTimeout)
	if _, err := out.WriteString("hello\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case input := <-done:
		if input.Kind != InputLine || input.Line != "hello" {
			t.Fatalf("bare escape swallowed input: %+v", input)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bare escape blocked the reader")
	}
}

// Nothing produced InputResize before: SIGWINCH was never watched and the size
// was sampled once at startup, so the cursor-addressed renderer kept drawing to
// the old geometry forever. term.GetSize is portable, so the editor now polls it
// while it is waiting for a keystroke.
func TestNativeEditorReportsTerminalResizeWhileIdle(t *testing.T) {
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	sink, err := os.CreateTemp(t.TempDir(), "resize")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	editor := NewNativeEditor(in, sink)
	// A temp file has no window, so the editor starts from NativeSize's
	// fallback; swap the measurement to stand in for a console being dragged.
	editor.size = func() Size { return Size{Width: 132, Height: 41} }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input, err := editor.Next(ctx)
	if err != nil || input.Kind != InputResize || input.Size != (Size{Width: 132, Height: 41}) {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	// The new geometry is reported once, not on every poll.
	editor.size = func() Size { return Size{Width: 132, Height: 41} }
	if _, err := out.WriteString("after\r"); err != nil {
		t.Fatal(err)
	}
	if input, err := editor.Next(ctx); err != nil || input.Kind != InputLine || input.Line != "after" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
}

func TestNativeEditorClearsSubmittedInteractiveInputInPlace(t *testing.T) {
	in, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inputWriter.Close()
	out, err := os.CreateTemp(t.TempDir(), "editor-output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	editor := NewNativeEditor(in, out)
	_ = NativeRenderer(out, false, editor)
	if _, err := inputWriter.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	input, err := editor.Next(context.Background())
	if err != nil || input.Line != "hello" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); !strings.HasSuffix(got, "hello\r\x1b[2K") || strings.Contains(got, "\r\n") {
		t.Fatalf("interactive submit was not cleared in place: %q", got)
	}
}

func TestNativeEditorHorizontallyScrollsLongInput(t *testing.T) {
	editor := NewNativeEditor(nil)
	editor.eraseSubmitted = true
	editor.b.WriteString(strings.Repeat("a", 20) + strings.Repeat("b", 80))

	editor.mu.Lock()
	display := editor.inputDisplayLocked()
	editor.mu.Unlock()
	if len([]rune(display)) != 76 {
		t.Fatalf("display width = %d, want 76", len([]rune(display)))
	}
	if strings.Contains(display, "a") || display != strings.Repeat("b", 76) {
		t.Fatalf("display did not retain only the input tail: %q", display)
	}
}

// Raw mode clears OPOST on unix, so the plain transcript must carry its own
// carriage returns or every line steps one column further right.
func TestPlainRendererTerminatesLinesForRawMode(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := NativeRenderer(file, true)(Screen{Lines: []string{"one", "two"}, Size: Size{Width: 40, Height: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if got != "one\r\ntwo\r\n> " {
		t.Fatalf("plain transcript = %q", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("plain transcript has a bare newline: %q", got)
	}
}

// The real terminal must be reachable through the portable Terminal seam on
// every platform this test runs on, even when the test process has no TTY.
func TestNativeTerminalRefusesNonTTYWithoutMutating(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	terminal := NewNativeTerminal(file, file)
	if terminal.StdinTTY() || terminal.StdoutTTY() {
		t.Fatal("a regular file reported itself as a terminal")
	}
	if _, err := NewGuard(terminal, false); err != ErrNotTTY {
		t.Fatalf("err=%v", err)
	}
}
