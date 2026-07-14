package commands

import (
	"os"
	"strings"
	"testing"
)

func TestM7FleetGrammarContainsOnlyObserverCommands(t *testing.T) {
	c := Fleet()
	want := map[string]bool{"init": true, "enrollment": true, "credential": true, "serve-observer": true, "steer": true, "steer-targets": true, "interrupt": true, "interrupt-targets": true, "ships": true, "status": true, "events": true, "follow": true}
	if len(c.Commands) != len(want) {
		t.Fatalf("commands=%d", len(c.Commands))
	}
	for _, sub := range c.Commands {
		if !want[sub.Name] {
			t.Fatalf("action command exposed: %s", sub.Name)
		}
		delete(want, sub.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing observer commands: %v", want)
	}
}

func TestM7FollowAdvancesThroughEmptyScopedPages(t *testing.T) {
	b, err := os.ReadFile("fleetobserve.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "after, e = fleetobserver.AdvanceCursor(after, r)") {
		t.Fatal("follow does not use the authoritative response cursor")
	}
}
