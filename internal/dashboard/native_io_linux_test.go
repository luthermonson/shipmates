//go:build linux

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
	case <-time.After(300 * time.Millisecond):
		t.Fatal("idle native input reader did not stop after cancellation")
	}
}

func TestNativeRendererUpdatesChangedRowsWithoutFullScreenClear(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "renderer")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	render := NativeRenderer(file, false)
	if err := render(Screen{Lines: []string{"one", "two"}, Persona: "skipper", Planning: true}); err != nil {
		t.Fatal(err)
	}
	if err := render(Screen{Lines: []string{"one", "changed"}, Persona: "skipper", Planning: true}); err != nil {
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

func TestNativeEditorMapsPlanNavigationKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		sequence string
		scroll   int
	}{
		"page-up": {"\x1b[5~", -1}, "page-down": {"\x1b[6~", 1},
		"home": {"\x1b[H", -2}, "end": {"\x1b[F", 2},
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

// Moved from dashboard_test.go: colorizeDashboardLine only exists in the
// linux native-IO build, so an untagged test file referencing it broke
// `go test` compilation for this package on windows and darwin.
func TestDashboardColorsDistinguishPersonaStatusAndSidebar(t *testing.T) {
	line := colorizeDashboardLine("agent: hello │ VOYAGE PLAN", "skipper", true)
	if !strings.Contains(line, "\x1b[1;96magent: hello") || !strings.Contains(line, "\x1b[97mVOYAGE PLAN") {
		t.Fatalf("colored line=%q", line)
	}
	if colorizeDashboardLine("agent: hello", "skipper", false) == colorizeDashboardLine("agent: hello", "architect", false) {
		t.Fatal("persona colors should be stable and distinct for skipper and architect")
	}
}
