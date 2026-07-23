package recovery

// NewRequest constructs the exact bounded local recovery request used by the
// M1 assessment boundary. Callers supply only already-sanitized local facts;
// Commander envelopes and their opaque references have no parameter here.
func NewRequest(provenance Provenance, reason ReasonCode, attempt, tierCount uint8, evidence []EvidenceRef) (RequestV1, error) {
	req := RequestV1{SchemaVersion: SchemaVersion, Provenance: provenance, Reason: reason, Attempt: attempt, TierCount: tierCount, Evidence: append([]EvidenceRef(nil), evidence...)}
	if err := req.Validate(); err != nil {
		return RequestV1{}, err
	}
	return req, nil
}

// ValidateAdvisory is the narrow policy seam for local Sail validation. It
// never authorizes an action; callers still decide what to do with a valid
// advisory under the existing Sail state machine.
func ValidateAdvisory(req RequestV1, response ResponseV1) error {
	return response.ValidateFor(req)
}
