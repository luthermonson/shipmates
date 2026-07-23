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

func TestResolve_ContainmentFromProject(t *testing.T) {
	project := File{
		Runtime: "claude",
		Containment: Containment{
			Mode:              "watchdog",
			MemoryLimitMB:     8192,
			CPULimitSeconds:   3600,
			PollIntervalMS:    250,
			GracefulTimeoutMS: 5000,
		},
	}
	got, err := Resolve("", project, File{})
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

func TestResolve_ContainmentProjectBeatsUser(t *testing.T) {
	project := File{Containment: Containment{Mode: "none"}}
	user := File{Containment: Containment{Mode: "cgroup", MemoryLimitMB: 4096}}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "none" {
		t.Errorf("project should win over user: %+v", got.Containment)
	}
	// Project's zero MemoryLimitMB should NOT be overridden by user's —
	// project ownership means project settings verbatim.
	if got.Containment.MemoryLimitMB != 0 {
		t.Errorf("expected 0 (project's value), got %d", got.Containment.MemoryLimitMB)
	}
}
