//go:build !windows

package dashboard

import "os"

// Unix terminals interpret ANSI escape sequences unconditionally, so there is
// no output mode to enable and nothing to reverse. Only the Windows console
// needs the counterpart in vt_windows.go.
func enableVirtualTerminal(*os.File) (func() error, error) { return nil, nil }
