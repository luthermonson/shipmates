package server

import (
	"bytes"
	"testing"
)

func TestTrackModesLatchAndReplay(t *testing.T) {
	p := &ptyProc{modes: map[int]bool{}}

	// startup: alt screen + alternate scroll + bracketed paste on
	p.trackModes([]byte("\x1b[?1049h\x1b[?1007h\x1b[?2004hhello"))
	// later: cursor hidden then shown again
	p.trackModes([]byte("\x1b[?25l...\x1b[?25h"))
	// semicolon list + a reset
	p.trackModes([]byte("\x1b[?1002;1006h\x1b[?2004l"))

	prefix := p.modePrefix()
	want := []string{"\x1b[?25h", "\x1b[?1002h", "\x1b[?1006h", "\x1b[?1007h", "\x1b[?1049h", "\x1b[?2004l"}
	for _, w := range want {
		if !bytes.Contains(prefix, []byte(w)) {
			t.Errorf("prefix missing %q; got %q", w, prefix)
		}
	}
}

func TestTrackModesSplitAcrossChunks(t *testing.T) {
	p := &ptyProc{modes: map[int]bool{}}
	seq := []byte("\x1b[?1049h")
	p.trackModes(seq[:3]) // "\x1b[?"
	p.trackModes(seq[3:]) // "1049h"
	if !p.modes[1049] {
		t.Fatalf("split sequence not latched: %+v", p.modes)
	}
}

func TestTrackModesCarryNoDoubleCount(t *testing.T) {
	p := &ptyProc{modes: map[int]bool{}}
	// a full sequence at the very end of a chunk lands in the carry; the next
	// chunk must not flip it back if it re-matches — latching the same value
	// twice is harmless, but a LATER reset must win over a carried set.
	p.trackModes([]byte("\x1b[?2004h"))
	p.trackModes([]byte("\x1b[?2004l"))
	if p.modes[2004] {
		t.Fatalf("later reset lost: %+v", p.modes)
	}
}
