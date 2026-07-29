package config

import "testing"

func TestResolve_ContainmentDefault(t *testing.T) {
	got, err := Resolve("", File{}, File{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "watchdog" {
		t.Errorf("default mode=%q want watchdog", got.Containment.Mode)
	}
	if got.Containment.MemoryLimitMB != 0 {
		t.Errorf("default memory should be uncapped (0), got %d", got.Containment.MemoryLimitMB)
	}
}

func TestResolve_ContainmentFromUser(t *testing.T) {
	user := File{
		Containment: Containment{
			Mode:              "watchdog",
			MemoryLimitMB:     8192,
			CPULimitSeconds:   3600,
			PollIntervalMS:    250,
			GracefulTimeoutMS: 5000,
		},
	}
	got, err := Resolve("", File{}, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.MemoryLimitMB != 8192 || got.Containment.PollIntervalMS != 250 {
		t.Errorf("containment fields not propagated: %+v", got.Containment)
	}
}

func TestResolve_ContainmentUserFallback(t *testing.T) {
	user := File{Containment: Containment{Mode: "cgroup", MemoryLimitMB: 4096}}
	got, err := Resolve("", File{}, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "cgroup" {
		t.Errorf("user containment lost: %+v", got.Containment)
	}
	if got.Containment.MemoryLimitMB != 4096 {
		t.Errorf("user mem lost: %d", got.Containment.MemoryLimitMB)
	}
}

// Containment is the operator's call, not the checkout's: a cloned
// repository must not be able to weaken (or otherwise redefine) the bounds
// applied to the turns it runs.
func TestResolve_UserContainmentBeatsProject(t *testing.T) {
	project := File{Containment: Containment{Mode: "none"}}
	user := File{Containment: Containment{Mode: "cgroup", MemoryLimitMB: 4096}}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "cgroup" {
		t.Errorf("user should win over project: %+v", got.Containment)
	}
	if got.Containment.MemoryLimitMB != 4096 {
		t.Errorf("expected the user's 4096, got %d", got.Containment.MemoryLimitMB)
	}
}
