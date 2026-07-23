package installer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeQualifierManager struct {
	state   QualifierUnitState
	pending bool
	err     error
	unit    string
}

func (m *fakeQualifierManager) UnitState(_ context.Context, unit string) (QualifierUnitState, error) {
	m.unit = unit
	return m.state, m.err
}
func (m *fakeQualifierManager) PendingJob(_ context.Context, unit string) (bool, error) {
	m.unit = unit
	return m.pending, m.err
}

func TestQualifierFenceRequiresFixedInactiveUnit(t *testing.T) {
	for name, state := range map[string]QualifierUnitState{
		"active": QualifierStateActive, "failed": QualifierStateFailed,
		"transitioning": QualifierStateTransitioning,
		"unknown":       QualifierStateUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeQualifierManager{state: state}
			f := NewQualifierFence(t.TempDir(), m)
			if err := f.CheckInactive(); err == nil || err.Error() != "qualifier_state_refused" {
				t.Fatalf("state %v accepted: %v", state, err)
			}
			if m.unit != QualifierUnitName {
				t.Fatalf("manager selected %q", m.unit)
			}
		})
	}
	if err := NewQualifierFence(t.TempDir(), &fakeQualifierManager{state: QualifierStateMissing}).CheckInactive(); err != nil {
		t.Fatalf("authenticated missing unit must permit first install: %v", err)
	}
	m := &fakeQualifierManager{state: QualifierStateInactive, pending: true}
	if err := NewQualifierFence(t.TempDir(), m).CheckInactive(); err == nil {
		t.Fatal("pending job accepted")
	}
}

func TestQualifierFenceLeaseIdentityAndConcurrency(t *testing.T) {
	root := t.TempDir()
	m := &fakeQualifierManager{state: QualifierStateInactive}
	f := NewQualifierFence(root, m)
	first, err := f.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Recheck(); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, qualifierLifecycleLock)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Recheck(); err == nil {
		t.Fatal("replacement lock accepted")
	}
}
