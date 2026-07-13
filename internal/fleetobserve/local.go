package fleetobserve

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
)

// PersonaReference returns the Fleet-visible opaque identity for one trusted
// local persona. The private name is used only to find its canonical slot and
// is never included in the returned reference.
func (a *LocalStateAdapter) PersonaReference(shipID, persona string) (string, error) {
	if a == nil || shipID == "" {
		return "", &Error{InvalidInput}
	}
	for slot, installed := range a.personas {
		if installed == persona {
			return OpaquePersonaReference(shipID, uint16(slot)), nil
		}
	}
	return "", &Error{InvalidInput}
}

// OpaquePersonaReference derives the stable Fleet identity for a trusted slot.
func OpaquePersonaReference(shipID string, slot uint16) string {
	h := sha256.New()
	h.Write([]byte("shipmates/fleet-persona-ref/v1\x00"))
	h.Write([]byte(shipID))
	h.Write([]byte{byte(slot >> 8), byte(slot)})
	return "prf_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:18])
}

// ValidPersonaReference reports whether v is the canonical Fleet-visible
// persona-reference encoding. Persona references are opaque 144-bit values;
// local persona names are never valid on remote Fleet request surfaces.
func ValidPersonaReference(v string) bool {
	const prefix = "prf_"
	if !strings.HasPrefix(v, prefix) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(v, prefix))
	return err == nil && len(b) == 18 && prefix+base64.RawURLEncoding.EncodeToString(b) == v
}

// StatesForActivePersonas maps server-owned active persona names back onto the
// adapter's private canonical slots. Unknown identities fail closed.
func (a *LocalStateAdapter) StatesForActivePersonas(active []string) ([]LocalPersonaState, error) {
	if a == nil {
		return nil, &Error{InvalidInput}
	}
	set := make(map[string]bool, len(active))
	for _, persona := range active {
		if set[persona] {
			continue
		}
		found := false
		for _, installed := range a.personas {
			if installed == persona {
				found = true
				break
			}
		}
		if !found {
			return nil, &Error{InvalidInput}
		}
		set[persona] = true
	}
	out := make([]LocalPersonaState, len(a.personas))
	for i, persona := range a.personas {
		out[i] = LocalPersonaState{Session: SessionAbsent, Turn: TurnNone, Activity: ActivityIdle}
		if set[persona] {
			out[i] = LocalPersonaState{Session: SessionWorking, Turn: TurnActive, Activity: ActivityOther}
		}
	}
	return out, nil
}

// LocalPersonaState is the typed M3-M6 observer projection input. It contains
// state only: identities and free-form summaries cannot be supplied here.
type LocalPersonaState struct {
	Session         SessionState
	Turn            TurnState
	Activity        Activity
	ApprovalWaiting bool
}

type LocalEvent struct {
	PersonaIndex int
	Kind         EventKind
	Data         EventDataV1
}

// LocalStateAdapter binds state slots to the canonical installed Codex persona
// inventory. The inventory is private so callers cannot manufacture a trusted
// persona identity after construction.
type LocalStateAdapter struct{ personas []string }

// SlotCount is the bounded installed inventory size. The corresponding names
// deliberately remain private and are never returned to tunnel code.
func (a *LocalStateAdapter) SlotCount() int {
	if a == nil {
		return 0
	}
	return len(a.personas)
}

func (a *LocalStateAdapter) ValidateStates(states []LocalPersonaState) error {
	if a == nil || len(states) != len(a.personas) {
		return &Error{InvalidInput}
	}
	for _, state := range states {
		if !validSession(state.Session) || !validTurn(state.Turn) || !validActivity(state.Activity) {
			return &Error{InvalidInput}
		}
	}
	return nil
}

func (a *LocalStateAdapter) ValidateEvent(in LocalEvent) error {
	if a == nil || in.PersonaIndex < 0 || in.PersonaIndex >= len(a.personas) {
		return &Error{InvalidInput}
	}
	// Reuse the closed event validator with a fixed non-publishable sentinel.
	_, err := NormalizeObservationEvent(ObservationEventV1{Persona: "local-slot", Kind: in.Kind, Data: in.Data})
	return err
}

func OpenLocalStateAdapter(projectRoot string) (*LocalStateAdapter, error) {
	entries, err := project.CanonicalPersonaInventory(projectRoot)
	if err != nil {
		return nil, err
	}
	personas := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !safeID(entry.Name) {
			return nil, &Error{InvalidInput}
		}
		personas = append(personas, entry.Name)
	}
	if len(personas) == 0 {
		return nil, errors.New("no_installed_personas")
	}
	return &LocalStateAdapter{personas: personas}, nil
}

// ValidateInventory proves that the exact sorted name-to-slot binding captured
// at construction still describes the canonical inventory.
func (a *LocalStateAdapter) ValidateInventory(projectRoot string) error {
	if a == nil {
		return &Error{InvalidInput}
	}
	entries, err := project.CanonicalPersonaInventory(projectRoot)
	if err != nil {
		return err
	}
	if len(entries) != len(a.personas) {
		return errors.New("local_persona_inventory_changed")
	}
	for i := range entries {
		if entries[i].Name != a.personas[i] {
			return errors.New("local_persona_inventory_changed")
		}
	}
	return nil
}

func (a *LocalStateAdapter) Snapshot(shipID string, states []LocalPersonaState) (ShipStatusV1, error) {
	if a == nil || !safeID(shipID) || len(states) != len(a.personas) {
		return ShipStatusV1{}, &Error{InvalidInput}
	}
	out := ShipStatusV1{ShipID: shipID, Personas: make([]PersonaStatusV1, len(states))}
	for i, state := range states {
		if !validSession(state.Session) || !validTurn(state.Turn) || !validActivity(state.Activity) {
			return ShipStatusV1{}, &Error{InvalidInput}
		}
		out.Personas[i] = PersonaStatusV1{Persona: a.personas[i], Installed: true, Session: state.Session, Turn: state.Turn, Activity: state.Activity, ApprovalWaiting: state.ApprovalWaiting}
	}
	return out, nil
}

func (a *LocalStateAdapter) Event(in LocalEvent) (ObservationEventV1, error) {
	if a == nil || in.PersonaIndex < 0 || in.PersonaIndex >= len(a.personas) {
		return ObservationEventV1{}, &Error{InvalidInput}
	}
	return NormalizeObservationEvent(ObservationEventV1{Persona: a.personas[in.PersonaIndex], Kind: in.Kind, Data: in.Data})
}
