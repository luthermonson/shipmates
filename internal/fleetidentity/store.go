package fleetidentity

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const ShipStateSchema = 1
const shipStateFile = "identity.json"

// ShipState is the complete local M7 identity. It contains no project, label,
// persona, session, controller, policy, or approval material.
type ShipState struct {
	SchemaVersion        int    `json:"schema_version"`
	FleetID              string `json:"fleet_id"`
	ShipID               string `json:"ship_id"`
	FleetDestination     string `json:"fleet_destination"`
	FleetServiceIdentity string `json:"fleet_service_identity"`
	CredentialID         string `json:"ship_credential_id"`
	CredentialSecret     string `json:"ship_credential_secret"`
}

type StatePlan struct {
	Path   string
	Exists bool
}

func validateState(s ShipState) error {
	if s.SchemaVersion != ShipStateSchema || !opaqueID.MatchString(s.FleetID) || !opaqueID.MatchString(s.ShipID) || !opaqueID.MatchString(s.CredentialID) || len(s.CredentialSecret) < 32 || len(s.CredentialSecret) > 128 || len(s.FleetDestination) < 1 || len(s.FleetDestination) > 512 || len(s.FleetServiceIdentity) < 1 || len(s.FleetServiceIdentity) > 256 {
		return fail(InvalidInput)
	}
	return nil
}
func stateBytes(s ShipState) ([]byte, error) {
	if err := validateState(s); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fail(InvalidInput)
	}
	return append(b, '\n'), nil
}

// PlanShipState performs no writes and refuses ambiguous filesystem state.
func PlanShipState(dir string) (StatePlan, error) { return planShipState(filepath.Clean(dir)) }
func LoadShipState(dir string) (ShipState, error) {
	b, err := readShipState(filepath.Clean(dir))
	if err != nil {
		return ShipState{}, err
	}
	if len(b) > 4096 {
		return ShipState{}, fail(InvalidInput)
	}
	var s ShipState
	if json.Unmarshal(b, &s) != nil || validateState(s) != nil {
		return ShipState{}, fail(InvalidInput)
	}
	return s, nil
}

// StoreShipState creates the identity exactly once. Replacement is deliberately
// a distinct operation so startup and enrollment cannot silently overwrite it.
func StoreShipState(dir string, s ShipState) error {
	b, err := stateBytes(s)
	if err != nil {
		return err
	}
	return writeShipState(filepath.Clean(dir), b, false)
}
func ReplaceShipState(dir string, s ShipState) error {
	b, err := stateBytes(s)
	if err != nil {
		return err
	}
	return writeShipState(filepath.Clean(dir), b, true)
}

type CommitUncertainError struct{ Err error }

func (e *CommitUncertainError) Error() string { return "ship identity commit durability is uncertain" }
func (e *CommitUncertainError) Unwrap() error { return e.Err }
func shortWrite(n, want int) error {
	if n != want {
		return io.ErrShortWrite
	}
	return nil
}
func storageError() error { return fmt.Errorf("ship identity storage unavailable") }
