package bridge

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// setup builds a bridge against a fake ship with the pollers effectively
// disabled, so tests drive the polls explicitly and stay deterministic.
func setup(t *testing.T, f *fakeShip, tweak ...func(*Options)) *harness {
	t.Helper()
	o := Options{
		Base:            f.base(),
		StatusInterval:  time.Hour,
		PendingInterval: time.Hour,
	}
	for _, fn := range tweak {
		fn(&o)
	}
	h := newHarness(t, New(o))
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.settle()
	return h
}

// poll runs one status + pending fetch and lets everything it triggers finish.
func (h *harness) poll() {
	h.t.Helper()
	h.exec(h.m.fetchStatus())
	h.exec(h.m.fetchPending())
	h.settle()
}

func TestInitIssuesPollsAndQueueReader(t *testing.T) {
	f := newFakeShip(t)
	m := New(Options{Base: f.base()})
	defer m.Close()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	msg := cmd()
	cmds, ok := asCmdSlice(msg)
	if !ok {
		t.Fatalf("Init did not return a batch, got %T", msg)
	}
	if len(cmds) != 4 {
		t.Errorf("Init batched %d commands, want 4 (queue reader, status, pending, tick)", len(cmds))
	}
}

func TestTabsComeFromStatusJSONAndOnlyLiveMates(t *testing.T) {
	f := newFakeShip(t)
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "working"},
		MateStatus{Persona: "sleeper", Status: "off"},  // never ran: no tab
		MateStatus{Persona: "retired", Status: "done"}, // exited: no tab
	)
	h := setup(t, f)
	h.poll()

	var got []string
	for _, st := range h.m.tabStates() {
		got = append(got, st.Persona)
	}
	want := []string{"cooper", "rigger"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tabs = %v, want %v", got, want)
	}
	h.wantView("cooper", "rigger")
	h.wantNotView("sleeper", "retired")
}

func TestAttachPrimesFromSnapshotSoTabIsNotBlank(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "\x1b[2J\x1b[Hhello from rigger\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})

	h := setup(t, f)
	h.poll()

	if st := h.tabOf("rigger"); !st.Attached {
		t.Fatalf("rigger not attached: %+v", st)
	}
	if got := h.paneText(); !strings.Contains(got, "hello from rigger") {
		t.Errorf("pane was not primed from the snapshot:\n%s", got)
	}
}

func TestStreamSnapshotResetsAndCarriesModes(t *testing.T) {
	f := newFakeShip(t)
	// Ring content that would duplicate if the second prime stacked instead of
	// resetting. The stream's snapshot prefixes ?2004h, which the plain snapshot
	// endpoint does not.
	f.spawnPTY("rigger", "line-one\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	pane := h.paneText()
	if n := strings.Count(pane, "line-one"); n != 1 {
		t.Errorf("snapshot applied %d times, want exactly 1:\n%s", n, pane)
	}
	tab := h.m.tab("rigger")
	if tab == nil || tab.screen == nil {
		t.Fatal("no screen")
	}
	if !tab.screen.Mode(2004) {
		t.Error("bracketed-paste mode from the stream snapshot prefix was not latched")
	}
}

func TestKeystrokesReachTheFocusedMateOnly(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()
	// cooper sorts first, so it is focused. A long-ish string is deliberate: every
	// keystroke is a separate tea.Cmd and Bubble Tea runs commands concurrently, so
	// nothing but inputPipe keeps these in order. Two characters would let a
	// reordering bug pass half the time.
	h.typeAt()
	const typed = "git commit -m wip"
	h.send(typeString(typed)...)
	h.send(keyMsg("enter"))
	h.settle()

	if got := f.inputText("cooper"); got != typed+"\r" {
		t.Errorf("cooper received %q, want %q", got, typed+"\r")
	}
	if got := f.inputText("rigger"); got != "" {
		t.Errorf("rigger received %q, want nothing", got)
	}
}

// TestKeystrokeOrderSurvivesConcurrentDelivery is the regression guard for the
// ordering bug directly: a long burst of single-key commands must arrive as one
// in-order byte stream. Bubble Tea gives tea.Cmds no ordering (its own BatchMsg
// docs say so), so the order has to be established when the bytes are queued.
func TestKeystrokeOrderSurvivesConcurrentDelivery(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	h.typeAt()
	want := strings.Repeat("abcdefghij", 12) // 120 keystrokes
	h.send(typeString(want)...)
	h.settle()

	if got := f.inputText("rigger"); got != want {
		t.Errorf("keystrokes arrived out of order or incomplete:\n got %q\nwant %q", got, want)
	}
}

func TestSwitchingTabsKeepsBothStreamsAlive(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()

	// Switch to rigger, which attaches it, then back to cooper.
	h.key("n")
	h.settle()
	if h.m.currentPersona() != "rigger" {
		t.Fatalf("focus = %q, want rigger", h.m.currentPersona())
	}
	h.key("p")
	h.settle()
	if h.m.currentPersona() != "cooper" {
		t.Fatalf("focus = %q, want cooper", h.m.currentPersona())
	}

	// Both tabs must still be attached, and output to the BACKGROUND mate must
	// still land in its own emulator.
	for _, p := range []string{"cooper", "rigger"} {
		if st := h.tabOf(p); !st.Attached {
			t.Errorf("%s lost its stream after tab switching: %+v", p, st)
		}
	}
	f.push("rigger", "background work\r\n")
	h.settle()
	if st := h.tabOf("rigger"); !st.Activity {
		t.Error("background output did not mark activity on the rigger tab")
	}
	h.key("2")
	h.settle()
	if got := h.paneText(); !strings.Contains(got, "background work") {
		t.Errorf("background output was lost:\n%s", got)
	}
}

func TestWindowResizePropagatesToPTY(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	h.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	h.settle()

	_, resizes, _ := f.snapshot()
	if len(resizes) == 0 {
		t.Fatal("no resize reached the server")
	}
	last := resizes[len(resizes)-1]
	wantCols, wantRows := h.m.paneSize()
	if last.Persona != "rigger" || last.Cols != wantCols || last.Rows != wantRows {
		t.Errorf("resize = %+v, want rigger %dx%d", last, wantCols, wantRows)
	}
	// The emulator must have reflowed too, or the pane would render stale rows.
	cols, rows := h.m.tab("rigger").screen.Size()
	if cols != wantCols || rows != wantRows {
		t.Errorf("emulator size = %dx%d, want %dx%d", cols, rows, wantCols, wantRows)
	}
}

func TestPendingApprovalIsVisibleOnABackgroundTab(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "blocked"},
	)
	f.setPending(Pending{ID: "abc123", Persona: "rigger", Tool: "Bash", Input: "rm -rf /tmp/x"})
	h := setup(t, f)
	h.poll()

	if h.m.currentPersona() == "rigger" {
		// focusInitial prefers a blocked mate; force focus elsewhere so the
		// approval really is on a BACKGROUND tab.
		h.key("1")
		h.settle()
	}
	if h.m.currentPersona() == "rigger" {
		t.Fatal("could not move focus away from rigger")
	}

	v := h.view()
	// The tab bar must carry the badge...
	if !strings.Contains(v, "rigger "+glyphBlocked+"1") {
		t.Errorf("tab bar has no approval badge for the background mate:\n%s", v)
	}
	// ...and the alert bar must name the mate so the operator knows where to go.
	if !strings.Contains(v, "approvals waiting on rigger(1)") {
		t.Errorf("alert bar does not name the blocked background mate:\n%s", v)
	}
	// ...and the header must show the global count.
	if !strings.Contains(v, "1 waiting") {
		t.Errorf("header has no global approval count:\n%s", v)
	}
}

func TestApprovalsOverlayAllowAndDeny(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "blocked"})
	f.setPending(
		Pending{ID: "id-one", Persona: "rigger", Tool: "Bash", Input: "make test"},
		Pending{ID: "id-two", Persona: "rigger", Tool: "Write", Input: "/etc/hosts"},
	)
	h := setup(t, f)
	h.poll()

	h.key("a")
	h.settle()
	h.wantView("approvals — 2 waiting", "rigger wants Bash", "make test", "rigger wants Write")

	// allow the first, deny the second
	h.key("a")
	h.settle()
	h.key("d")
	h.settle()

	_, _, resolves := f.snapshot()
	if len(resolves) != 2 {
		t.Fatalf("resolves = %+v, want 2", resolves)
	}
	if resolves[0] != (resolveCall{ID: "id-one", Behavior: "allow"}) {
		t.Errorf("first resolve = %+v", resolves[0])
	}
	if resolves[1] != (resolveCall{ID: "id-two", Behavior: "deny"}) {
		t.Errorf("second resolve = %+v", resolves[1])
	}
	// With nothing left, the overlay closes on its own.
	if h.m.overlay == overlayApprovals {
		t.Error("approvals overlay stayed open with an empty list")
	}
}

func TestApprovalTimeBoxSendsDuration(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "blocked"})
	f.setPending(Pending{ID: "tb1", Persona: "rigger", Tool: "Bash", Input: "go test ./..."})
	h := setup(t, f)
	h.poll()
	h.key("a")
	h.settle()
	h.key("A")
	h.settle()

	_, _, resolves := f.snapshot()
	if len(resolves) != 1 || resolves[0].Duration != timeBoxDuration {
		t.Fatalf("resolves = %+v, want one allow with duration %q", resolves, timeBoxDuration)
	}
	if resolves[0].Behavior != "allow" {
		t.Errorf("time-box resolve behavior = %q", resolves[0].Behavior)
	}
}

func TestApprovalDenyAllForPersona(t *testing.T) {
	f := newFakeShip(t)
	// Both need a PTY: otherwise the focused mate is blocked with no terminal
	// and the attach grace-period prompt captures the keys instead.
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "blocked"},
		MateStatus{Persona: "rigger", Status: "blocked"},
	)
	f.setPending(
		Pending{ID: "r1", Persona: "rigger", Tool: "Bash", Input: "one"},
		Pending{ID: "r2", Persona: "rigger", Tool: "Bash", Input: "two"},
		Pending{ID: "c1", Persona: "cooper", Tool: "Bash", Input: "three"},
	)
	h := setup(t, f)
	h.poll()
	h.key("a")
	h.settle()
	if h.m.overlay != overlayApprovals {
		t.Fatalf("approvals overlay did not open, overlay = %v", h.m.overlay)
	}
	// Move the cursor onto a rigger row, then deny all of rigger's.
	for i := 0; i < len(h.m.pendings) && h.m.pendings[h.m.approvalIdx].Persona != "rigger"; i++ {
		h.key("j")
		h.settle()
	}
	if h.m.pendings[h.m.approvalIdx].Persona != "rigger" {
		t.Fatalf("could not reach a rigger row; cursor on %+v", h.m.pendings[h.m.approvalIdx])
	}
	h.key("D")
	h.settle()

	_, _, resolves := f.snapshot()
	if len(resolves) != 2 {
		t.Fatalf("resolves = %+v, want 2 (both rigger rows)", resolves)
	}
	for _, r := range resolves {
		if r.Behavior != "deny" || (r.ID != "r1" && r.ID != "r2") {
			t.Errorf("unexpected resolve %+v", r)
		}
	}
}

func TestApprovalGoJumpsToThePane(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "blocked"},
	)
	f.setPending(Pending{ID: "z1", Persona: "rigger", Tool: "Bash", Input: "x"})
	h := setup(t, f)
	h.poll()
	h.key("1") // focus cooper
	h.settle()
	h.key("a")
	h.settle()
	h.key("g")
	h.settle()
	if h.m.currentPersona() != "rigger" {
		t.Errorf("g did not jump to the blocked mate, focus = %q", h.m.currentPersona())
	}
	if h.m.overlay != overlayNone {
		t.Error("overlay stayed open after g")
	}
}

func TestApprovalsOverlayDoesNotLeakKeysToTheMate(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "blocked"})
	f.setPending(Pending{ID: "k1", Persona: "rigger", Tool: "Bash", Input: "x"})
	h := setup(t, f)
	h.poll()
	h.key("a")
	h.settle()
	// j/k/g are overlay navigation; none may reach the mate's PTY.
	h.key("j", "k", "g")
	h.settle()
	if got := f.inputText("rigger"); got != "" {
		t.Errorf("overlay leaked %q into the mate", got)
	}
}

func TestBellRingsOnceForANewApproval(t *testing.T) {
	f := newFakeShip(t)
	f.setRoster(MateStatus{Persona: "rigger", Status: "blocked"})
	var bell strings.Builder
	h := setup(t, f, func(o *Options) { o.Bell = &bell })

	f.setPending(Pending{ID: "b1", Persona: "rigger", Tool: "Bash"})
	h.poll()
	if bell.String() != "\a" {
		t.Fatalf("bell = %q, want one BEL", bell.String())
	}
	// The same request on the next poll must not ring again.
	h.poll()
	if bell.String() != "\a" {
		t.Errorf("bell rang again for a known approval: %q", bell.String())
	}
	// A genuinely new one does.
	f.setPending(
		Pending{ID: "b1", Persona: "rigger", Tool: "Bash"},
		Pending{ID: "b2", Persona: "rigger", Tool: "Write"},
	)
	h.poll()
	if bell.String() != "\a\a" {
		t.Errorf("bell = %q, want two BELs", bell.String())
	}
}

func TestMateBellIsNeverForwardedToTheRealTerminal(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	var bell strings.Builder
	h := setup(t, f, func(o *Options) { o.Bell = &bell })
	h.poll()

	// A mate spamming BEL must not be able to ring the operator's terminal.
	f.push("rigger", "\a\a\a\a\a\a\a\a")
	h.settle()
	if bell.String() != "" {
		t.Errorf("agent output rang the bell: %q", bell.String())
	}
}

// Focusing a mate for typing must claim the writer lease on its own. The operator
// never asks for a "takeover": there is nobody to arbitrate with on a pod, so the
// claim is a consequence of pressing enter and is not narrated.
func TestFocusingAMateClaimsTheKeyboardWithoutOperatorAction(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	// Someone else holds the lease. The operator should not have to know.
	f.lock("rigger", "some-browser-tab")
	h := setup(t, f)
	h.poll()

	if _, claims, _ := f.counts(); claims != 0 {
		t.Fatalf("claimed the keyboard before the operator focused anything (%d)", claims)
	}
	h.typeAt()

	if _, claims, _ := f.counts(); claims != 1 {
		t.Errorf("claims = %d, want exactly 1 on focus", claims)
	}
	if st := h.tabOf("rigger"); !st.Writer || st.Locked {
		t.Errorf("tab state after focus = %+v, want writer and unlocked", st)
	}
	// And the word "takeover" is nowhere on screen.
	h.wantNotView("takeover", "take over", "view-only", "VIEW-ONLY")

	// Typing works immediately, with no intervening keystroke.
	h.send(typeString("ok")...)
	h.settle()
	if got := f.inputText("rigger"); got != "ok" {
		t.Errorf("mate received %q, want %q", got, "ok")
	}
}

// A genuine 409 is one line in the status line plus a retry key. It must not become
// a mode the operator has to escape from.
func TestRefusedClaimIsAStatusLineWithARetry(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()

	// Another viewer grabs the lease after we claimed it, so the next keystroke 409s.
	f.lock("rigger", "some-browser-tab")
	h.send(typeString("x")...)
	h.settle()

	st := h.tabOf("rigger")
	if !st.Locked || st.Err != lockedHint {
		t.Fatalf("409 did not surface as a status line: %+v", st)
	}
	h.wantView(lockedHint)
	// Still Type mode: a lock is a problem to report, not a state to live in.
	if h.m.mode != modeType {
		t.Errorf("a 409 knocked the operator out of Type mode (mode = %v)", h.m.mode)
	}

	// esc back to Navigate and press the advertised retry key.
	h.key("esc")
	h.settle()
	f.unlock("rigger")
	h.key("r")
	h.settle()
	if st := h.tabOf("rigger"); st.Locked {
		t.Errorf("retry did not clear the lock: %+v", st)
	}
	h.typeAt()
	h.send(typeString("y")...)
	h.settle()
	if got := f.inputText("rigger"); !strings.Contains(got, "y") {
		t.Errorf("input still blocked after the retry, got %q", got)
	}
}

func TestDetachStopsTheStreamAndReleases(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.key("x")
	h.settle()
	if st := h.tabOf("rigger"); st.Attached {
		t.Error("tab still attached after x")
	}
	_, _, releases := f.counts()
	if releases != 1 {
		t.Errorf("releases = %d, want 1 after detach", releases)
	}
}

func TestQuitReleasesEveryHeldKeyboard(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()
	h.key("n") // attach rigger too
	h.settle()

	h.key("q")
	h.settle()
	if !h.quitted() {
		t.Error("q did not quit")
	}
	_, _, releases := f.counts()
	if releases != 2 {
		t.Errorf("releases on quit = %d, want 2 (one per attached mate)", releases)
	}
}

// ---------------------------------------------------------------------------
// The input model: two modes, no prefix.
// ---------------------------------------------------------------------------

// The whole point of the redesign: bare digits, no chord, from the mode the bridge
// launches in.
func TestBareNumberKeysSelectMatesInNavigateMode(t *testing.T) {
	f := newFakeShip(t)
	for _, p := range []string{"backend", "frontend", "security"} {
		f.spawnPTY(p, "")
	}
	f.setRoster(
		MateStatus{Persona: "backend", Status: "idle"},
		MateStatus{Persona: "frontend", Status: "idle"},
		MateStatus{Persona: "security", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()
	if h.m.mode != modeNavigate {
		t.Fatalf("the bridge did not launch in Navigate mode (mode = %v)", h.m.mode)
	}

	for _, tc := range []struct{ key, want string }{
		{"2", "frontend"},
		{"1", "backend"},
		{"3", "security"},
		{"2", "frontend"},
	} {
		h.key(tc.key)
		h.settle()
		if got := h.m.currentPersona(); got != tc.want {
			t.Errorf("bare %q focused %q, want %q", tc.key, got, tc.want)
		}
	}
	// Selecting is not typing: not one byte reached any mate.
	for _, p := range []string{"backend", "frontend", "security"} {
		if got := f.inputText(p); got != "" {
			t.Errorf("navigating leaked %q into %s", got, p)
		}
	}
	// Out of range is a no-op, not a crash.
	h.key("9")
	h.settle()
	if got := h.m.currentPersona(); got != "frontend" {
		t.Errorf("out-of-range 9 moved focus to %q", got)
	}
}

// Every key means every key. ^B in particular: Claude Code binds it, so the bridge
// must not own it — that was the owner's first complaint about the old model.
func TestTypeModePassesEveryKeyThroughIncludingControlB(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()

	// Keys that USED to be bridge commands, plus the old prefix itself.
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"ctrl+b", "\x02"},
		{"ctrl+c", "\x03"},
		{"ctrl+a", "\x01"},
		{"ctrl+d", "\x04"},
		{"n", "n"},
		{"p", "p"},
		{"a", "a"},
		{"q", "q"},
		{"x", "x"},
		{"1", "1"},
		{"?", "?"},
		{"tab", "\t"},
		{"enter", "\r"},
		{"up", "\x1b[A"},
		{"left", "\x1b[D"},
	} {
		before := f.inputText("rigger")
		h.key(tc.key)
		h.settle()
		got := strings.TrimPrefix(f.inputText("rigger"), before)
		if got != tc.want {
			t.Errorf("Type mode sent %q for %q, want %q", got, tc.key, tc.want)
		}
		if h.quitted() {
			t.Fatalf("%q quit the bridge from Type mode", tc.key)
		}
		if h.m.mode != modeType {
			t.Fatalf("%q knocked the bridge out of Type mode", tc.key)
		}
	}
}

func TestEscReturnsToNavigateAndAltEscSendsALiteralEsc(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()

	// alt+esc is the literal-escape hatch: Claude Code uses esc, and the bridge
	// eats the bare one.
	h.key("alt+esc")
	h.settle()
	if got := f.inputText("rigger"); got != "\x1b" {
		t.Errorf("alt+esc sent %q, want a single ESC", got)
	}
	if h.m.mode != modeType {
		t.Fatal("alt+esc left Type mode")
	}

	h.key("esc")
	h.settle()
	if h.m.mode != modeNavigate {
		t.Fatalf("esc did not return to Navigate (mode = %v)", h.m.mode)
	}
	// And bare esc sent nothing.
	if got := f.inputText("rigger"); got != "\x1b" {
		t.Errorf("bare esc leaked to the mate: input is now %q", got)
	}
	// Navigate is live again: a bare digit is a command, not a keystroke.
	h.key("1")
	h.settle()
	if got := f.inputText("rigger"); got != "\x1b" {
		t.Errorf("a bare digit reached the mate after esc: %q", got)
	}
}

// alt+digit is the "flop between mates without stopping typing" path. Claude Code
// has no alt+digit binding, which is why that namespace is safe to reserve.
func TestAltDigitSwitchesMatesWhileTyping(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()
	h.typeAt()
	if h.m.currentPersona() != "cooper" {
		t.Fatalf("focus = %q, want cooper", h.m.currentPersona())
	}
	h.send(typeString("aa")...)
	h.settle()

	h.key("alt+2")
	h.settle()
	if h.m.currentPersona() != "rigger" {
		t.Fatalf("alt+2 focused %q, want rigger", h.m.currentPersona())
	}
	if h.m.mode != modeType {
		t.Fatal("alt+2 dropped out of Type mode")
	}
	// The new mate's keyboard was claimed on the way in, with no operator action.
	if st := h.tabOf("rigger"); !st.Writer {
		t.Errorf("alt+2 did not claim rigger's keyboard: %+v", st)
	}
	h.send(typeString("bb")...)
	h.settle()

	h.key("alt+1")
	h.settle()
	h.send(typeString("cc")...)
	h.settle()

	if got := f.inputText("cooper"); got != "aacc" {
		t.Errorf("cooper received %q, want %q", got, "aacc")
	}
	if got := f.inputText("rigger"); got != "bb" {
		t.Errorf("rigger received %q, want %q", got, "bb")
	}
	// alt+digit is bridge addressing; it must not also land in the mate.
	if strings.ContainsAny(f.inputText("cooper")+f.inputText("rigger"), "\x1b12") {
		t.Error("alt+digit leaked into a mate")
	}
}

// Navigate mode must not be a place where keystrokes vanish into a live-looking
// pane. It renders the mate, but the mode word and the border say who has the keys.
func TestNavigateModeSendsNothingAndSaysSo(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "hello\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	// A whole sentence of ordinary letters, none of which is a binding.
	h.send(typeString("zzz yyy zzz")...)
	h.settle()
	if got := f.inputText("rigger"); got != "" {
		t.Errorf("Navigate mode forwarded %q", got)
	}
	h.wantView("NAVIGATE")
	h.wantNotView("TYPE")

	h.typeAt()
	h.wantView("TYPE")
	h.wantNotView("NAVIGATE")
}

// The mode must be legible with colour stripped, which is why the border changes
// WEIGHT and not just hue.
func TestModeIsVisibleWithoutColour(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	nav := h.view()
	if !strings.Contains(nav, frameNavigate.tl) || !strings.Contains(nav, frameNavigate.v) {
		t.Errorf("Navigate pane is not drawn in the light border:\n%s", nav)
	}
	if strings.Contains(nav, frameType.v) {
		t.Error("Navigate pane used the heavy border")
	}

	h.typeAt()
	typ := h.view()
	if !strings.Contains(typ, frameType.tl) || !strings.Contains(typ, frameType.v) {
		t.Errorf("Type pane is not drawn in the heavy border:\n%s", typ)
	}
	if strings.Contains(typ, frameNavigate.v) {
		t.Error("Type pane used the light border")
	}
}

// The prefix is gone, everywhere: there is no key that arms a chord, and no key
// whose meaning depends on what was pressed before it.
func TestThereIsNoPrefixLayer(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	// In Navigate, ^b is not a command and not forwarded — it is simply unbound.
	h.key("ctrl+b")
	h.settle()
	if got := f.inputText("rigger"); got != "" {
		t.Errorf("^b in Navigate reached the mate: %q", got)
	}
	// And the very next key means what it always means.
	h.key("?")
	h.settle()
	if h.m.overlay != overlayHelp {
		t.Errorf("bare ? did not open help, overlay = %v", h.m.overlay)
	}
	h.wantView("NAVIGATE", "TYPE", "no prefix key")
	h.key("z") // any key closes help, and must not reach the mate
	h.settle()
	if h.m.overlay != overlayNone {
		t.Error("help overlay did not close")
	}
	if got := f.inputText("rigger"); got != "" {
		t.Errorf("help overlay leaked %q to the mate", got)
	}
}

// End to end for the "^@" bug: keys that encode to nothing must reach the server as
// nothing — not as a zero byte, and not as an empty POST either.
func TestKeysThatEncodeToNothingSendNoBytes(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()

	// A bare modifier keypress as Windows reports it, a zero-value KeyMsg, and a
	// KeyType with no terminal form. All three used to become a NUL or were about to.
	h.send(
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}, Alt: true},
		tea.KeyMsg{},
		tea.KeyMsg{Type: tea.KeyCtrlAt},
		tea.KeyMsg{Type: tea.KeyType(-9999)},
	)
	h.settle()

	if got := f.inputText("rigger"); got != "" {
		t.Errorf("a key with no encoding sent %q to the mate", got)
	}
	inputs, _, _ := f.snapshot()
	if len(inputs) != 0 {
		t.Errorf("keys with no encoding produced %d input requests: %+v", len(inputs), inputs)
	}
	// Real keys still work right afterwards, so the guard drops keys rather than
	// wedging the pipe.
	h.send(typeString("ok")...)
	h.settle()
	if got := f.inputText("rigger"); got != "ok" {
		t.Errorf("mate received %q after the dropped keys, want %q", got, "ok")
	}
}

func TestControlCQuitsFromNavigateAndInterruptsTheMateWhileTyping(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()
	h.key("ctrl+c")
	h.settle()
	if h.quitted() {
		t.Error("ctrl+c while typing quit the bridge instead of interrupting the mate")
	}
	if got := f.inputText("rigger"); got != "\x03" {
		t.Errorf("mate received %q, want \\x03", got)
	}

	h.key("esc")
	h.settle()
	h.key("ctrl+c")
	h.settle()
	if !h.quitted() {
		t.Error("ctrl+c in Navigate did not quit")
	}
}

func TestControlCQuitsWhenNothingIsAttached(t *testing.T) {
	f := newFakeShip(t)
	h := setup(t, f)
	h.poll()
	h.key("ctrl+c")
	h.settle()
	if !h.quitted() {
		t.Error("ctrl+c with no attached mate should quit")
	}
}

// The one prompt that survived the redesign, and it is not an attach step: opening
// a terminal for a mid-turn HEADLESS mate kills that process and loses the
// in-flight answer. That is destructive, so it needs a human. Everything else
// attaches on its own.
func TestMidTurnHeadlessMateAsksBeforeLosingTheInFlightAnswer(t *testing.T) {
	f := newFakeShip(t)
	// No PTY: /pty/.../snapshot 404s. Mate is mid-turn.
	f.setRoster(MateStatus{Persona: "rigger", Status: "working"})
	h := setup(t, f)
	h.poll()

	if h.m.overlay != overlayAttachAsk {
		t.Fatalf("no confirmation for a mid-turn headless mate, overlay = %v", h.m.overlay)
	}
	h.wantView("rigger is mid-turn with no terminal", "losing the in-flight answer", "wait for the turn")
	// It is NOT phrased as an attach step the operator has to resolve.
	h.wantNotView("not attached", "to attach", "take over")
	starts, _, _ := f.counts()
	if starts != 0 {
		t.Errorf("PTY started before the operator consented (%d starts)", starts)
	}

	// esc leaves it alone without starting anything.
	h.key("esc")
	h.settle()
	starts, _, _ = f.counts()
	if starts != 0 {
		t.Errorf("esc still started a PTY (%d starts)", starts)
	}

	// Focusing the mate asks again, and `o` consents.
	h.key("enter")
	h.settle()
	if h.m.overlay != overlayAttachAsk {
		t.Fatalf("focusing the mate did not re-ask, overlay = %v", h.m.overlay)
	}
	h.key("o")
	h.settle()
	starts, _, _ = f.counts()
	if starts != 1 {
		t.Errorf("starts = %d, want 1 after explicit consent", starts)
	}
	if st := h.tabOf("rigger"); !st.Attached {
		t.Errorf("not attached after consent: %+v", st)
	}
}

func TestAttachWaitForIdleDefersUntilTheTurnEnds(t *testing.T) {
	f := newFakeShip(t)
	f.setRoster(MateStatus{Persona: "rigger", Status: "working"})
	h := setup(t, f)
	h.poll()
	if h.m.overlay != overlayAttachAsk {
		t.Fatalf("expected the attach prompt, got %v", h.m.overlay)
	}
	h.key("w")
	h.settle()
	if starts, _, _ := f.counts(); starts != 0 {
		t.Fatalf("waiting still started a PTY (%d)", starts)
	}
	h.wantView("will attach to rigger once its turn finishes")

	// Still busy: still waiting.
	h.poll()
	if starts, _, _ := f.counts(); starts != 0 {
		t.Fatalf("attached while still working (%d starts)", starts)
	}
	// Turn ends.
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h.poll()
	if starts, _, _ := f.counts(); starts != 1 {
		t.Errorf("starts = %d, want 1 once idle", starts)
	}
	if st := h.tabOf("rigger"); !st.Attached {
		t.Errorf("did not attach when the mate went idle: %+v", st)
	}
}

func TestExistingPTYAttachesWithoutAsking(t *testing.T) {
	f := newFakeShip(t)
	// Working AND already under a PTY: attaching steals nothing, so no prompt.
	f.spawnPTY("rigger", "already hosted\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "working"})
	h := setup(t, f)
	h.poll()
	if h.m.overlay != overlayNone {
		t.Errorf("prompted even though a PTY already existed, overlay = %v", h.m.overlay)
	}
	if starts, _, _ := f.counts(); starts != 0 {
		t.Errorf("issued %d starts for an already-hosted mate", starts)
	}
	if got := h.paneText(); !strings.Contains(got, "already hosted") {
		t.Errorf("pane not primed:\n%s", got)
	}
}

func TestMateExitMarksTheTab(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "working\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	f.exitPTY("rigger")
	h.settle()

	st := h.tabOf("rigger")
	if !st.Exited || st.Attached {
		t.Errorf("tab state after exit = %+v, want exited and detached", st)
	}
	if got := h.paneText(); !strings.Contains(got, "[mate exited]") {
		t.Errorf("pane does not report the exit:\n%s", got)
	}
	h.wantView(glyphDone)
}

func TestStatusErrorSurfacesWithoutCrashing(t *testing.T) {
	f := newFakeShip(t)
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	f.mu.Lock()
	f.statusErr = 500
	f.mu.Unlock()
	h.poll()
	h.wantView("status:")
	// The roster we already had is kept rather than blanked.
	if len(h.m.tabStates()) != 1 {
		t.Errorf("tabs dropped on a status error: %+v", h.m.tabStates())
	}
}

func TestNoLiveMatesShowsGuidance(t *testing.T) {
	f := newFakeShip(t)
	h := setup(t, f)
	h.poll()
	h.wantView("no live mates", "No live mates on this machine.")
}

// Hostile text from an agent must be neutralised on its way into every string
// the bridge renders itself. A persona name or Bash command carrying an escape
// sequence would otherwise repaint the operator's screen from inside a tab label
// or an approval row.
func TestChromeIsSanitizedEndToEnd(t *testing.T) {
	f := newFakeShip(t)
	evil := "ok\x1b[2J\x1b]0;pwn\x07‮gnp\r\n"
	f.spawnPTY(evil, "") // so the attach prompt doesn't capture the keys below
	f.setRoster(MateStatus{Persona: evil, Status: "blocked"})
	f.setPending(Pending{
		ID:      "e1\x1b[31m",
		Persona: evil,
		Tool:    "Bash\x1b[999;999H",
		Input:   "rm -rf /\x1b[2J‮/ etc\n\nnext line",
	})
	h := setup(t, f)
	h.poll()
	h.key("a")
	h.settle()

	raw := h.m.View()
	// Only SGR is permitted in the frame we hand the terminal. The pane is empty
	// here (nothing attached), so every escape present came from bridge chrome.
	for i := 0; i < len(raw); i++ {
		if raw[i] != 0x1b {
			continue
		}
		if i+1 >= len(raw) || raw[i+1] != '[' {
			t.Fatalf("non-CSI escape at %d in rendered frame", i)
		}
		j := i + 2
		for j < len(raw) && (raw[j] == ';' || (raw[j] >= '0' && raw[j] <= '9')) {
			j++
		}
		if j >= len(raw) || raw[j] != 'm' {
			t.Fatalf("non-SGR escape %q leaked into the frame", raw[i:min(j+1, len(raw))])
		}
		i = j
	}
	// The bidi override and the newlines are gone from the approval row, and so is
	// the BODY of every escape sequence — not just its ESC introducer.
	stripped := h.view()
	if strings.Contains(stripped, "‮") {
		t.Error("bidi override survived into the view")
	}
	if !strings.Contains(stripped, "rm -rf // etc next line") {
		t.Errorf("approval summary not collapsed to one line:\n%s", stripped)
	}
	for _, residue := range []string{"[2J", "[999;999H", "0;pwn", "[31m"} {
		if strings.Contains(stripped, residue) {
			t.Errorf("escape-sequence payload %q survived as visible text:\n%s", residue, stripped)
		}
	}
}

func TestTabBarOverflowKeepsBlockedAndActiveVisible(t *testing.T) {
	f := newFakeShip(t)
	var roster []MateStatus
	for _, name := range []string{
		"alpha-mate", "bravo-mate", "charlie-mate", "delta-mate", "echo-mate",
		"foxtrot-mate", "golf-mate", "hotel-mate", "india-mate", "juliet-mate",
	} {
		roster = append(roster, MateStatus{Persona: name, Status: "idle"})
	}
	roster = append(roster, MateStatus{Persona: "zulu-mate", Status: "blocked"})
	f.setRoster(roster...)
	f.setPending(Pending{ID: "p1", Persona: "zulu-mate", Tool: "Bash"})
	h := newHarness(t, New(Options{Base: f.base(), StatusInterval: time.Hour, PendingInterval: time.Hour}))
	h.send(tea.WindowSizeMsg{Width: 60, Height: 24}) // deliberately narrow
	h.settle()
	h.poll()

	v := h.view()
	if !strings.Contains(v, "zulu-mate") {
		t.Errorf("overflowing tab bar dropped the blocked mate:\n%s", v)
	}
	if !strings.Contains(v, "more") {
		t.Errorf("overflow summary missing:\n%s", v)
	}
	// And the bar still fits the window.
	for i, line := range strings.Split(v, "\n") {
		if len([]rune(line)) > 60 {
			t.Errorf("line %d is %d cells wide, window is 60: %q", i, len([]rune(line)), line)
		}
	}
}

func TestViewFitsTheWindow(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", strings.Repeat("x", 500)+"\r\n")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	f.setPending(Pending{ID: "q1", Persona: "rigger", Tool: "Bash", Input: strings.Repeat("y", 500)})
	h := setup(t, f)
	h.poll()

	for _, mode := range []inputMode{modeNavigate, modeType} {
		h.m.mode = mode
		for _, size := range []tea.WindowSizeMsg{
			{Width: 100, Height: 30}, {Width: 40, Height: 10}, {Width: 200, Height: 60},
			{Width: 24, Height: 8}, // absurdly small: must still not overflow or panic
		} {
			h.send(size)
			h.settle()
			lines := strings.Split(h.view(), "\n")
			if len(lines) > size.Height {
				t.Errorf("%v %dx%d: rendered %d lines", mode, size.Width, size.Height, len(lines))
			}
			for i, l := range lines {
				if w := len([]rune(l)); w > size.Width {
					t.Errorf("%v %dx%d: line %d is %d cells wide", mode, size.Width, size.Height, i, w)
				}
			}
			// The pane box has to be EXACTLY the window width on every row, or the
			// right-hand border staircases down the screen. An upper bound alone
			// would not catch that.
			g, _ := h.m.frame()
			for i, l := range lines {
				r := []rune(l)
				if len(r) == 0 || (r[0] != []rune(g.tl)[0] && r[0] != []rune(g.v)[0] && r[0] != []rune(g.bl)[0]) {
					continue
				}
				if len(r) != size.Width {
					t.Errorf("%v %dx%d: pane row %d is %d cells wide, want %d: %q",
						mode, size.Width, size.Height, i, len(r), size.Width, l)
				}
			}
		}
	}
	h.m.mode = modeNavigate
}

// Bare arrows, h/l and tab all move between mates in Navigate mode — every one of
// them unshifted and unprefixed.
func TestBareArrowsAndVimKeysMoveBetweenMates(t *testing.T) {
	f := newFakeShip(t)
	for _, p := range []string{"aa", "bb", "cc"} {
		f.spawnPTY(p, "")
	}
	f.setRoster(
		MateStatus{Persona: "aa", Status: "idle"},
		MateStatus{Persona: "bb", Status: "idle"},
		MateStatus{Persona: "cc", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()

	for _, tc := range []struct{ key, want string }{
		{"right", "bb"},
		{"l", "cc"},
		{"tab", "aa"},  // wraps
		{"left", "cc"}, // wraps back
		{"h", "bb"},
		{"shift+tab", "aa"},
	} {
		h.key(tc.key)
		h.settle()
		if got := h.m.currentPersona(); got != tc.want {
			t.Errorf("bare %q focused %q, want %q", tc.key, got, tc.want)
		}
	}
	for _, p := range []string{"aa", "bb", "cc"} {
		if got := f.inputText(p); got != "" {
			t.Errorf("moving between mates leaked %q into %s", got, p)
		}
	}
}

func TestFocusOptionSelectsAPersona(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("aa", "")
	f.spawnPTY("zz", "")
	f.setRoster(
		MateStatus{Persona: "aa", Status: "idle"},
		MateStatus{Persona: "zz", Status: "idle"},
	)
	h := setup(t, f, func(o *Options) { o.Focus = "zz" })
	h.poll()
	if h.m.currentPersona() != "zz" {
		t.Errorf("focus option ignored, focused %q", h.m.currentPersona())
	}
}

func TestDeviceReportRepliesOnlyForTheFocusedPane(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("cooper", "")
	f.spawnPTY("rigger", "")
	f.setRoster(
		MateStatus{Persona: "cooper", Status: "idle"},
		MateStatus{Persona: "rigger", Status: "idle"},
	)
	h := setup(t, f)
	h.poll()
	h.key("n") // attach rigger, focus it
	h.settle()
	if h.m.currentPersona() != "rigger" {
		t.Fatalf("focus = %q", h.m.currentPersona())
	}

	// A cursor-position query on the BACKGROUND mate must not be answered:
	// answering is a write, and a write claims the writer lock, so a background
	// pane would silently steal the operator's keyboard.
	f.push("cooper", "\x1b[6n")
	h.settle()
	if got := f.inputText("cooper"); got != "" {
		t.Errorf("background pane answered a device report: %q", got)
	}

	// The focused mate does get its answer, so a TUI probing at startup isn't
	// left hanging.
	f.push("rigger", "\x1b[6n")
	h.settle()
	if got := f.inputText("rigger"); !strings.HasPrefix(got, "\x1b[") || !strings.HasSuffix(got, "R") {
		t.Errorf("focused pane did not answer the cursor-position report: %q", got)
	}
}

func TestPasteUsesBracketedPasteWhenTheMateEnabledIt(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()
	// The fake ship's stream snapshot latches ?2004h.
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted text"), Paste: true})
	h.settle()
	if got := f.inputText("rigger"); got != "\x1b[200~pasted text\x1b[201~" {
		t.Errorf("paste sent %q, want it bracketed", got)
	}
}

func TestLargePasteIsChunkedNotTruncated(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()
	big := strings.Repeat("z", maxInputChunk*2+37)
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(big), Paste: true})
	h.settle()
	got := f.inputText("rigger")
	if !strings.Contains(got, big) {
		t.Errorf("large paste was truncated: got %d bytes, want at least %d", len(got), len(big))
	}
	inputs, _, _ := f.snapshot()
	if len(inputs) < 3 {
		t.Errorf("large paste sent in %d requests, expected it to be chunked", len(inputs))
	}
}

func TestArrowKeysFollowApplicationCursorMode(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	h.typeAt()
	h.key("up")
	h.settle()
	if got := f.inputText("rigger"); got != "\x1b[A" {
		t.Fatalf("normal-mode up = %q, want ESC[A", got)
	}
	// The mate switches to application cursor keys; the encoding must follow.
	f.push("rigger", "\x1b[?1h")
	h.settle()
	h.key("up")
	h.settle()
	if got := f.inputText("rigger"); !strings.HasSuffix(got, "\x1bOA") {
		t.Errorf("app-cursor up not sent as SS3: %q", got)
	}
}

// The approval alert bar costs a screen row, so a mate blocking shrinks the pane
// just as a window resize does. The emulator and the server both have to be told,
// or the mate keeps drawing to a row the bridge no longer shows.
func TestAlertBarAppearingResizesThePTY(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()

	_, resizes, _ := f.snapshot()
	before := resizes[len(resizes)-1]
	if h.m.alertText() != "" {
		t.Fatal("alert bar showing with no approvals")
	}

	// A mate blocks: the alert bar appears and takes a row.
	f.setPending(Pending{ID: "a1", Persona: "rigger", Tool: "Bash", Input: "x"})
	h.poll()
	if h.m.alertText() == "" {
		t.Fatal("alert bar did not appear for a pending approval")
	}

	wantCols, wantRows := h.m.paneSize()
	if wantRows >= before.Rows {
		t.Fatalf("pane did not shrink: was %d rows, now %d", before.Rows, wantRows)
	}
	_, resizes, _ = f.snapshot()
	last := resizes[len(resizes)-1]
	if last.Cols != wantCols || last.Rows != wantRows {
		t.Errorf("server was told %dx%d, pane is %dx%d", last.Cols, last.Rows, wantCols, wantRows)
	}
	cols, rows := h.m.tab("rigger").screen.Size()
	if cols != wantCols || rows != wantRows {
		t.Errorf("emulator is %dx%d, pane is %dx%d", cols, rows, wantCols, wantRows)
	}

	// And it grows back when the approval is resolved.
	f.setPending()
	h.poll()
	grownCols, grownRows := h.m.paneSize()
	if grownRows != before.Rows {
		t.Errorf("pane did not grow back: %d rows, want %d", grownRows, before.Rows)
	}
	cols, rows = h.m.tab("rigger").screen.Size()
	if cols != grownCols || rows != grownRows {
		t.Errorf("emulator did not grow back: %dx%d, want %dx%d", cols, rows, grownCols, grownRows)
	}
}

func TestOverlayDoesNotChangePaneGeometry(t *testing.T) {
	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "blocked"})
	f.setPending(Pending{ID: "g1", Persona: "rigger", Tool: "Bash"})
	h := setup(t, f)
	h.poll()
	_, before, _ := f.snapshot()
	h.key("a")
	h.settle()
	h.key("esc")
	h.settle()
	_, after, _ := f.snapshot()
	if len(after) != len(before) {
		t.Errorf("opening an overlay caused %d extra PTY resizes", len(after)-len(before))
	}
}
