//go:build linux

package installer

import (
	"errors"
	"os"
	"strings"
)

func DetectPlatform() (CapabilitySnapshot, error) {
	proc1, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		return CapabilitySnapshot{}, errors.New("platform_probe_failed")
	}
	version, _ := os.ReadFile("/proc/version")
	_, cgroupErr := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	_, pidfdErr := os.Stat("/proc/self/fd")
	wsl := strings.Contains(strings.ToLower(string(version)), "microsoft") || os.Getenv("WSL_INTEROP") != ""
	container := os.Getenv("container") != "" || strings.TrimSpace(string(proc1)) != "systemd"
	s := CapabilitySnapshot{Systemd: strings.TrimSpace(string(proc1)) == "systemd", CgroupV2: cgroupErr == nil, Pidfd: pidfdErr == nil, TrustedLauncher: false, UserNamespace: false}
	s.DelegatedCgroup = false
	s.Platform = PlatformBareLinux
	if wsl {
		s.Platform = PlatformWSLLimited
	}
	if container {
		if s.Systemd {
			s.Platform = PlatformContainerUnit
		} else {
			s.Platform = PlatformContainerPlain
		}
	}
	return s, nil
}
