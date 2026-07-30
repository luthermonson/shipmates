package server

import (
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
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
	// Nine mode bits on unix, an enumerated DACL on Windows — Windows has no
	// mode bits, so os.Stat there reports a synthesized 0666 and a literal
	// comparison could never hold.
	if err := project.VerifyPrivateFile(p); err != nil {
		t.Fatalf("audit file is not private: %v", err)
	}
}
