package bridge

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the bridge against the given coordination server and blocks until
// the operator quits.
//
// Bubble Tea owns raw mode, resize detection and input decoding on Linux, macOS
// and Windows — there is no termios or ConPTY code anywhere in this package, by
// design. Alt-screen keeps the operator's scrollback intact, and the program's
// context is wired to ctx so a signal tears the streams down with it.
func Run(ctx context.Context, opts Options) error {
	m := New(opts)
	defer m.Close()

	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		// Bracketed paste stays ON: it is how a paste arrives as one KeyMsg with
		// Paste set, which EncodeKey re-wraps for the mate. Mouse reporting stays
		// OFF: the bridge does not forward mouse events, and capturing them would
		// break the terminal's own selection and scroll for no gain.
		tea.WithOutput(os.Stdout),
	)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	return nil
}
