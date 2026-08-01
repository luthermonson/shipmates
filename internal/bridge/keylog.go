package bridge

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TEMPORARY INSTRUMENTATION.
//
// This file exists to answer one question that cannot be answered without a real
// keyboard on a real Windows console: what does bubbletea actually report for the
// keys an operator presses, and what does EncodeKey turn that into?
//
// The motivating symptom was a literal "^@" appearing in a mate's prompt, i.e. a
// NUL byte reaching the PTY. EncodeKey no longer emits NUL under any
// circumstances (see keys.go), so the bug is contained — but "contained" is not
// "explained", and a decoder that reports the wrong KeyType for a key would still
// send the wrong bytes for that key. The log below is the measurement that tells
// us which keys, if any, are still mis-decoded.
//
// Delete this file once the question is answered.
//
// Enable with:
//
//	SHIPMATES_BRIDGE_KEYLOG=/path/to/keys.log shipmates bridge
//
// One line per tea.KeyMsg, in the order Update saw them (the write happens on the
// Update goroutine, NOT in a tea.Cmd, precisely so the file order is the keystroke
// order). Fields are space-separated key=value; values that can contain spaces are
// %q-quoted.

// keyLogEnv names the environment variable that turns the log on. Empty or unset
// means no instrumentation at all — no file is opened and record() is a nil-method
// call that returns immediately.
const keyLogEnv = "SHIPMATES_BRIDGE_KEYLOG"

// keyLog appends one line per key event to a file. A nil *keyLog is valid and
// records nothing, so callers never branch.
type keyLog struct {
	mu sync.Mutex
	f  *os.File
	n  int
}

// openKeyLog returns a live log when keyLogEnv names a writable path, and nil
// otherwise. A path that cannot be opened is reported on stderr and then ignored:
// diagnostics must never stop the bridge from starting.
func openKeyLog() *keyLog {
	path := strings.TrimSpace(os.Getenv(keyLogEnv))
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s=%q: %v\n", keyLogEnv, path, err)
		return nil
	}
	l := &keyLog{f: f}
	l.line("--- bridge keylog opened " + time.Now().Format(time.RFC3339Nano) +
		" goos=" + runtime.GOOS + " ---")
	return l
}

// record writes one event. mode is the bridge's input mode at the time, encoded is
// exactly what EncodeKey returned (nil included, as encoded=""), and sent reports
// whether those bytes were actually handed to the mate.
func (l *keyLog) record(k tea.KeyMsg, mode string, encoded []byte, sent bool) {
	if l == nil {
		return
	}
	var b strings.Builder
	b.WriteString(time.Now().Format("15:04:05.000000"))
	b.WriteString(" type=")
	b.WriteString(strconv.Itoa(int(k.Type)))
	b.WriteString(" string=")
	b.WriteString(strconv.Quote(k.String()))
	b.WriteString(" typename=")
	b.WriteString(strconv.Quote(k.Type.String()))
	b.WriteString(" runes=")
	b.WriteString(strconv.Quote(string(k.Runes)))
	b.WriteString(" codepoints=[")
	for i, r := range k.Runes {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(int(r)))
	}
	b.WriteString("] alt=")
	b.WriteString(strconv.FormatBool(k.Alt))
	b.WriteString(" paste=")
	b.WriteString(strconv.FormatBool(k.Paste))
	b.WriteString(" mode=")
	b.WriteString(mode)
	b.WriteString(" encoded=")
	b.WriteString(strconv.Quote(hexBytes(encoded)))
	b.WriteString(" nbytes=")
	b.WriteString(strconv.Itoa(len(encoded)))
	b.WriteString(" sent=")
	b.WriteString(strconv.FormatBool(sent))
	l.line(b.String())
}

// hexBytes renders bytes as space-separated two-digit hex, so a NUL is visible as
// "00" instead of vanishing into a quoted string.
func hexBytes(p []byte) string {
	if len(p) == 0 {
		return ""
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(p)*3-1)
	for i, c := range p {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func (l *keyLog) line(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	l.n++
	_, _ = l.f.WriteString(s + "\n")
	// Flushed per line on purpose: the operator will be reading this while the
	// bridge is still running, and a crash must not lose the keystroke that caused
	// it.
	_ = l.f.Sync()
}

// count reports how many lines have been written. Used by tests.
func (l *keyLog) count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// Close flushes and closes the file. Safe on nil and safe to call twice.
func (l *keyLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}
