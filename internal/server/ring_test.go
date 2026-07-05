package server

import (
	"bytes"
	"testing"
)

func TestRingBasics(t *testing.T) {
	r := newRing(8)
	r.Write([]byte("abc"))
	if got := r.Snapshot(); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("want abc, got %q", got)
	}
	r.Write([]byte("defgh")) // exactly full
	if got := r.Snapshot(); !bytes.Equal(got, []byte("abcdefgh")) {
		t.Fatalf("want abcdefgh, got %q", got)
	}
	r.Write([]byte("XY")) // evicts ab
	if got := r.Snapshot(); !bytes.Equal(got, []byte("cdefghXY")) {
		t.Fatalf("want cdefghXY, got %q", got)
	}
}

func TestRingOversizedChunk(t *testing.T) {
	r := newRing(4)
	r.Write([]byte("0123456789"))
	if got := r.Snapshot(); !bytes.Equal(got, []byte("6789")) {
		t.Fatalf("want tail 6789, got %q", got)
	}
	r.Write([]byte("ab"))
	if got := r.Snapshot(); !bytes.Equal(got, []byte("89ab")) {
		t.Fatalf("want 89ab, got %q", got)
	}
}

func TestRingWrapStress(t *testing.T) {
	r := newRing(16)
	var mirror []byte
	for i := 0; i < 100; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i%26)}, 1+i%7)
		r.Write(chunk)
		mirror = append(mirror, chunk...)
		if len(mirror) > 16 {
			mirror = mirror[len(mirror)-16:]
		}
		if got := r.Snapshot(); !bytes.Equal(got, mirror) {
			t.Fatalf("iter %d: want %q, got %q", i, mirror, got)
		}
	}
}
