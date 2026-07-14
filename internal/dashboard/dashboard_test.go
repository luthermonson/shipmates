package dashboard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/livesession"
)

func attach(events ...livesession.Event) livesession.Attach {
	return livesession.Attach{Snapshot: livesession.Snapshot{Persona: "backend", SessionID: "session", ThreadID: "thread", State: livesession.Working}, ControllerID: "SECRET-CONTROLLER", OldestAvailable: 0, NextSequence: uint64(len(events)), Events: events}
}
func event(seq uint64, kind string, data map[string]any) livesession.Event {
	return livesession.Event{SchemaVersion: 1, Sequence: seq, Persona: "backend", SessionID: "session", ThreadID: "raw-thread", TurnID: "raw-turn", Kind: kind, Data: data}
}

func TestDeterministicRedactedSnapshot(t *testing.T) {
	secret := "RAW\x1b[31m\u202eSECRET"
	m, err := NewModel(attach(
		event(0, "agent.message", map[string]any{"text": "hello\x1bworld", "truncated": true}),
		event(1, "unknown.raw", map[string]any{"frame": secret}),
		event(2, "request.refused", map[string]any{"approval": secret}),
	))
	if err != nil {
		t.Fatal(err)
	}
	s := Render(m, Size{Width: 80, Height: 6})
	got := strings.Join(s.Lines, "\n")
	want := "shipmates open backend | working | controller\nagent: hello�world [truncated]\nrequest refused\n/help  /interrupt  /detach  /quit  // literal slash"
	if got != want {
		t.Fatalf("snapshot:\n%s\nwant:\n%s", got, want)
	}
	for _, forbidden := range []string{"SECRET-CONTROLLER", "raw-thread", "raw-turn", "RAW", "\x1b", "\u202e"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("leaked %q in %q", forbidden, got)
		}
	}
}

func TestDroppedHistoryResizeAndBoundedLayout(t *testing.T) {
	m, err := NewModel(livesession.Attach{Snapshot: livesession.Snapshot{Persona: "backend", SessionID: "session", State: livesession.Idle}, ControllerID: "c", OldestAvailable: 7, NextSequence: 8, HistoryDropped: true, Events: []livesession.Event{event(7, "turn.completed", nil)}})
	if err != nil {
		t.Fatal(err)
	}
	wide := Render(m, Size{Width: 80, Height: 5})
	if wide.Lines[1] != "history dropped before sequence 7" {
		t.Fatal(wide.Lines)
	}
	narrow := Render(m, Size{Width: 12, Height: 3})
	want := []string{"shipmates o…", "completed", "/help  /int…"}
	if !reflect.DeepEqual(narrow.Lines, want) {
		t.Fatalf("narrow=%q", narrow.Lines)
	}
	for _, line := range narrow.Lines {
		if len([]rune(line)) > 12 {
			t.Fatal(line)
		}
	}
}

func TestAgentDeltasCoalesceAndWrapWithoutEllipsis(t *testing.T) {
	m, err := NewModel(attach(
		event(0, "agent.message", map[string]any{"text": "A thoughtfully", "partial": true}),
		event(1, "activity", map[string]any{"category": "command"}),
		event(2, "agent.message", map[string]any{"text": " composed response", "partial": true}),
		event(3, "agent.message", map[string]any{"text": "A thoughtfully composed response"}),
		event(4, "turn.completed", nil),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Events) != 3 || m.Events[0].Text != "agent: A thoughtfully composed response" || m.Events[0].Partial {
		t.Fatalf("events=%+v", m.Events)
	}
	got := strings.Join(Render(m, Size{Width: 18, Height: 6}).Lines, "\n")
	if !strings.Contains(got, "thoughtfully") || !strings.Contains(got, "composed response") || strings.Contains(got, "thoughtfull…") || strings.Contains(got, "composed respo…") {
		t.Fatalf("wrapped output=%q", got)
	}
}

func TestDashboardColorsDistinguishPersonaStatusAndSidebar(t *testing.T) {
	line := colorizeDashboardLine("agent: hello │ VOYAGE PLAN", "skipper", true)
	if !strings.Contains(line, "\x1b[1;96magent: hello") || !strings.Contains(line, "\x1b[97mVOYAGE PLAN") {
		t.Fatalf("colored line=%q", line)
	}
	if colorizeDashboardLine("agent: hello", "skipper", false) == colorizeDashboardLine("agent: hello", "architect", false) {
		t.Fatal("persona colors should be stable and distinct for skipper and architect")
	}
}

func TestStreamRejectsGapRegressionAndIdentityMismatch(t *testing.T) {
	for name, f := range map[string]livesession.Feed{
		"gap":      {OldestAvailable: 0, NextSequence: 2, Events: []livesession.Event{event(1, "activity", nil)}},
		"identity": {OldestAvailable: 0, NextSequence: 1, Events: []livesession.Event{{SchemaVersion: 1, Sequence: 0, Persona: "other", SessionID: "session", Kind: "activity"}}},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := NewModel(attach())
			if !errors.Is(m.ApplyFeed(f), ErrInvalidStream) {
				t.Fatal("accepted")
			}
		})
	}
	m, _ := NewModel(attach(event(0, "activity", map[string]any{"category": "tool"})))
	if err := m.ApplyFeed(livesession.Feed{OldestAvailable: 0, NextSequence: 1, Events: []livesession.Event{event(0, "activity", nil)}}); err != nil {
		t.Fatalf("exact duplicate: %v", err)
	}
}

func TestReconnectReplacesLeaseWithoutRenderingCredential(t *testing.T) {
	m, _ := NewModel(attach(event(0, "turn.started", nil)))
	a := livesession.Attach{Snapshot: livesession.Snapshot{Persona: "backend", SessionID: "new-session", State: livesession.Idle}, ControllerID: "NEW-SECRET", OldestAvailable: 4, NextSequence: 4, HistoryDropped: true}
	if err := m.Reconnect(a); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(Render(m, Size{80, 4}).Lines, "\n")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "turn started") {
		t.Fatal(got)
	}
}

type fakeEditor struct {
	inputs []Input
	err    error
}

func (f *fakeEditor) Next(context.Context) (Input, error) {
	if len(f.inputs) == 0 {
		return Input{}, f.err
	}
	v := f.inputs[0]
	f.inputs = f.inputs[1:]
	return v, nil
}

func TestInputLoopResizeCancellationAndNoActionWiring(t *testing.T) {
	m, _ := NewModel(attach())
	ed := &fakeEditor{inputs: []Input{{Kind: InputResize, Size: Size{10, 2}}, {Kind: InputLine, Line: "/help"}, {Kind: InputCancel}}}
	var screens []Screen
	var parsed []ParsedKind
	err := InputLoop(context.Background(), ed, Size{20, 3}, func(s Screen) error { screens = append(screens, s); return nil }, m, func(p ParsedInput) error { parsed = append(parsed, p.Kind); return nil })
	if err != nil || len(screens) != 2 || !reflect.DeepEqual(parsed, []ParsedKind{ParsedHelp}) {
		t.Fatalf("err=%v screens=%d parsed=%v", err, len(screens), parsed)
	}
}

func TestInputGrammarAndLimits(t *testing.T) {
	tests := map[string]ParsedInput{"": {Kind: ParsedNoop}, "//help": {Kind: ParsedMessage, Text: "/help"}, "///x": {Kind: ParsedMessage, Text: "//x"}, "/help": {Kind: ParsedHelp}, "/interrupt": {Kind: ParsedInterrupt}, "/detach": {Kind: ParsedDetach}, "/quit": {Kind: ParsedDetach}, "/sail": {Kind: ParsedSail}, "/sail --retry-failed": {Kind: ParsedSail, Text: "retry-failed"}, "/sail --verbose": {Kind: ParsedSail, Text: "verbose"}, "/sail --verbose --retry-failed": {Kind: ParsedSail, Text: "retry-failed verbose"}, "/consult data boundaries": {Kind: ParsedConsult, Text: "data boundaries"}, "/help x": {Kind: ParsedUnknown}, " hi ": {Kind: ParsedMessage, Text: " hi "}}
	for in, want := range tests {
		got, err := ParseLine(in)
		if err != nil || got != want {
			t.Fatalf("%q got=%+v err=%v", in, got, err)
		}
	}
	if _, err := ParseLine("bad\x00"); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	if _, err := ParseLine(strings.Repeat("x", MaxInputBytes+1)); !errors.Is(err, ErrInputTooLarge) {
		t.Fatal(err)
	}
}

func TestWidePlanningSidebarAndNarrowFallback(t *testing.T) {
	m, _ := NewModel(attach())
	m.Sidebar = &Sidebar{Title: "Export voyage", Status: "DRAFT", Sections: []SidebarSection{{Heading: "Blast area", Items: []string{"Account API", "Worker"}}}}
	wide := strings.Join(Render(m, Size{Width: 120, Height: 8}).Lines, "\n")
	if !strings.Contains(wide, "Export voyage") || !strings.Contains(wide, "BLAST AREA") || !strings.Contains(wide, "│") {
		t.Fatalf("wide sidebar = %s", wide)
	}
	narrow := strings.Join(Render(m, Size{Width: 80, Height: 5}).Lines, "\n")
	if strings.Contains(narrow, "Export voyage") || strings.Contains(narrow, "│") {
		t.Fatalf("narrow view did not fall back = %s", narrow)
	}
}

func TestPlanningSidebarScrollsWithoutTruncatingContent(t *testing.T) {
	m, _ := NewModel(attach())
	items := make([]string, 20)
	for i := range items {
		items[i] = fmt.Sprintf("criterion-%02d", i)
	}
	m.Sidebar = &Sidebar{Title: "Long voyage", Status: "DRAFT", Sections: []SidebarSection{{Heading: "Acceptance", Items: items}}}
	top := strings.Join(Render(m, Size{Width: 120, Height: 8}).Lines, "\n")
	if !strings.Contains(top, "criterion-00") || strings.Contains(top, "criterion-19") {
		t.Fatalf("top viewport=%s", top)
	}
	m.SidebarScroll = int(^uint(0) >> 1)
	bottom := strings.Join(Render(m, Size{Width: 120, Height: 8}).Lines, "\n")
	if strings.Contains(bottom, "criterion-00") || !strings.Contains(bottom, "criterion-19") {
		t.Fatalf("bottom viewport=%s", bottom)
	}
}

func TestGuardRestorationWrapsInputCancellation(t *testing.T) {
	f := &fakeTerminal{in: true, out: true}
	g, _ := NewGuard(f, false)
	m, _ := NewModel(attach())
	err := Run(context.Background(), g, func(ctx context.Context) error {
		return InputLoop(ctx, &fakeEditor{inputs: []Input{{Kind: InputCancel}}}, Size{20, 3}, func(Screen) error { return nil }, m, nil)
	})
	if err != nil || f.calls[len(f.calls)-1] != "restore" {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

type actionTransport struct {
	actions        []ActionRequest
	approvals      []ApprovalRequest
	result         livesession.ControlResult
	approvalResult livesession.ApprovalResult
	err            error
	released       int
}

func (f *actionTransport) Attach(context.Context, string, AttachRequest) (livesession.Attach, error) {
	return attach(), nil
}
func (f *actionTransport) Heartbeat(context.Context, string, string, string) error { return nil }
func (f *actionTransport) Sync(context.Context, string, string, string, uint64) (livesession.Attach, error) {
	return attach(), nil
}
func (f *actionTransport) Release(context.Context, string, string, string) error {
	f.released++
	return nil
}
func (f *actionTransport) Action(_ context.Context, _ string, a ActionRequest) (livesession.ControlResult, error) {
	f.actions = append(f.actions, a)
	return f.result, f.err
}
func (f *actionTransport) ResolveApproval(_ context.Context, _ string, a ApprovalRequest) (livesession.ApprovalResult, error) {
	f.approvals = append(f.approvals, a)
	if f.approvalResult.Outcome == "" {
		f.approvalResult.Outcome = "allowed_once"
	}
	return f.approvalResult, f.err
}

func approvalAttach() livesession.Attach {
	a := attach()
	a.TurnID = "turn"
	a.ControllerLeaseGeneration = 7
	a.PendingApproval = &livesession.ApprovalCard{
		Authority:   livesession.ApprovalAuthority{Persona: "backend", SessionID: "session", Backend: "codex-app-server", ThreadID: "thread", TurnID: "turn", ControllerID: a.ControllerID, ControllerLeaseGeneration: 7, BackendRequestID: "backend-secret", ApprovalRequestFingerprint: strings.Repeat("a", 64), PolicySnapshotID: strings.Repeat("b", 64), ApprovalID: "approval-public"},
		RequestKind: "process.exec", Summary: "printf [path]", PolicyEffect: "ask", ExpiresInMS: 30000,
	}
	return a
}

func TestApprovalCommandsUseCapturedCardAndAcknowledge(t *testing.T) {
	for _, tc := range []struct {
		line            string
		decision        livesession.ApprovalDecision
		outcome, notice string
	}{
		{"/allow-once", livesession.AllowOnce, "allowed_once", "allowed once"},
		{"/deny", livesession.DenyOnce, "denied", "denied"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			a := approvalAttach()
			m, _ := NewModel(a)
			f := &actionTransport{approvalResult: livesession.ApprovalResult{Outcome: tc.outcome}}
			c := &Connection{transport: f, Persona: "backend", Attach: a}
			var last Screen
			err := ActionLoopWithTicks(context.Background(), &fakeEditor{inputs: []Input{{Kind: InputLine, Line: tc.line}, {Kind: InputCancel}}}, Size{Width: 100, Height: 8}, func(s Screen) error { last = s; return nil }, m, c, make(chan time.Time))
			if err != nil || len(f.approvals) != 1 || f.approvals[0].Decision != tc.decision || f.approvals[0].Authority != a.PendingApproval.Authority {
				t.Fatalf("err=%v approvals=%+v", err, f.approvals)
			}
			if !strings.Contains(strings.Join(last.Lines, "\n"), tc.notice) || m.PendingApproval != nil {
				t.Fatalf("screen=%v pending=%+v", last, m.PendingApproval)
			}
		})
	}
}

func TestApprovalPendingRefusesMessagesAndDetachedOrStaleCannotDecide(t *testing.T) {
	a := approvalAttach()
	m, _ := NewModel(a)
	f := &actionTransport{}
	c := &Connection{transport: f, Persona: "backend", Attach: a}
	var last Screen
	err := ActionLoopWithTicks(context.Background(), &fakeEditor{inputs: []Input{{Kind: InputLine, Line: "yes"}, {Kind: InputLine, Line: "/detach"}}}, Size{100, 8}, func(s Screen) error { last = s; return nil }, m, c, make(chan time.Time))
	if err != nil || len(f.actions) != 0 || len(f.approvals) != 0 || !strings.Contains(strings.Join(last.Lines, "\n"), "approval_pending") {
		t.Fatalf("err=%v actions=%v approvals=%v screen=%v", err, f.actions, f.approvals, last)
	}

	m.PendingApproval = nil
	err = ActionLoopWithTicks(context.Background(), &fakeEditor{inputs: []Input{{Kind: InputLine, Line: "/allow-once"}, {Kind: InputCancel}}}, Size{100, 8}, func(s Screen) error { last = s; return nil }, m, c, make(chan time.Time))
	if err != nil || len(f.approvals) != 0 || !strings.Contains(strings.Join(last.Lines, "\n"), "stale/already resolved") {
		t.Fatalf("err=%v approvals=%v screen=%v", err, f.approvals, last)
	}
}

func TestApprovalCardRenderingIsBoundedAndRedacted(t *testing.T) {
	a := approvalAttach()
	a.PendingApproval.Summary = "run [path] SECRET-SUMMARY"
	m, _ := NewModel(a)
	text := strings.Join(Render(m, Size{Width: 48, Height: 6}).Lines, "\n")
	for _, secret := range []string{"backend-secret", strings.Repeat("a", 64), strings.Repeat("b", 64), a.ControllerID} {
		if strings.Contains(text, secret) {
			t.Fatalf("leaked %q in %q", secret, text)
		}
	}
	if !strings.Contains(text, "/allow-once") || !strings.Contains(text, "request: run [path]") {
		t.Fatal(text)
	}
}

func TestActionLoopExactActionsAcknowledgementsAndDetach(t *testing.T) {
	a := attach()
	a.State = livesession.Idle
	a.TurnID = ""
	m, _ := NewModel(a)
	f := &actionTransport{result: livesession.ControlResult{Snapshot: livesession.Snapshot{Persona: "backend", SessionID: "session", ThreadID: "thread", TurnID: "turn-1", State: livesession.Working}}}
	c := &Connection{transport: f, Persona: "backend", Attach: a}
	ed := &fakeEditor{inputs: []Input{{Kind: InputLine, Line: "hello"}, {Kind: InputLine, Line: "/help"}, {Kind: InputLine, Line: "/detach"}}}
	var last Screen
	if err := ActionLoopWithTicks(context.Background(), ed, Size{80, 8}, func(s Screen) error { last = s; return nil }, m, c, make(chan time.Time)); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].Action != livesession.ControllerMessage || f.actions[0].TurnID != "" || f.actions[0].Message != "hello" {
		t.Fatalf("actions=%+v", f.actions)
	}
	text := strings.Join(last.Lines, "\n")
	if !strings.Contains(text, "turn started") || !strings.Contains(text, "/help  /interrupt") {
		t.Fatal(text)
	}
	if f.released != 0 {
		t.Fatal("detach must be lease-only at caller cleanup")
	}
}

func TestActionLoopStaleRefusalAndCancelNeverInterrupt(t *testing.T) {
	a := attach()
	a.TurnID = "turn-old"
	m, _ := NewModel(a)
	f := &actionTransport{err: errors.New("stale_target")}
	c := &Connection{transport: f, Persona: "backend", Attach: a}
	ed := &fakeEditor{inputs: []Input{{Kind: InputLine, Line: "/interrupt"}, {Kind: InputCancel}}}
	var last Screen
	if err := ActionLoopWithTicks(context.Background(), ed, Size{80, 6}, func(s Screen) error { last = s; return nil }, m, c, make(chan time.Time)); err != nil {
		t.Fatal(err)
	}
	if len(f.actions) != 1 || f.actions[0].TurnID != "turn-old" {
		t.Fatalf("actions=%+v", f.actions)
	}
	if !strings.Contains(strings.Join(last.Lines, "\n"), "stale_target") {
		t.Fatal(last)
	}
}
