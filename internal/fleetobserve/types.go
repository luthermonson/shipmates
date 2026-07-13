// Package fleetobserve implements the pure, in-memory M7 observation read model.
// It contains no transport, authentication, persistence, or control surface.
package fleetobserve

import "time"

const SchemaVersion = 1

type Capability string

const (
	CapabilitySnapshotV1 Capability = "fleet.snapshot.v1"
	CapabilityEventsV1   Capability = "fleet.events.v1"
	CapabilityGapsV1     Capability = "fleet.gaps.v1"
)

var CapabilitiesV1 = [...]Capability{CapabilitySnapshotV1, CapabilityEventsV1, CapabilityGapsV1}

type Connectivity string

const (
	Online  Connectivity = "online"
	Stale   Connectivity = "stale"
	Offline Connectivity = "offline"
)

type SessionState string

const (
	SessionAbsent                SessionState = "absent"
	SessionStarting              SessionState = "starting"
	SessionIdle                  SessionState = "idle"
	SessionWorking               SessionState = "working"
	SessionSteering              SessionState = "steering"
	SessionInterrupting          SessionState = "interrupting"
	SessionStopped               SessionState = "stopped"
	SessionFailed                SessionState = "failed"
	SessionProjectionUnavailable SessionState = "projection_unavailable"
)

type TurnState string

const (
	TurnNone            TurnState = "none"
	TurnActive          TurnState = "active"
	TurnTerminalUnknown TurnState = "terminal_unknown"
)

type Activity string

const (
	ActivityIdle          Activity = "idle"
	ActivityCommand       Activity = "command"
	ActivityFileChange    Activity = "file_change"
	ActivityWebSearch     Activity = "web_search"
	ActivityConnectedTool Activity = "connected_tool"
	ActivityOther         Activity = "other"
)

type SafeSummaryV1 struct {
	Code      string `json:"code"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
type PersonaStatusV1 struct {
	Persona         string         `json:"persona"`
	Installed       bool           `json:"installed"`
	Session         SessionState   `json:"session"`
	Turn            TurnState      `json:"turn"`
	Activity        Activity       `json:"activity"`
	ApprovalWaiting bool           `json:"approval_waiting"`
	SessionInstance string         `json:"session_instance,omitempty"`
	TurnInstance    string         `json:"turn_instance,omitempty"`
	StatusChangedAt time.Time      `json:"status_changed_at"`
	Summary         *SafeSummaryV1 `json:"summary,omitempty"`
}
type ShipStatusV1 struct {
	ShipID                string            `json:"ship_id"`
	ShipLabel             string            `json:"ship_label,omitempty"`
	Connectivity          Connectivity      `json:"connectivity"`
	ConnectionGeneration  uint64            `json:"connection_generation,omitempty"`
	ConnectivityChangedAt time.Time         `json:"connectivity_changed_at"`
	LastObservedAt        *time.Time        `json:"last_observed_at,omitempty"`
	Personas              []PersonaStatusV1 `json:"personas"`
	Truncated             bool              `json:"truncated"`
	OmittedPersonaCount   uint              `json:"omitted_persona_count"`
}
type FleetSnapshotV1 struct {
	SchemaVersion    int            `json:"schema_version"`
	Capabilities     []Capability   `json:"capabilities"`
	FleetID          string         `json:"fleet_id"`
	FleetEpoch       string         `json:"fleet_epoch"`
	SnapshotCursor   uint64         `json:"snapshot_cursor"`
	GeneratedAt      time.Time      `json:"generated_at"`
	Ships            []ShipStatusV1 `json:"ships"`
	Truncated        bool           `json:"truncated"`
	OmittedShipCount uint           `json:"omitted_ship_count"`
}

type EventKind string

const (
	ShipOnline        EventKind = "ship.online"
	ShipStale         EventKind = "ship.stale"
	ShipOffline       EventKind = "ship.offline"
	ShipSnapshot      EventKind = "ship.snapshot"
	PersonaInstalled  EventKind = "persona.installed"
	PersonaRemoved    EventKind = "persona.removed"
	SessionStateEvent EventKind = "session.state"
	TurnStateEvent    EventKind = "turn.state"
	ActivityEvent     EventKind = "activity"
	AgentMessage      EventKind = "agent.message"
	ApprovalWaiting   EventKind = "approval.waiting"
	RequestRefused    EventKind = "request.refused"
	ProjectionWarning EventKind = "projection.warning"
)

// EventDataV1 is a closed allowlist. Fields irrelevant to Kind must be zero.
type EventDataV1 struct {
	Generation   uint64       `json:"generation,omitempty"`
	ReasonCode   string       `json:"reason_code,omitempty"`
	Count        uint         `json:"count,omitempty"`
	Truncated    bool         `json:"truncated,omitempty"`
	Session      SessionState `json:"session,omitempty"`
	Turn         string       `json:"turn,omitempty"`
	Activity     Activity     `json:"activity,omitempty"`
	Label        string       `json:"label,omitempty"`
	Text         string       `json:"text,omitempty"`
	Partial      bool         `json:"partial,omitempty"`
	Waiting      *bool        `json:"waiting,omitempty"`
	RequestClass string       `json:"request_class,omitempty"`
	WarningCode  string       `json:"warning_code,omitempty"`
}
type ObservationEventV1 struct {
	Persona         string
	SessionInstance string
	TurnInstance    string
	Kind            EventKind
	Data            EventDataV1
}
type FleetEventV1 struct {
	SchemaVersion   int         `json:"schema_version"`
	FleetID         string      `json:"fleet_id"`
	FleetEpoch      string      `json:"fleet_epoch"`
	Cursor          uint64      `json:"cursor"`
	ObservedAt      time.Time   `json:"observed_at"`
	ShipID          string      `json:"ship_id"`
	Persona         string      `json:"persona,omitempty"`
	SessionInstance string      `json:"session_instance,omitempty"`
	TurnInstance    string      `json:"turn_instance,omitempty"`
	Kind            EventKind   `json:"kind"`
	Data            EventDataV1 `json:"data"`
}

type GapReason string

const (
	EpochChanged   GapReason = "epoch_changed"
	HistoryDropped GapReason = "history_dropped"
	IngressDropped GapReason = "ingress_dropped"
	CursorAhead    GapReason = "cursor_ahead"
	ShipResync     GapReason = "ship_resync"
	ClientTooSlow  GapReason = "client_too_slow"
)

type GapV1 struct {
	SchemaVersion   int       `json:"schema_version"`
	Reason          GapReason `json:"reason"`
	RequestedEpoch  string    `json:"requested_epoch,omitempty"`
	RequestedCursor *uint64   `json:"requested_cursor,omitempty"`
	CurrentEpoch    string    `json:"current_epoch"`
	OldestAvailable uint64    `json:"oldest_available"`
	NextCursor      uint64    `json:"next_cursor"`
}
type ReadResult struct {
	SchemaVersion   int              `json:"schema_version"`
	Gap             *GapV1           `json:"gap,omitempty"`
	Snapshot        *FleetSnapshotV1 `json:"snapshot,omitempty"`
	Events          []FleetEventV1   `json:"events"`
	OldestAvailable uint64           `json:"oldest_available"`
	NextCursor      uint64           `json:"next_cursor"`
}

type Code string

const (
	InvalidInput    Code = "invalid_input"
	LimitExceeded   Code = "limit_exceeded"
	StaleGeneration Code = "stale_generation"
	NotReady        Code = "not_ready"
	IngressFull     Code = "ingress_full"
	CursorExhausted Code = "cursor_exhausted"
)

type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) }
