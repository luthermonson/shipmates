package bridge

import (
	"bytes"
	"strings"
	"testing"
)

// drain mimics what the drain command does: take chunks until the queue reports
// empty, recording them in order.
func drain(p *inputPipe, chunk int) [][]byte {
	var out [][]byte
	for {
		c := p.take(chunk)
		if c == nil {
			return out
		}
		out = append(out, c)
	}
}

func TestInputPipeOnlyOneDrainerAtATime(t *testing.T) {
	p := &inputPipe{}
	start, dropped := p.enqueue([]byte("ab"))
	if !start || dropped {
		t.Fatalf("first enqueue: start=%v dropped=%v, want true/false", start, dropped)
	}
	// While a drain is running, further enqueues must NOT start a second one —
	// two concurrent drainers would interleave and reorder the bytes.
	if start, _ := p.enqueue([]byte("cd")); start {
		t.Error("second enqueue started a competing drain")
	}
	if start, _ := p.enqueue([]byte("ef")); start {
		t.Error("third enqueue started a competing drain")
	}
	got := bytes.Join(drain(p, 64), nil)
	if string(got) != "abcdef" {
		t.Errorf("drained %q, want %q", got, "abcdef")
	}
	// The drain is over, so the next enqueue owns the next one.
	if start, _ := p.enqueue([]byte("g")); !start {
		t.Error("enqueue after a finished drain did not start a new one")
	}
}

func TestInputPipeChunksWithoutLosingBytes(t *testing.T) {
	p := &inputPipe{}
	want := strings.Repeat("z", maxInputChunk*2+37)
	p.enqueue([]byte(want))
	chunks := drain(p, maxInputChunk)
	if len(chunks) != 3 {
		t.Errorf("split into %d chunks, want 3", len(chunks))
	}
	for i, c := range chunks[:len(chunks)-1] {
		if len(c) != maxInputChunk {
			t.Errorf("chunk %d is %d bytes, want %d", i, len(c), maxInputChunk)
		}
	}
	if got := string(bytes.Join(chunks, nil)); got != want {
		t.Errorf("chunking lost or reordered bytes: got %d of %d", len(got), len(want))
	}
}

func TestInputPipeBoundsItsQueue(t *testing.T) {
	p := &inputPipe{}
	if _, dropped := p.enqueue(make([]byte, maxInputQueue)); dropped {
		t.Fatal("a queue exactly at the cap was rejected")
	}
	if _, dropped := p.enqueue([]byte("x")); !dropped {
		t.Error("enqueue past the cap was accepted, so the queue is unbounded")
	}
	if p.queued() != maxInputQueue {
		t.Errorf("queue grew past the cap to %d", p.queued())
	}
}

func TestInputPipeAbortDropsTheBacklog(t *testing.T) {
	p := &inputPipe{}
	p.enqueue([]byte("stale keystrokes"))
	p.abort()
	if p.queued() != 0 {
		t.Errorf("abort left %d bytes queued", p.queued())
	}
	// abort also ends the drain, so the next keystroke starts a fresh one rather
	// than being stranded behind a drainer that has already returned.
	if start, _ := p.enqueue([]byte("fresh")); !start {
		t.Error("enqueue after abort did not start a drain")
	}
}
