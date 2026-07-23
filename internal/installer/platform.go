package installer

import (
	"errors"
	"strings"
)

type Platform string

const (
	PlatformBareLinux      Platform = "bare-linux"
	PlatformWSLCapable     Platform = "wsl-capable"
	PlatformWSLLimited     Platform = "wsl-limited"
	PlatformContainerUnit  Platform = "container-systemd"
	PlatformContainerPlain Platform = "container-nonsystemd"
	PlatformUnknown        Platform = "unknown"
)

type CapabilitySnapshot struct {
	Platform        Platform `json:"platform"`
	Systemd         bool     `json:"systemd"`
	CgroupV2        bool     `json:"cgroup_v2"`
	DelegatedCgroup bool     `json:"delegated_cgroup"`
	Pidfd           bool     `json:"pidfd"`
	TrustedLauncher bool     `json:"trusted_launcher"`
	UserNamespace   bool     `json:"user_namespace"`
	ReadOnlyRoot    bool     `json:"read_only_root"`
}

type Composition struct {
	Mode             string   `json:"mode"`
	Platform         Platform `json:"platform"`
	Hardened         bool     `json:"hardened"`
	ServiceIdentity  string   `json:"service_identity"`
	CredentialLayout string   `json:"credential_layout"`
	Guidance         string   `json:"guidance"`
	Qualification    bool     `json:"qualification_started"`
	Profile          string   `json:"profile,omitempty"`
}

func DetectFromSnapshot(s CapabilitySnapshot) (Composition, error) {
	if s.Platform == PlatformUnknown {
		return Composition{}, errors.New("platform_unknown")
	}
	c := Composition{Platform: s.Platform, ServiceIdentity: "shipmates", Qualification: false}
	switch s.Platform {
	case PlatformBareLinux, PlatformWSLCapable, PlatformContainerUnit:
		if s.Systemd && s.CgroupV2 && s.DelegatedCgroup && s.Pidfd && s.TrustedLauncher && !s.UserNamespace && !s.ReadOnlyRoot {
			c.Mode, c.Hardened, c.CredentialLayout = "hardened", true, "systemd-loadcredential"
			return c, nil
		}
		c.Mode, c.CredentialLayout, c.Guidance = "ordinary-fallback", "ordinary-user", "hardened containment unavailable; ordinary Shipmates operation retained"
	case PlatformWSLLimited:
		c.Mode, c.CredentialLayout, c.Guidance = "ordinary-fallback", "ordinary-user", "limited WSL: administrator must provision the fixed Linux assets; no Windows mutation"
	case PlatformContainerPlain:
		c.Mode, c.CredentialLayout, c.Guidance = "container-fallback", "container-runtime", "non-systemd container: use the fixed runtime layout; no init/service manager assumed"
	default:
		return Composition{}, errors.New("platform_unknown")
	}
	return c, nil
}

// ComposeProfile is intentionally a plan only. It validates the one optional
// profile and records its authority scope without opening stores or secrets.
func ComposeProfile(c Composition, profile string) (Composition, error) {
	if profile == "" {
		return c, nil
	}
	if profile != "ubuntu-rojo-localhost" {
		return Composition{}, errors.New("profile_unknown")
	}
	if !c.Hardened {
		return Composition{}, errors.New("profile_requires_hardened_host")
	}
	c.Profile = profile
	c.Guidance = strings.TrimSpace("profile plan only; existing typed authority/provisioner boundaries remain authoritative")
	return c, nil
}
