package bridge

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// harness is a miniature Bubble Tea runtime: one goroutine calls Update, and
// every tea.Cmd runs concurrently and feeds its message back — the same contract
// the real program provides. That is what makes a TUI testable without a TTY:
// Update is a pure function of (Model, Msg), so driving it with synthetic
// KeyMsg/WindowSizeMsg and asserting on View() exercises the real code path.
type harness struct {
	t  *testing.T
	m  *Model
	in chan tea.Msg

	mu     sync.Mutex
	quit   bool
	inFlgt int
}

func newHarness(t *testing.T, m *Model) *harness {
	t.Helper()
	h := &harness{t: t, m: m, in: make(chan tea.Msg, 1024)}
	t.Cleanup(m.Close)
	// Arm the stream-queue reader, exactly as Init does. Without it, frames from
	// the stream goroutines would pile up in m.msgs and never reach Update.
	h.exec(m.readQueue())
	return h
}

func (h *harness) add(n int) {
	h.mu.Lock()
	h.inFlgt += n
	h.mu.Unlock()
}

// working reports how many commands are doing actual work. Queue readers parked on
// m.msgs are subtracted: they block until a mate produces output, which may be
// never, so counting them would mean the model never looks quiet.
func (h *harness) working() int {
	h.mu.Lock()
	n := h.inFlgt
	h.mu.Unlock()
	return n - int(h.m.readers.Load())
}

// send enqueues a message for Update.
func (h *harness) send(msgs ...tea.Msg) {
	for _, msg := range msgs {
		h.in <- msg
	}
}

// key enqueues a keypress by its bubbletea name ("a", "ctrl+b", "enter", …).
func (h *harness) key(names ...string) {
	for _, n := range names {
		h.send(keyMsg(n))
	}
}

// settle runs the update loop to quiescence: it processes messages until the
// queue is empty AND no command is still running. Waiting on the in-flight count
// rather than on a timeout is what makes the assertions deterministic — a POST
// that takes longer than a fixed quiet window (which happens under full-suite
// load) used to let a test assert before the request had landed.
//
// Returns true if it went quiet rather than exhausting its budget.
func (h *harness) settle() bool {
	h.t.Helper()
	const quiet = 150 * time.Millisecond
	deadline := time.Now().Add(15 * time.Second)
	for {
		if time.Now().After(deadline) {
			h.t.Errorf("harness did not settle within 15s (%d commands working)", h.working())
			return false
		}
		select {
		case msg := <-h.in:
			h.dispatch(msg)
			continue
		default:
		}
		if h.working() > 0 {
			// Commands are still running. Their results will arrive on h.in;
			// yield rather than spin.
			time.Sleep(time.Millisecond)
			continue
		}
		// Queue drained and nothing working: a real quiescent point, because every
		// command pushes its message before its counter drops. The window below
		// covers the one thing the counter cannot see — a frame still travelling
		// from the fake ship through the SSE parser into m.msgs.
		select {
		case msg := <-h.in:
			h.dispatch(msg)
		case <-time.After(quiet):
			return true
		}
	}
}

func (h *harness) dispatch(msg tea.Msg) {
	if msg == nil {
		return
	}
	if _, isQuit := msg.(tea.QuitMsg); isQuit {
		h.mu.Lock()
		h.quit = true
		h.mu.Unlock()
		return
	}
	_, cmd := h.m.Update(msg)
	h.exec(cmd)
}

func (h *harness) quitted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quit
}

// exec runs a command off the Update goroutine and routes its result back, the
// same way Bubble Tea's event loop does:
//
//   - a plain command runs in its own goroutine, concurrently with every other
//     command. That is the real contract, and reproducing it here is what exposed
//     the keystroke-ordering bug that inputPipe now fixes;
//   - tea.BatchMsg children also run concurrently, with no ordering;
//   - tea.Sequence's message type is unexported but is also a []tea.Cmd, so it is
//     recognised by conversion and its children run strictly in order.
func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.add(1)
	go func() {
		defer h.add(-1)
		h.run(cmd)
	}()
}

// run executes one command on the current goroutine and routes its result.
func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			h.exec(c)
		}
		return
	}
	if cmds, ok := asCmdSlice(msg); ok {
		// sequenceMsg: one at a time, in order, on this goroutine.
		for _, c := range cmds {
			h.run(c)
		}
		return
	}
	h.in <- msg
}

var cmdSliceType = reflect.TypeOf([]tea.Cmd(nil))

func asCmdSlice(msg tea.Msg) ([]tea.Cmd, bool) {
	v := reflect.ValueOf(msg)
	if !v.IsValid() || v.Kind() != reflect.Slice {
		return nil, false
	}
	if !v.Type().ConvertibleTo(cmdSliceType) {
		return nil, false
	}
	return v.Convert(cmdSliceType).Interface().([]tea.Cmd), true
}

// view renders and strips styling, so assertions read on content rather than on
// escape sequences.
func (h *harness) view() string {
	h.t.Helper()
	return ansi.Strip(h.m.View())
}

// wantView fails unless every needle appears in the stripped view.
func (h *harness) wantView(needles ...string) {
	h.t.Helper()
	v := h.view()
	for _, n := range needles {
		if !strings.Contains(v, n) {
			h.t.Errorf("view missing %q\n--- view ---\n%s\n------------", n, v)
		}
	}
}

func (h *harness) wantNotView(needles ...string) {
	h.t.Helper()
	v := h.view()
	for _, n := range needles {
		if strings.Contains(v, n) {
			h.t.Errorf("view unexpectedly contains %q\n--- view ---\n%s\n------------", n, v)
		}
	}
}

// paneText returns just the terminal-pane rows of the stripped view.
func (h *harness) paneText() string {
	h.t.Helper()
	lines := strings.Split(h.view(), "\n")
	if len(lines) < 4 {
		return ""
	}
	_, rows := h.m.paneSize()
	end := min(2+rows, len(lines))
	return strings.Join(lines[2:end], "\n")
}

func (h *harness) tabOf(persona string) tabState {
	h.t.Helper()
	for _, st := range h.m.tabStates() {
		if st.Persona == persona {
			return st
		}
	}
	h.t.Fatalf("no tab for %q (have %+v)", persona, h.m.tabStates())
	return tabState{}
}

// keyMsg builds a KeyMsg from a bubbletea key name, so tests read like the
// keystrokes an operator actually makes. "alt+" is a prefix on any other name, so
// "alt+2" and "alt+esc" both work and produce the same shape bubbletea does: the
// base key with Alt set, NOT a rune sequence containing the word "esc".
func keyMsg(name string) tea.KeyMsg {
	alt := false
	if rest, ok := strings.CutPrefix(name, "alt+"); ok {
		alt, name = true, rest
	}
	k := baseKeyMsg(name)
	k.Alt = alt
	return k
}

func baseKeyMsg(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	if strings.HasPrefix(name, "ctrl+") && len(name) == 6 {
		c := name[5]
		if c >= 'a' && c <= 'z' {
			return tea.KeyMsg{Type: tea.KeyType(c - 'a' + 1)}
		}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// typeAt focuses the current mate for typing, the way an operator does: one enter
// in Navigate mode. It fails the test if the model did not end up in Type mode,
// because every pass-through assertion below it would otherwise be vacuous.
func (h *harness) typeAt() {
	h.t.Helper()
	h.key("enter")
	h.settle()
	if h.m.mode != modeType {
		h.t.Fatalf("enter did not enter Type mode (mode = %v, tabs = %+v)", h.m.mode, h.m.tabStates())
	}
}

// typeString turns text into one KeyMsg per rune.
func typeString(s string) []tea.Msg {
	out := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		if r == ' ' {
			out = append(out, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return out
}
