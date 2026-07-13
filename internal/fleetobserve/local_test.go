package fleetobserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStateAdapterAssignsCanonicalInstalledIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".codex", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"security.toml", "backend.toml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("developer_instructions = \"trusted\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := OpenLocalStateAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Snapshot("shp_0123456789abcdef", []LocalPersonaState{{Session: SessionIdle, Turn: TurnNone, Activity: ActivityIdle}, {Session: SessionWorking, Turn: TurnActive, Activity: ActivityCommand}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Personas) != 2 || s.Personas[0].Persona != "backend" || s.Personas[1].Persona != "security" {
		t.Fatalf("canonical identities = %#v", s.Personas)
	}
	canary := "persona_from_tunnel_controller_policy_/tmp/secret"
	ev, err := a.Event(LocalEvent{PersonaIndex: 0, Kind: ActivityEvent, Data: EventDataV1{Activity: ActivityCommand, Label: canary, Text: canary, ReasonCode: canary}})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Persona != "backend" || strings.Contains(ev.Data.Label+ev.Data.Text+ev.Data.ReasonCode, canary) {
		t.Fatalf("identifier canary survived: %#v", ev)
	}
}

func TestLocalStateAdapterRejectsUnsafeInstalledIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".codex", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "backend.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocalStateAdapter(root); err == nil {
		t.Fatal("accepted symlinked project identity")
	}
}
