package voyage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func acceptanceFixture() (*State, *Plan) {
	p := &Plan{Version: 1, Title: "acceptance", Objective: "objective", AcceptanceGateTask: "gate", Tasks: []Task{{ID: "work", Persona: "backend", Summary: "work", Prompt: "work"}, {ID: "gate", Persona: "backend", Summary: "gate", Prompt: "gate", DependsOn: []string{"work"}}}}
	return NewState(p, "plan-hash"), p
}

func TestAcceptanceOutputIsClosedGateOnlyAndIdempotent(t *testing.T) {
	state, plan := acceptanceFixture()
	marker := AcceptanceMarker + ` {"verdict":"pass","evidence":["host:check"]}`
	if err := ApplyAcceptanceOutput(state, plan, "work", marker, time.Unix(10, 0)); err == nil {
		t.Fatal("non-gate marker accepted")
	}
	if state.Acceptance != nil {
		t.Fatal("unauthorized marker mutated state")
	}
	if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, marker, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	if state.Acceptance == nil || state.Acceptance.Status != AcceptancePass {
		t.Fatalf("acceptance = %#v", state.Acceptance)
	}
	if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, marker, time.Unix(20, 0)); err != nil {
		t.Fatal("identical replay: ", err)
	}
	conflict := AcceptanceMarker + ` {"verdict":"no_go","evidence":["host:check"]}`
	if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, conflict, time.Unix(20, 0)); err == nil {
		t.Fatal("conflicting verdict accepted")
	}
}

func TestAcceptanceOutputRejectsMalformedDuplicateAndUnsupported(t *testing.T) {
	for _, output := range []string{
		AcceptanceMarker + ` {"verdict":"pass","verdict":"no_go","evidence":["x"]}`,
		AcceptanceMarker + ` {"verdict":"pass","evidence":[]}`,
		AcceptanceMarker + ` {"verdict":"unset","evidence":["x"]}`,
		AcceptanceMarker + ` {"verdict":"pass","evidence":["x"]} trailing`,
	} {
		state, plan := acceptanceFixture()
		if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, output, time.Unix(10, 0)); err == nil {
			t.Fatalf("accepted malformed output %q", output)
		}
		if state.Acceptance != nil {
			t.Fatalf("malformed output mutated state: %q", output)
		}
	}
}

func TestAcceptanceVerdictRoundTripsAndLegacyRemainsUnset(t *testing.T) {
	state, plan := acceptanceFixture()
	if state.Acceptance != nil {
		t.Fatal("new state unexpectedly has verdict")
	}
	if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, AcceptanceMarker+` {"verdict":"no_go","evidence":["fixture:m3-no-go"]}`, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := state.Acceptance.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(AcceptanceMarker), " ") {
		t.Fatal("marker unexpectedly contains whitespace")
	}
}

func TestAcceptanceLegacyStateCannotCarryVerdictAndAuthorizedGateMigrates(t *testing.T) {
	state, plan := acceptanceFixture()
	state.Version = 1
	state.GlobalFingerprint = ""
	state.Acceptance = &AcceptanceVerdict{Status: AcceptancePass, RecordedAt: time.Unix(10, 0).UTC(), EvidenceRefs: []string{"forged:legacy"}}
	path := filepath.Join(t.TempDir(), "legacy.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path, plan, state.PlanHash); err == nil {
		t.Fatal("legacy state carrying acceptance verdict was accepted")
	}

	state, plan = acceptanceFixture()
	state.Version = 1
	state.GlobalFingerprint = ""
	if err := ApplyAcceptanceOutput(state, plan, plan.AcceptanceGateTask, AcceptanceMarker+` {"verdict":"no_go","evidence":["report:m3"]}`, time.Unix(20, 0)); err != nil {
		t.Fatal(err)
	}
	if state.Version != StateVersion || len(state.GlobalFingerprint) != 64 || state.Acceptance == nil || state.Acceptance.Status != AcceptanceNoGo {
		t.Fatalf("authorized legacy migration = %#v", state)
	}
}

func TestPersistAcceptanceOutputPreservesTaskEvidence(t *testing.T) {
	state, plan := acceptanceFixture()
	state.PlanHash = strings.Repeat("a", 64)
	entry := state.Tasks["gate"]
	entry.Status = Completed
	entry.EvidenceRefs = []string{"task:evidence"}
	state.Tasks["gate"] = entry
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	if err := PersistAcceptanceOutput(path, state, plan, plan.AcceptanceGateTask, AcceptanceMarker+` {"verdict":"pass","evidence":["verdict:evidence"]}`, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path, plan, state.PlanHash)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Acceptance == nil || loaded.Acceptance.Status != AcceptancePass {
		t.Fatalf("loaded verdict = %#v", loaded.Acceptance)
	}
	if !sameStrings(loaded.Tasks["gate"].EvidenceRefs, []string{"task:evidence"}) || loaded.Tasks["gate"].Status != Completed {
		t.Fatalf("task evidence changed: %#v", loaded.Tasks["gate"])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
