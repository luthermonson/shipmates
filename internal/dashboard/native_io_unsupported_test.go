//go:build !linux

package dashboard

import (
	"errors"
	"testing"
)

func TestUnsupportedNativeTerminalFailsBeforeMutation(t *testing.T) {
	terminal := NewNativeTerminal(nil, nil)
	guard, err := NewGuard(terminal, false)
	if guard != nil {
		t.Fatal("unsupported platform returned a terminal guard")
	}
	if !errors.Is(err, ErrNativeTTYUnsupported) {
		t.Fatalf("error = %v", err)
	}
}
