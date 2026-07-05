package server

import "testing"

func TestWriterLock(t *testing.T) {
	p := &ptyProc{subs: map[int]chan []byte{}, modes: map[int]bool{}}

	if !p.claimWriter("") {
		t.Fatal("empty client (internal write) must bypass the lock")
	}
	if p.writer != "" {
		t.Fatal("empty client must not claim the lock")
	}

	if !p.claimWriter("alice") {
		t.Fatal("first typist should claim an unclaimed lock")
	}
	if !p.claimWriter("alice") {
		t.Fatal("holder should keep typing")
	}
	if p.claimWriter("bob") {
		t.Fatal("second viewer must be rejected while alice holds the lock")
	}
	if !p.claimWriter("") {
		t.Fatal("internal writes still bypass a held lock")
	}

	// takeover then release
	p.mu.Lock()
	p.writer = "bob"
	p.mu.Unlock()
	if p.claimWriter("alice") {
		t.Fatal("alice must be locked out after bob's takeover")
	}
	p.mu.Lock()
	if p.writer == "bob" {
		p.writer = ""
	}
	p.mu.Unlock()
	if !p.claimWriter("alice") {
		t.Fatal("release should let the next typist claim")
	}
}
