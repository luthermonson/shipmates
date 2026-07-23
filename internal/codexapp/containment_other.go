//go:build !linux

package codexapp

import (
	"context"
	"errors"
	"os/exec"
)

type ExecutionContainment struct{}
type ExecutionCapabilities struct{}

func (ExecutionCapabilities) AssessmentEnabled() bool          { return false }
func DetectExecutionCapabilities(string) ExecutionCapabilities { return ExecutionCapabilities{} }
func DetectExecutionCapabilitiesAt(string, string) ExecutionCapabilities {
	return ExecutionCapabilities{}
}

func StartContained(*exec.Cmd, string) (*ExecutionContainment, error) {
	return nil, errors.New("execution containment unavailable")
}
func StartContainedAt(string, *exec.Cmd, string) (*ExecutionContainment, error) {
	return nil, errors.New("execution containment unavailable")
}
func StartContainedWithHelper(string, *exec.Cmd, string) (*ExecutionContainment, error) {
	return nil, errors.New("execution containment unavailable")
}
func StartContainedWithHelperCurrent(string, *exec.Cmd, string) (*ExecutionContainment, error) {
	return nil, errors.New("execution containment unavailable")
}
func StartContainedWithHelperAt(string, string, *exec.Cmd, string) (*ExecutionContainment, error) {
	return nil, errors.New("execution containment unavailable")
}
func (*ExecutionContainment) Path() string { return "" }
func (*ExecutionContainment) PID() int     { return 0 }
func (*ExecutionContainment) GracefulTerminate(context.Context) error {
	return errors.New("execution containment unavailable")
}
func (*ExecutionContainment) ForceKill(context.Context) error {
	return errors.New("execution containment unavailable")
}
func (*ExecutionContainment) killCgroup() error {
	return errors.New("execution containment unavailable")
}
func (*ExecutionContainment) Empty() (bool, error) {
	return false, errors.New("execution containment unavailable")
}
func (*ExecutionContainment) Close() error { return errors.New("execution containment unavailable") }
