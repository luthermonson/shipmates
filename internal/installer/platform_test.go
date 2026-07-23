package installer

import "testing"

func TestCapabilityComposition(t *testing.T) {
	base := CapabilitySnapshot{Platform: PlatformBareLinux, Systemd: true, CgroupV2: true, DelegatedCgroup: true, Pidfd: true, TrustedLauncher: true}
	c, err := DetectFromSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Hardened || c.Mode != "hardened" || c.Qualification {
		t.Fatalf("composition=%+v", c)
	}
	if _, err := ComposeProfile(c, "ubuntu-rojo-localhost"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Platform{PlatformWSLLimited, PlatformContainerPlain} {
		fallback, err := DetectFromSnapshot(CapabilitySnapshot{Platform: p})
		if err != nil {
			t.Fatal(err)
		}
		if fallback.Hardened || fallback.Mode == "hardened" {
			t.Fatalf("unsafe platform hardened: %+v", fallback)
		}
		if _, err := ComposeProfile(fallback, "ubuntu-rojo-localhost"); err == nil {
			t.Fatal("profile enabled without containment")
		}
	}
}

func TestCapabilityMissingTrustedLauncherFallsBack(t *testing.T) {
	c, err := DetectFromSnapshot(CapabilitySnapshot{Platform: PlatformBareLinux, Systemd: true, CgroupV2: true, DelegatedCgroup: true, Pidfd: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Hardened || c.CredentialLayout != "ordinary-user" {
		t.Fatalf("composition=%+v", c)
	}
}
