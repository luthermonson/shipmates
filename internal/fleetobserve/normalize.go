package fleetobserve

// NormalizeShipSnapshot is the M3-M6 trust-boundary adapter. It reconstructs
// observer state from the small typed local-state allowlist and intentionally
// discards all ship-authored projected metadata (labels, timestamps, instance
// IDs, summaries, connectivity and generation).
func NormalizeShipSnapshot(shipID string, in ShipStatusV1) (ShipStatusV1, error) {
	if !safeID(shipID) || (in.ShipID != "" && in.ShipID != shipID) {
		return ShipStatusV1{}, &Error{InvalidInput}
	}
	out := ShipStatusV1{ShipID: shipID}
	seen := map[string]bool{}
	for _, p := range in.Personas {
		if !safeID(p.Persona) || seen[p.Persona] || !validSession(p.Session) || !validTurn(p.Turn) || !validActivity(p.Activity) {
			return ShipStatusV1{}, &Error{InvalidInput}
		}
		seen[p.Persona] = true
		out.Personas = append(out.Personas, PersonaStatusV1{Persona: p.Persona, Installed: p.Installed, Session: p.Session, Turn: p.Turn, Activity: p.Activity, ApprovalWaiting: p.ApprovalWaiting})
	}
	return out, nil
}

// NormalizeObservationEvent reconstructs a public event using fixed Fleet-owned
// labels and reason codes. Free text, backend identifiers and authority tuples
// are never copied. Unsupported kinds are dropped fail-closed.
func NormalizeObservationEvent(in ObservationEventV1) (ObservationEventV1, error) {
	if !safeOptionalID(in.Persona) {
		return ObservationEventV1{}, &Error{InvalidInput}
	}
	o := ObservationEventV1{Persona: in.Persona, Kind: in.Kind}
	switch in.Kind {
	case PersonaInstalled, PersonaRemoved:
	case SessionStateEvent:
		if !validSession(in.Data.Session) {
			return ObservationEventV1{}, &Error{InvalidInput}
		}
		o.Data.Session = in.Data.Session
	case TurnStateEvent:
		switch in.Data.Turn {
		case "active", "completed", "interrupted", "failed", "unknown":
			o.Data.Turn = in.Data.Turn
		default:
			return ObservationEventV1{}, &Error{InvalidInput}
		}
	case ActivityEvent:
		if !validActivity(in.Data.Activity) {
			return ObservationEventV1{}, &Error{InvalidInput}
		}
		o.Data.Activity = in.Data.Activity
		o.Data.Label = fixedActivityLabel(in.Data.Activity)
	case ApprovalWaiting:
		if in.Data.Waiting == nil {
			return ObservationEventV1{}, &Error{InvalidInput}
		}
		v := *in.Data.Waiting
		o.Data.Waiting = &v
	case RequestRefused:
		// M3-M6 expose only this normalized request class and fixed refusal set.
		if in.Data.RequestClass != "process.exec" {
			return ObservationEventV1{}, &Error{InvalidInput}
		}
		switch in.Data.ReasonCode {
		case "policy_deny", "approval_denied", "approval_expired", "authority_lost":
		default:
			return ObservationEventV1{}, &Error{InvalidInput}
		}
		o.Data.RequestClass = "process.exec"
		o.Data.ReasonCode = in.Data.ReasonCode
	case ProjectionWarning:
		switch in.Data.WarningCode {
		case "projection_unavailable", "activity_truncated", "event_dropped":
			o.Data.WarningCode = in.Data.WarningCode
		default:
			return ObservationEventV1{}, &Error{InvalidInput}
		}
	default:
		// Ship connectivity and snapshots are Fleet-owned. Agent content is not
		// an observer-safe local-state input.
		return ObservationEventV1{}, &Error{InvalidInput}
	}
	return o, nil
}

func fixedActivityLabel(a Activity) string {
	switch a {
	case ActivityIdle:
		return "idle"
	case ActivityCommand:
		return "command"
	case ActivityFileChange:
		return "file change"
	case ActivityWebSearch:
		return "web search"
	case ActivityConnectedTool:
		return "connected tool"
	default:
		return "other activity"
	}
}
