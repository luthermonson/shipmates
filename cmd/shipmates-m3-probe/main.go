//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/fleetconfig"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
	"golang.org/x/sys/unix"
)

const installedHelper = "/usr/libexec/shipmates/shipmates-cgroup-launcher"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "shipmates-m3-probe: %s\n", err.Error())
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("cgroup-root", "", "pre-provisioned delegated cgroup root")
	fleetConfig := flag.String("fleet-config", "", "protected Fleet configuration path")
	evidence := flag.String("evidence-dir", "", "administrator-provided evidence directory")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	if *root == "" {
		return errors.New("SHIPMATES_CGROUP_ROOT_required")
	}
	if err := protectedFile(*fleetConfig); err != nil {
		return errors.New("fleet_config_" + classify(err))
	}
	profileRoot := filepath.Dir(*fleetConfig)
	profile, err := fleetconfig.LoadRuntimeProfileAt(*fleetConfig, fleetconfig.DefaultTrustRoot, filepath.Join(profileRoot, "credentials", "ship.json"), filepath.Join(profileRoot, "credentials", "commander.json"), false)
	if err != nil {
		return errors.New("fleet_config_invalid")
	}
	if err := evidenceDir(*evidence); err != nil {
		return errors.New("evidence_dir_" + classify(err))
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("mountinfo_unreadable")
	}
	selfCgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return errors.New("self_cgroup_unreadable")
	}
	layout, err := codexapp.DiscoverCgroup2Layout(mountInfo, selfCgroup)
	if err != nil {
		return errors.New("layout_" + classify(err))
	}
	delegated, err := codexapp.OpenDelegatedCgroup(*root, layout)
	if err != nil {
		return errors.New("delegated_root_" + classify(err))
	}
	defer delegated.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := fleettunnel.QualifyProfile(ctx, profile); err != nil {
		return errors.New("fleet_qualification_" + classify(err))
	}
	target, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		return errors.New("probe_target_unavailable")
	}
	cmd := exec.Command(target, "-c", "setsid sleep 30")
	contained, err := codexapp.StartContainedWithHelperInDir(delegated, installedHelper, cmd, fmt.Sprintf("probe-%d", time.Now().UnixNano()))
	if err != nil {
		return errors.New("probe_launch_" + classify(err))
	}
	defer func() {
		if contained != nil {
			_ = contained.ForceKill(context.Background())
			_ = contained.Close()
		}
	}()
	if ctx.Err() != nil {
		_ = contained.ForceKill(context.Background())
		return errors.New("probe_cancelled")
	}
	if err := probeControls(contained.Path(), contained.PID()); err != nil {
		_ = contained.ForceKill(context.Background())
		return errors.New("probe_controls_" + classify(err))
	}
	if err := contained.ForceKill(context.Background()); err != nil {
		return errors.New("probe_cleanup_" + classify(err))
	}
	empty, err := contained.Empty()
	if err != nil || !empty {
		return errors.New("probe_populated_nonzero")
	}
	if err := contained.Close(); err != nil {
		return errors.New("probe_leaf_cleanup")
	}
	contained = nil
	if err := writeEvidence(*evidence, map[string]any{"event": "delegated_cgroup_probe", "result": "pass", "version": "shipmates-cgroup-launcher-v1"}); err != nil {
		return errors.New("evidence_write_failed")
	}
	return nil
}

func probeControls(path string, pid int) error {
	if pid <= 0 || path == "" {
		return errors.New("identity_missing")
	}
	if _, err := os.Stat(filepath.Join(path, "cgroup.procs")); err != nil {
		return errors.New("procs_unavailable")
	}
	if _, err := os.Stat(filepath.Join(path, "cgroup.kill")); err != nil {
		return errors.New("kill_unavailable")
	}
	if b, err := os.ReadFile(filepath.Join(path, "cgroup.events")); err != nil || !containsPopulated(b, "1") {
		return errors.New("populated_transition_missing")
	}
	return nil
}

func containsPopulated(b []byte, value string) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "populated "+value {
			return true
		}
	}
	return false
}

func protectedFile(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("missing_or_unsafe")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0022 != 0 || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("not_protected")
	}
	return nil
}

func evidenceDir(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("missing_or_unsafe")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return errors.New("not_directory")
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode()&0022 != 0 || (sys.Uid != 0 && int(sys.Uid) != os.Getuid()) {
		return errors.New("not_directory")
	}
	return nil
}

func classify(err error) string {
	if err == nil {
		return "unknown"
	}
	_ = err
	return "invalid"
}

func writeEvidence(dir string, value map[string]any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, "m3-cgroup-probe.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "m3-cgroup-probe.json")
	if f == nil {
		_ = unix.Close(fd)
		return errors.New("evidence_descriptor_unavailable")
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
