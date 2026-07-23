package none

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

func sleeper(seconds int) *exec.Cmd {
	if runtime.GOOS == "windows" {
		// ping loopback: portable "sleep N" on Windows.
		s := "0"
		for i := 0; i < seconds; i++ {
			s = itostr(seconds + 1)
		}
		return exec.Command("cmd", "/C", "ping", "-n", s, "127.0.0.1", ">nul")
	}
	return exec.Command("sleep", itostr(seconds))
}

func itostr(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestKind(t *testing.T) {
	if New().Kind() != "none" {
		t.Errorf("kind mismatch")
	}
}

func TestStart_NaturalExit(t *testing.T) {
	h, err := New().Start(sleeper(1), containment.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-h.Done():
		if ev.Reason != containment.ReasonExited {
			t.Errorf("reason=%q want exited", ev.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process never exited")
	}
}

func TestClose(t *testing.T) {
	h, err := New().Start(sleeper(60), containment.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Errorf("close: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process still alive after Close")
	}
}
