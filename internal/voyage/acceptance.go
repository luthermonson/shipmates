package voyage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const AcceptanceMarker = "SHIPMATES_ACCEPTANCE_V1:"

type acceptanceOutput struct {
	Verdict  string   `json:"verdict"`
	Evidence []string `json:"evidence"`
}

// ApplyAcceptanceOutput validates the exact closed final marker and applies
// it to the separate voyage verdict. No marker means no verdict mutation.
// Marker text from any non-gate task, malformed JSON, duplicate fields, or a
// conflicting second verdict fails closed.
func ApplyAcceptanceOutput(state *State, plan *Plan, taskID, output string, now time.Time) error {
	if state == nil || plan == nil || taskID == "" {
		return errors.New("invalid acceptance context")
	}
	trimmed := strings.TrimSpace(output)
	if !strings.Contains(trimmed, AcceptanceMarker) {
		return nil
	}
	if plan.AcceptanceGateTask == "" || taskID != plan.AcceptanceGateTask || !strings.HasPrefix(trimmed, AcceptanceMarker+" ") {
		return errors.New("acceptance marker is unauthorized")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(trimmed, AcceptanceMarker))
	if err := uniqueJSON(raw); err != nil {
		return errors.New("malformed acceptance marker")
	}
	var parsed acceptanceOutput
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parsed); err != nil {
		return errors.New("malformed acceptance marker")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("malformed acceptance marker")
	}
	status := AcceptanceStatus(parsed.Verdict)
	if status != AcceptancePass && status != AcceptanceNoGo {
		return errors.New("unsupported acceptance verdict")
	}
	planBinding := ""
	if len(state.PlanHash) == 64 {
		planBinding = state.PlanHash
	}
	verdict := &AcceptanceVerdict{Status: status, RecordedAt: now.UTC(), EvidenceRefs: append([]string(nil), parsed.Evidence...), PlanHash: planBinding, GlobalFingerprint: GlobalFingerprint(plan)}
	if err := verdict.Validate(); err != nil {
		return err
	}
	if state.Acceptance != nil && state.Acceptance.Status != AcceptanceUnset {
		if state.Acceptance.Status != verdict.Status || !sameStrings(state.Acceptance.EvidenceRefs, verdict.EvidenceRefs) {
			return errors.New("conflicting acceptance verdict")
		}
		return nil
	}
	// A v1 state intentionally has no acceptance semantics.  Upgrade only at
	// this authorized, validated boundary so legacy completed voyages remain
	// acceptance-unknown and cannot be made to appear passed by editing a v1
	// JSON record.
	if state.Version == 1 {
		state.Version = StateVersion
		state.GlobalFingerprint = GlobalFingerprint(plan)
	}
	state.Acceptance = verdict
	return nil
}

func PersistAcceptanceOutput(path string, state *State, plan *Plan, taskID, output string, now time.Time) error {
	before := state.Acceptance
	if err := ApplyAcceptanceOutput(state, plan, taskID, output, now); err != nil {
		return err
	}
	if before == state.Acceptance {
		return nil
	}
	return SaveState(path, state)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uniqueJSON(raw string) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := consumeJSON(dec); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func consumeJSON(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch d := tok.(type) {
	case json.Delim:
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, ok := tokString(dec.Token())
				if !ok || seen[key] {
					return errors.New("duplicate JSON field")
				}
				seen[key] = true
				if err := consumeJSON(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := consumeJSON(dec); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
	}
	return nil
}

func tokString(tok json.Token, err error) (string, bool) {
	if err != nil {
		return "", false
	}
	s, ok := tok.(string)
	return s, ok
}
