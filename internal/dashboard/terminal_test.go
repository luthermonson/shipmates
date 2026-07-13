package dashboard

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeTerminal struct {
	in, out bool
	calls   []string
	fail    string
}

func (f *fakeTerminal) StdinTTY() bool  { return f.in }
func (f *fakeTerminal) StdoutTTY() bool { return f.out }
func (f *fakeTerminal) Capture() (any, error) {
	f.calls = append(f.calls, "capture")
	return "original", nil
}
func (f *fakeTerminal) call(s string) error {
	f.calls = append(f.calls, s)
	if f.fail == s {
		return errors.New(s)
	}
	return nil
}
func (f *fakeTerminal) SetRaw(any) error { return f.call("raw") }
func (f *fakeTerminal) AlternateScreen(v bool) error {
	if v {
		return f.call("screen:on")
	}
	return f.call("screen:off")
}
func (f *fakeTerminal) CursorVisible(v bool) error {
	if v {
		return f.call("cursor:on")
	}
	return f.call("cursor:off")
}
func (f *fakeTerminal) Restore(v any) error {
	if v != "original" {
		panic("wrong snapshot")
	}
	return f.call("restore")
}

func TestTTYGatePrecedesCaptureAndMutation(t *testing.T) {
	for _, tc := range []fakeTerminal{{in: false, out: true}, {in: true, out: false}} {
		f := tc
		if _, err := NewGuard(&f, false); !errors.Is(err, ErrNotTTY) {
			t.Fatalf("err=%v", err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("calls before gate: %v", f.calls)
		}
	}
}
func TestGuardRestoresExactlyOnceAfterCtrlCOrFailure(t *testing.T) {
	f := &fakeTerminal{in: true, out: true}
	g, err := NewGuard(f, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Enter(); err != nil {
		t.Fatal(err)
	}
	_ = g.Restore()
	_ = g.Restore()
	want := []string{"capture", "raw", "screen:on", "cursor:off", "cursor:on", "screen:off", "restore"}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls=%v", f.calls)
	}
}
func TestCtrlCCancellationRunsRestoration(t *testing.T) {
	f := &fakeTerminal{in: true, out: true}
	g, _ := NewGuard(f, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, g, func(ctx context.Context) error { return ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if f.calls[len(f.calls)-1] != "restore" {
		t.Fatalf("calls=%v", f.calls)
	}
}
func TestPartialEnterStillRestoresAndPlainAvoidsScreenCursor(t *testing.T) {
	f := &fakeTerminal{in: true, out: true, fail: "cursor:off"}
	g, _ := NewGuard(f, false)
	if g.Enter() == nil {
		t.Fatal("expected failure")
	}
	if f.calls[len(f.calls)-1] != "restore" {
		t.Fatalf("calls=%v", f.calls)
	}
	p := &fakeTerminal{in: true, out: true}
	pg, _ := NewGuard(p, true)
	if err := pg.Enter(); err != nil {
		t.Fatal(err)
	}
	_ = pg.Restore()
	if !reflect.DeepEqual(p.calls, []string{"capture", "raw", "restore"}) {
		t.Fatalf("plain calls=%v", p.calls)
	}
}
