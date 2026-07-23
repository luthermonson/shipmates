//go:build unix

package installer

import (
	"context"
	"errors"
)

const QualifierUnitName = "shipmates-m3-qualifier.service"

type QualifierUnitState uint8

const (
	QualifierStateUnknown QualifierUnitState = iota
	QualifierStateInactive
	QualifierStateActive
	QualifierStateFailed
	QualifierStateTransitioning
	QualifierStateMissing
)

// LocalSystemdManager is the deliberately narrow manager seam. Implementations
// must only inspect the fixed qualifier unit; they cannot start, stop, reload,
// or select another unit.
type LocalSystemdManager interface {
	UnitState(context.Context, string) (QualifierUnitState, error)
	PendingJob(context.Context, string) (bool, error)
}

type unavailableSystemdManager struct{}

func (unavailableSystemdManager) UnitState(context.Context, string) (QualifierUnitState, error) {
	return QualifierStateUnknown, errors.New("qualifier_manager_unavailable")
}
func (unavailableSystemdManager) PendingJob(context.Context, string) (bool, error) {
	return false, errors.New("qualifier_manager_unavailable")
}

// QualifierFence is the fixed lifecycle boundary used by public destructive
// operations. CheckInactive is retained for compatibility with older injected
// test fences; Acquire is the production path and includes the shared lock.
type QualifierFence interface {
	CheckInactive() error
	Acquire() (*FenceLease, error)
}

type fixedQualifierFence struct {
	root              string
	manager           LocalSystemdManager
	allowCurrentOwner bool
}

func NewQualifierFence(root string, manager LocalSystemdManager) QualifierFence {
	if manager == nil {
		manager = unavailableSystemdManager{}
	}
	// Non-root roots are only test fixtures; allowing their owner keeps the
	// production root-owned rule intact while making the seam deterministic.
	return fixedQualifierFence{root: root, manager: manager, allowCurrentOwner: root != "/"}
}

// NewProductionQualifierFence has no systemd command fallback. A host adapter
// may be supplied at a higher, explicitly authorized boundary; the default is
// fail-closed.
func NewProductionQualifierFence(root string) QualifierFence {
	return NewQualifierFence(root, productionSystemdManager{})
}

func (f fixedQualifierFence) CheckInactive() error {
	state, err := f.manager.UnitState(context.Background(), QualifierUnitName)
	// A fresh install owns the unit file, so an authenticated systemd
	// NoSuchUnit response is also quiescent. Unknown and malformed manager
	// responses remain fail-closed.
	if err != nil || (state != QualifierStateInactive && state != QualifierStateMissing) {
		return errors.New("qualifier_state_refused")
	}
	pending, err := f.manager.PendingJob(context.Background(), QualifierUnitName)
	if err != nil || pending {
		return errors.New("qualifier_state_refused")
	}
	return nil
}

func (f fixedQualifierFence) Acquire() (*FenceLease, error) {
	if err := f.CheckInactive(); err != nil {
		return nil, err
	}
	lease, err := acquireQualifierLifecycleLease(f.root, f.allowCurrentOwner)
	if err != nil {
		return nil, err
	}
	if err := lease.Recheck(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}
