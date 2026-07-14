//go:build !linux

package dashboard

import (
	"context"
	"os"
)

// unsupportedNativeTerminal makes the platform limitation visible to NewGuard
// before any TTY inspection, capture, or mutation can occur.
type unsupportedNativeTerminal struct{}

func NewNativeTerminal(_, _ *os.File) Terminal               { return unsupportedNativeTerminal{} }
func (unsupportedNativeTerminal) NativeTTYError() error      { return ErrNativeTTYUnsupported }
func (unsupportedNativeTerminal) StdinTTY() bool             { return false }
func (unsupportedNativeTerminal) StdoutTTY() bool            { return false }
func (unsupportedNativeTerminal) Capture() (any, error)      { return nil, ErrNativeTTYUnsupported }
func (unsupportedNativeTerminal) SetRaw(any) error           { return ErrNativeTTYUnsupported }
func (unsupportedNativeTerminal) AlternateScreen(bool) error { return ErrNativeTTYUnsupported }
func (unsupportedNativeTerminal) CursorVisible(bool) error   { return ErrNativeTTYUnsupported }
func (unsupportedNativeTerminal) Restore(any) error          { return ErrNativeTTYUnsupported }

// NativeEditor exists on unsupported platforms so command packages cross-build.
// The terminal guard refuses the runtime path before an editor is constructed.
type NativeEditor struct{}

func NewNativeEditor(*os.File, ...*os.File) *NativeEditor { return &NativeEditor{} }
func (*NativeEditor) Next(context.Context) (Input, error) {
	return Input{}, ErrNativeTTYUnsupported
}

func NativeSize(*os.File) Size { return Size{Width: 80, Height: 24} }

func NativeRenderer(*os.File, bool, ...*NativeEditor) func(Screen) error {
	return func(Screen) error { return ErrNativeTTYUnsupported }
}
