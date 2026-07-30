// Package dashboard contains the transport-facing, presentation-independent
// pieces of interactive open. Rendering and line editing intentionally live
// outside this package.
package dashboard

import (
	"context"
	"errors"
	"sync"
)

var ErrNotTTY = errors.New("open is interactive; stdin and stdout must be TTYs; scripts should use live, tell, interrupt, and feed")

// ErrNativeTTYUnsupported no longer means "this GOOS": the native terminal is
// portable across linux, macOS, and Windows. It now wraps a real refusal from
// the terminal itself, which is what a platform golang.org/x/term has no
// implementation for (js/wasm, for instance) produces at snapshot time.
var ErrNativeTTYUnsupported = errors.New("open interactive terminal support is unavailable on this platform; use live, tell, interrupt, and feed")

// Terminal is intentionally small so exit-path behavior is deterministic in
// tests and no real terminal is needed.
type Terminal interface {
	StdinTTY() bool
	StdoutTTY() bool
	Capture() (any, error)
	SetRaw(any) error
	AlternateScreen(bool) error
	CursorVisible(bool) error
	Restore(any) error
}

// Guard owns all terminal mutations and funnels every exit through Restore.
// Cleanup is installed by construction before Enter performs its first change.
type Guard struct {
	t     Terminal
	plain bool
	state any
	once  sync.Once
	err   error
}

func NewGuard(t Terminal, plain bool) (*Guard, error) {
	if t == nil || !t.StdinTTY() || !t.StdoutTTY() {
		return nil, ErrNotTTY
	}
	state, err := t.Capture()
	if err != nil {
		return nil, err
	}
	return &Guard{t: t, plain: plain, state: state}, nil
}

func (g *Guard) Enter() (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, g.Restore())
		}
	}()
	if err = g.t.SetRaw(g.state); err != nil {
		return err
	}
	if g.plain {
		return nil
	}
	if err = g.t.AlternateScreen(true); err != nil {
		return err
	}
	if err = g.t.CursorVisible(false); err != nil {
		return err
	}
	return nil
}

func (g *Guard) Restore() error {
	g.once.Do(func() {
		// Each reversal is attempted even when an earlier one fails.
		var errs []error
		if !g.plain {
			errs = append(errs, g.t.CursorVisible(true))
			errs = append(errs, g.t.AlternateScreen(false))
		}
		errs = append(errs, g.t.Restore(g.state))
		g.err = errors.Join(errs...)
	})
	return g.err
}

// Run installs restoration around a context-bound controller loop. The command
// context maps Ctrl-C and catchable termination signals to local detach; Run
// does not signal or otherwise mutate the owned backend session.
func Run(ctx context.Context, g *Guard, controller func(context.Context) error) (err error) {
	if err = g.Enter(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, g.Restore()) }()
	return controller(ctx)
}
