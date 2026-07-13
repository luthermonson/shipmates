package fleetobserve

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type Clock interface{ Now() time.Time }
type Config struct {
	FleetID, FleetEpoch                                                                                                                      string
	MaxShips, MaxPersonas, MaxSnapshotBytes, MaxEventBytes, PerShipIngress, ReplayCapacity, MaxSubscribers, MaxPageSize, MaxTerminalMetadata int
	LeaseDuration, StaleRetention                                                                                                            time.Duration
	Clock                                                                                                                                    Clock
}
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type ship struct {
	generation           uint64
	status               ShipStatusV1
	ready                bool
	deadline, staleUntil time.Time
	ingress              []ObservationEventV1
	ingressLost          bool
}
type Projection struct {
	mu          sync.Mutex
	cfg         Config
	ships       map[string]*ship
	events      []FleetEventV1
	next        uint64
	subscribers int
}

func New(c Config) (*Projection, error) {
	if !safeID(c.FleetID) || !safeID(c.FleetEpoch) || c.MaxShips < 1 || c.MaxPersonas < 1 || c.MaxSnapshotBytes < 1 || c.MaxEventBytes < 1 || c.PerShipIngress < 1 || c.ReplayCapacity < 1 || c.MaxSubscribers < 1 || c.MaxPageSize < 1 || c.MaxTerminalMetadata < 1 || c.LeaseDuration <= 0 || c.StaleRetention < 0 {
		return nil, &Error{InvalidInput}
	}
	if c.Clock == nil {
		c.Clock = systemClock{}
	}
	return &Projection{cfg: c, ships: map[string]*ship{}, next: 1}, nil
}

func (p *Projection) Connect(shipID string, generation uint64) error {
	if !safeID(shipID) || generation == 0 {
		return &Error{InvalidInput}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.ships[shipID]
	if s == nil {
		if len(p.ships) >= p.cfg.MaxShips {
			return &Error{LimitExceeded}
		}
		s = &ship{status: ShipStatusV1{ShipID: shipID, Connectivity: Offline}}
		p.ships[shipID] = s
	}
	if generation <= s.generation {
		return &Error{StaleGeneration}
	}
	s.generation = generation
	s.ready = false
	s.ingress = nil
	s.ingressLost = false
	s.status.ConnectionGeneration = 0
	s.status.Connectivity = Offline
	s.status.Personas = nil
	return nil
}

func (p *Projection) InstallSnapshot(shipID string, generation uint64, in ShipStatusV1) error {
	if err := p.validateShipSnapshot(shipID, in); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := p.current(shipID, generation)
	if err != nil {
		return err
	}
	now := p.cfg.Clock.Now().UTC()
	in = cloneShip(in)
	in.ShipID = shipID
	in.Connectivity = Online
	in.ConnectionGeneration = generation
	in.ConnectivityChangedAt = now
	in.LastObservedAt = &now
	s.status = in
	s.ready = true
	s.deadline = now.Add(p.cfg.LeaseDuration)
	s.staleUntil = time.Time{}
	return p.appendLocked(shipID, generation, ObservationEventV1{Kind: ShipOnline, Data: EventDataV1{Generation: generation}})
}

// InstallResnapshot atomically replaces the state for an already-ready ship.
// A closed projection-warning event advances the Fleet cursor so readers can
// observe that their prior snapshot is no longer current without synthesizing
// a sequence of lifecycle events that may never have crossed the tunnel.
func (p *Projection) InstallResnapshot(shipID string, generation uint64, in ShipStatusV1) error {
	if err := p.validateShipSnapshot(shipID, in); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := p.current(shipID, generation)
	if err != nil {
		return err
	}
	if !s.ready {
		return &Error{NotReady}
	}
	now := p.cfg.Clock.Now().UTC()
	in = cloneShip(in)
	in.ShipID = shipID
	in.Connectivity = Online
	in.ConnectionGeneration = generation
	in.ConnectivityChangedAt = s.status.ConnectivityChangedAt
	in.LastObservedAt = &now
	s.status = in
	s.deadline = now.Add(p.cfg.LeaseDuration)
	s.ingress = nil
	s.ingressLost = false
	return p.appendLocked(shipID, generation, ObservationEventV1{Kind: ProjectionWarning, Data: EventDataV1{WarningCode: string(ShipResync)}})
}

// Enqueue is bounded and never waits for a consumer. Overflow is remembered as
// an explicit ingress gap until the next full ship snapshot.
func (p *Projection) Enqueue(shipID string, generation uint64, e ObservationEventV1) error {
	if err := p.validateEvent(e); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := p.current(shipID, generation)
	if err != nil {
		return err
	}
	if !s.ready {
		return &Error{NotReady}
	}
	if len(s.ingress) >= p.cfg.PerShipIngress {
		s.ingressLost = true
		return &Error{IngressFull}
	}
	s.ingress = append(s.ingress, cloneObservation(e))
	return nil
}
func (p *Projection) Drain(shipID string, generation uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := p.current(shipID, generation)
	if err != nil {
		return err
	}
	if s.ingressLost {
		return &Error{IngressFull}
	}
	for len(s.ingress) > 0 {
		e := s.ingress[0]
		s.ingress = s.ingress[1:]
		if err = p.appendLocked(shipID, generation, e); err != nil {
			return err
		}
	}
	return nil
}
func (p *Projection) Heartbeat(shipID string, g uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, e := p.current(shipID, g)
	if e != nil {
		return e
	}
	if !s.ready {
		return &Error{NotReady}
	}
	now := p.cfg.Clock.Now().UTC()
	s.deadline = now.Add(p.cfg.LeaseDuration)
	s.status.LastObservedAt = &now
	return nil
}
func (p *Projection) Disconnect(shipID string, g uint64, clean bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, e := p.current(shipID, g)
	if e != nil {
		return e
	}
	if !s.ready {
		return &Error{NotReady}
	}
	now := p.cfg.Clock.Now().UTC()
	if clean {
		s.status.Connectivity = Offline
		s.status.ConnectivityChangedAt = now
		return p.appendLocked(shipID, g, ObservationEventV1{Kind: ShipOffline, Data: EventDataV1{ReasonCode: "clean_close"}})
	}
	s.status.Connectivity = Stale
	s.status.ConnectivityChangedAt = now
	s.staleUntil = now.Add(p.cfg.StaleRetention)
	return p.appendLocked(shipID, g, ObservationEventV1{Kind: ShipStale, Data: EventDataV1{ReasonCode: "transport_loss"}})
}
func (p *Projection) Expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Clock.Now().UTC()
	for id, s := range p.ships {
		if !s.ready {
			continue
		}
		if s.status.Connectivity == Online && !now.Before(s.deadline) {
			s.status.Connectivity = Stale
			s.status.ConnectivityChangedAt = now
			s.staleUntil = now.Add(p.cfg.StaleRetention)
			_ = p.appendLocked(id, s.generation, ObservationEventV1{Kind: ShipStale, Data: EventDataV1{ReasonCode: "lease_expired"}})
		}
		if s.status.Connectivity == Stale && !now.Before(s.staleUntil) {
			s.status.Connectivity = Offline
			s.status.ConnectivityChangedAt = now
			_ = p.appendLocked(id, s.generation, ObservationEventV1{Kind: ShipOffline, Data: EventDataV1{ReasonCode: "stale_expired"}})
		}
	}
}

func (p *Projection) Snapshot() FleetSnapshotV1 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

// MaxPageSize exposes the configured public read bound without exposing or
// permitting mutation of projection configuration.
func (p *Projection) MaxPageSize() int { return p.cfg.MaxPageSize }
func (p *Projection) Read(epoch string, after *uint64, limit int) (ReadResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit < 1 || limit > p.cfg.MaxPageSize {
		return ReadResult{}, &Error{InvalidInput}
	}
	oldest := p.oldest()
	r := ReadResult{SchemaVersion: SchemaVersion, OldestAvailable: oldest, NextCursor: p.next}
	var reason GapReason
	if after == nil {
		snap := p.snapshotLocked()
		r.Snapshot = &snap
		return r, nil
	}
	if epoch != p.cfg.FleetEpoch {
		reason = EpochChanged
	} else if *after+1 < oldest {
		reason = HistoryDropped
	} else if *after >= p.next {
		reason = CursorAhead
	}
	if reason != "" {
		a := *after
		r.Gap = &GapV1{SchemaVersion: 1, Reason: reason, RequestedEpoch: epoch, RequestedCursor: &a, CurrentEpoch: p.cfg.FleetEpoch, OldestAvailable: oldest, NextCursor: p.next}
		snap := p.snapshotLocked()
		r.Snapshot = &snap
		return r, nil
	}
	for _, e := range p.events {
		if e.Cursor > *after && len(r.Events) < limit {
			r.Events = append(r.Events, cloneEvent(e))
		}
	}
	return r, nil
}
func (p *Projection) Resync(shipID string, g uint64) (ReadResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, e := p.current(shipID, g)
	if e != nil {
		return ReadResult{}, e
	}
	reason := ShipResync
	if s.ingressLost {
		reason = IngressDropped
	}
	s.ingress = nil
	s.ingressLost = false
	snap := p.snapshotLocked()
	return ReadResult{Gap: &GapV1{SchemaVersion: 1, Reason: reason, CurrentEpoch: p.cfg.FleetEpoch, OldestAvailable: p.oldest(), NextCursor: p.next}, Snapshot: &snap, OldestAvailable: p.oldest(), NextCursor: p.next}, nil
}

func (p *Projection) current(id string, g uint64) (*ship, error) {
	s := p.ships[id]
	if s == nil || g == 0 || g != s.generation {
		return nil, &Error{StaleGeneration}
	}
	return s, nil
}
func (p *Projection) appendLocked(id string, g uint64, o ObservationEventV1) error {
	if p.next == math.MaxUint64 {
		return &Error{CursorExhausted}
	}
	now := p.cfg.Clock.Now().UTC()
	e := FleetEventV1{SchemaVersion: 1, FleetID: p.cfg.FleetID, FleetEpoch: p.cfg.FleetEpoch, Cursor: p.next, ObservedAt: now, ShipID: id, Persona: o.Persona, SessionInstance: o.SessionInstance, TurnInstance: o.TurnInstance, Kind: o.Kind, Data: o.Data}
	p.next++
	p.events = append(p.events, e)
	if len(p.events) > p.cfg.ReplayCapacity {
		p.events = append([]FleetEventV1(nil), p.events[len(p.events)-p.cfg.ReplayCapacity:]...)
	}
	s := p.ships[id]
	s.status.LastObservedAt = &now
	return nil
}
func (p *Projection) oldest() uint64 {
	if len(p.events) == 0 {
		return p.next
	}
	return p.events[0].Cursor
}
func (p *Projection) snapshotLocked() FleetSnapshotV1 {
	out := FleetSnapshotV1{SchemaVersion: 1, Capabilities: append([]Capability(nil), CapabilitiesV1[:]...), FleetID: p.cfg.FleetID, FleetEpoch: p.cfg.FleetEpoch, SnapshotCursor: p.next - 1, GeneratedAt: p.cfg.Clock.Now().UTC()}
	ids := make([]string, 0, len(p.ships))
	for id, s := range p.ships {
		if s.ready {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out.Ships = append(out.Ships, cloneShip(p.ships[id].status))
	}
	return out
}

func (p *Projection) validateShipSnapshot(id string, s ShipStatusV1) error {
	if !safeID(id) || (s.ShipID != "" && s.ShipID != id) || len(s.ShipLabel) > 256 || !safeText(s.ShipLabel) || len(s.Personas) > p.cfg.MaxPersonas {
		return &Error{InvalidInput}
	}
	seen := map[string]bool{}
	for _, x := range s.Personas {
		if !safeID(x.Persona) || seen[x.Persona] || !validSession(x.Session) || !validTurn(x.Turn) || !validActivity(x.Activity) || !safeOptionalID(x.SessionInstance) || !safeOptionalID(x.TurnInstance) {
			return &Error{InvalidInput}
		}
		seen[x.Persona] = true
		if x.Summary != nil && !validSummary(*x.Summary) {
			return &Error{InvalidInput}
		}
	}
	b, _ := json.Marshal(s)
	if len(b) > p.cfg.MaxSnapshotBytes {
		return &Error{LimitExceeded}
	}
	return nil
}
func (p *Projection) validateEvent(e ObservationEventV1) error {
	if !safeOptionalID(e.Persona) || !safeOptionalID(e.SessionInstance) || !safeOptionalID(e.TurnInstance) || !validEventKind(e.Kind) {
		return &Error{InvalidInput}
	}
	d := e.Data
	if !safeText(d.ReasonCode) || !safeText(d.Label) || !safeText(d.Text) || !safeText(d.RequestClass) || !safeText(d.WarningCode) || len(d.Text) > 4096 || len(d.Label) > 128 || len(d.ReasonCode) > 64 || len(d.RequestClass) > 64 || len(d.WarningCode) > 64 || !validData(e.Kind, d) {
		return &Error{InvalidInput}
	}
	b, _ := json.Marshal(e)
	if len(b) > p.cfg.MaxEventBytes {
		return &Error{LimitExceeded}
	}
	return nil
}

func validData(k EventKind, d EventDataV1) bool {
	base := d
	switch k {
	case ShipOnline:
		return d.Generation > 0 && (base == EventDataV1{Generation: d.Generation})
	case ShipStale, ShipOffline:
		return d.ReasonCode != "" && (base == EventDataV1{ReasonCode: d.ReasonCode})
	case ShipSnapshot:
		return base == (EventDataV1{Count: d.Count, Truncated: d.Truncated})
	case PersonaInstalled, PersonaRemoved:
		return base == (EventDataV1{})
	case SessionStateEvent:
		return validSession(d.Session) && base == (EventDataV1{Session: d.Session})
	case TurnStateEvent:
		return (d.Turn == "active" || d.Turn == "completed" || d.Turn == "interrupted" || d.Turn == "failed" || d.Turn == "unknown") && base == (EventDataV1{Turn: d.Turn})
	case ActivityEvent:
		return validActivity(d.Activity) && d.Label != "" && base == (EventDataV1{Activity: d.Activity, Label: d.Label})
	case AgentMessage:
		return base == (EventDataV1{Text: d.Text, Partial: d.Partial, Truncated: d.Truncated})
	case ApprovalWaiting:
		return d.Waiting != nil && base == (EventDataV1{Waiting: d.Waiting})
	case RequestRefused:
		return d.RequestClass != "" && d.ReasonCode != "" && base == (EventDataV1{RequestClass: d.RequestClass, ReasonCode: d.ReasonCode})
	case ProjectionWarning:
		return d.WarningCode != "" && base == (EventDataV1{WarningCode: d.WarningCode})
	}
	return false
}
func safeID(s string) bool {
	return len(s) > 0 && len(s) <= 128 && safeText(s) && !strings.ContainsAny(s, "/\\")
}
func safeOptionalID(s string) bool { return s == "" || safeID(s) }
func safeText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u202a' || r == '\u202b' || r == '\u202c' || r == '\u202d' || r == '\u202e' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069' {
			return false
		}
	}
	return true
}
func validSession(v SessionState) bool {
	switch v {
	case SessionAbsent, SessionStarting, SessionIdle, SessionWorking, SessionSteering, SessionInterrupting, SessionStopped, SessionFailed, SessionProjectionUnavailable:
		return true
	}
	return false
}
func validTurn(v TurnState) bool {
	switch v {
	case TurnNone, TurnActive, TurnTerminalUnknown:
		return true
	}
	return false
}
func validActivity(v Activity) bool {
	switch v {
	case ActivityIdle, ActivityCommand, ActivityFileChange, ActivityWebSearch, ActivityConnectedTool, ActivityOther:
		return true
	}
	return false
}
func validEventKind(v EventKind) bool {
	switch v {
	case ShipOnline, ShipStale, ShipOffline, ShipSnapshot, PersonaInstalled, PersonaRemoved, SessionStateEvent, TurnStateEvent, ActivityEvent, AgentMessage, ApprovalWaiting, RequestRefused, ProjectionWarning:
		return true
	}
	return false
}
func validSummary(s SafeSummaryV1) bool {
	return (s.Code == "projection_unavailable" && s.Text == "status unavailable") || (s.Code == "activity_truncated" && s.Text == "activity truncated")
}
func cloneShip(s ShipStatusV1) ShipStatusV1 {
	s.Personas = append([]PersonaStatusV1(nil), s.Personas...)
	for i := range s.Personas {
		if s.Personas[i].Summary != nil {
			x := *s.Personas[i].Summary
			s.Personas[i].Summary = &x
		}
	}
	if s.LastObservedAt != nil {
		x := *s.LastObservedAt
		s.LastObservedAt = &x
	}
	return s
}
func cloneObservation(e ObservationEventV1) ObservationEventV1 { return e }
func cloneEvent(e FleetEventV1) FleetEventV1                   { return e }
