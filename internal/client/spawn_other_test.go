//go:build !windows

package client

import (
	"os/exec"
	"testing"
)

// detach must put the child in its own process group so it survives the CLI
// exiting and isn't hit by signals aimed at the caller's terminal.
func TestDetachSetsPgid(t *testing.T) {
	cmd := exec.Command("true")
	detach(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("detach left SysProcAttr nil")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid = false, want the child in its own process group")
	}
}
