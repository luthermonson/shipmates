package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/livesession"
)

func TestProductionRemoteInterruptConstructorCreatesPrivateAudit(t *testing.T) {
	root := t.TempDir()
	s := &Server{projectRoot: root, liveSessions: &livesession.Manager{}}
	if err := s.openProductionRemoteInterrupt(); err != nil {
		t.Fatal(err)
	}
	if s.remoteInterrupt == nil {
		t.Fatal("production coordinator not installed")
	}
	p := filepath.Join(root, ".shipmates", "remote-interrupt", "audit", "remote-interrupt.audit")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("audit=%v err=%v", st, err)
	}
}
