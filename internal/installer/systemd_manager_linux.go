//go:build linux

package installer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const systemdQueryTimeout = 2 * time.Second

type productionSystemdManager struct{}

type systemdJob struct {
	ID     uint32
	Unit   string
	Kind   string
	State  string
	Object dbus.ObjectPath
}

func (productionSystemdManager) UnitState(ctx context.Context, unit string) (QualifierUnitState, error) {
	if unit != QualifierUnitName {
		return QualifierStateUnknown, errors.New("qualifier_unit_refused")
	}
	ctx, cancel := context.WithTimeout(ctx, systemdQueryTimeout)
	defer cancel()
	conn, err := openSystemBus(ctx)
	if err != nil {
		return QualifierStateUnknown, err
	}
	defer conn.Close()

	var path dbus.ObjectPath
	err = conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").
		CallWithContext(ctx, "org.freedesktop.systemd1.Manager.GetUnit", 0, unit).Store(&path)
	if err != nil {
		if isNoSuchUnit(err) {
			return QualifierStateMissing, nil
		}
		return QualifierStateUnknown, errors.New("qualifier_manager_unavailable")
	}
	obj := conn.Object("org.freedesktop.systemd1", path)
	load, err := unitProperty(ctx, obj, "LoadState")
	if err != nil {
		return QualifierStateUnknown, err
	}
	active, err := unitProperty(ctx, obj, "ActiveState")
	if err != nil {
		return QualifierStateUnknown, err
	}
	sub, err := unitProperty(ctx, obj, "SubState")
	if err != nil {
		return QualifierStateUnknown, err
	}
	if load != "loaded" {
		return QualifierStateUnknown, nil
	}
	if active == "inactive" && sub == "dead" {
		return QualifierStateInactive, nil
	}
	if active == "failed" || sub == "failed" {
		return QualifierStateFailed, nil
	}
	if active == "active" && sub == "running" {
		return QualifierStateActive, nil
	}
	return QualifierStateTransitioning, nil
}

func (productionSystemdManager) PendingJob(ctx context.Context, unit string) (bool, error) {
	if unit != QualifierUnitName {
		return false, errors.New("qualifier_unit_refused")
	}
	ctx, cancel := context.WithTimeout(ctx, systemdQueryTimeout)
	defer cancel()
	conn, err := openSystemBus(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var jobs []systemdJob
	if err := conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").
		CallWithContext(ctx, "org.freedesktop.systemd1.Manager.ListJobs", 0).Store(&jobs); err != nil {
		return false, errors.New("qualifier_manager_unavailable")
	}
	for _, job := range jobs {
		if job.Unit == unit {
			return true, nil
		}
	}
	return false, nil
}

func openSystemBus(ctx context.Context) (*dbus.Conn, error) {
	conn, err := dbus.SystemBusPrivate(dbus.WithContext(ctx))
	if err != nil {
		return nil, errors.New("qualifier_manager_unavailable")
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, errors.New("qualifier_manager_unavailable")
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, errors.New("qualifier_manager_unavailable")
	}
	return conn, nil
}

func unitProperty(ctx context.Context, obj dbus.BusObject, name string) (string, error) {
	call := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.systemd1.Unit", name)
	if call.Err != nil || len(call.Body) != 1 {
		return "", errors.New("qualifier_manager_unavailable")
	}
	variant, ok := call.Body[0].(dbus.Variant)
	if !ok {
		return "", errors.New("qualifier_manager_malformed")
	}
	value, ok := variant.Value().(string)
	if !ok {
		return "", errors.New("qualifier_manager_malformed")
	}
	return value, nil
}

func isNoSuchUnit(err error) bool {
	var dbusErr dbus.Error
	return errors.As(err, &dbusErr) && strings.HasSuffix(dbusErr.Name, ".NoSuchUnit")
}
