//go:build windows

package client

import (
	"os/exec"
	"testing"
)

// detach must mark the child DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP so the
// captain server outlives the CLI that started it and doesn't take a Ctrl-C
// aimed at the caller's console.
func TestDetachSetsWindowsCreationFlags(t *testing.T) {
	const (
		detachedProcess    = 0x00000008
		createNewProcGroup = 0x00000200
	)
	cmd := exec.Command("cmd", "/c", "exit")
	detach(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("detach left SysProcAttr nil")
	}
	got := cmd.SysProcAttr.CreationFlags
	if got&detachedProcess == 0 {
		t.Errorf("CreationFlags = %#x, missing DETACHED_PROCESS", got)
	}
	if got&createNewProcGroup == 0 {
		t.Errorf("CreationFlags = %#x, missing CREATE_NEW_PROCESS_GROUP", got)
	}
}
