package fleetidentity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"time"
)

const authoritySchema = 1
const authorityFile = "fleet-authority.json"

type durableCredential struct {
	ID, Verifier string
	Revoked      bool
}
type durableRotation struct {
	Pending durableCredential
	Expires time.Time
	Proved  bool
}
type durableShip struct {
	ID         string
	Revoked    bool
	Current    durableCredential
	Rotation   *durableRotation
	Generation uint64
}
type durableArtifact struct {
	ID, Verifier string
	Expires      time.Time
	Used         bool
	TxnID        string
	Result       EnrollmentResult
}
type durableObserver struct {
	Credential      durableCredential
	Ships           []string
	Issued, Expires time.Time
}
type durableOperator struct {
	Credential                   durableCredential `json:"credential"`
	Subject, FleetID, Capability string
	Generation                   uint64
	Ships                        []string
	Issued, Expires, RevokeAt    time.Time
	PreviousID                   string
}
type durableCommander struct {
	Credential                durableCredential `json:"credential"`
	Subject, FleetID          string
	Generation                uint64
	Ships                     []string
	Issued, Expires, RevokeAt time.Time
	PreviousID                string
}
type durableAuthority struct {
	Schema     int                `json:"schema"`
	FleetID    string             `json:"fleet_id"`
	Artifacts  []durableArtifact  `json:"artifacts"`
	Ships      []durableShip      `json:"ships"`
	Observers  []durableObserver  `json:"observers"`
	Operators  []durableOperator  `json:"operators"`
	Commanders []durableCommander `json:"commanders,omitempty"`
	TxnOrder   []string           `json:"txn_order"`
}

func dc(c credential) durableCredential {
	return durableCredential{c.id, base64.RawURLEncoding.EncodeToString(c.verifier[:]), c.revoked}
}
func credentialFrom(c durableCredential) (credential, error) {
	b, e := base64.RawURLEncoding.DecodeString(c.Verifier)
	if e != nil || len(b) != 32 || !opaqueID.MatchString(c.ID) {
		return credential{}, errors.New("bad")
	}
	var v [32]byte
	copy(v[:], b)
	return credential{c.ID, v, c.Revoked}, nil
}
func (r *Registry) durableLocked() durableAuthority {
	d := durableAuthority{Schema: authoritySchema, FleetID: r.fleetID, TxnOrder: append([]string(nil), r.txnOrder...)}
	for id, a := range r.artifacts {
		d.Artifacts = append(d.Artifacts, durableArtifact{id, base64.RawURLEncoding.EncodeToString(a.verifier[:]), a.expires, a.used, a.txnID, a.result})
	}
	for _, s := range r.ships {
		x := durableShip{ID: s.id, Revoked: s.revoked, Current: dc(s.current), Generation: s.generation}
		if s.rotation != nil {
			x.Rotation = &durableRotation{dc(s.rotation.pending), s.rotation.expires, s.rotation.proved}
		}
		d.Ships = append(d.Ships, x)
	}
	for _, o := range r.observers {
		ids := make([]string, 0, len(o.ships))
		for id := range o.ships {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		d.Observers = append(d.Observers, durableObserver{dc(o.credential), ids, o.issued, o.expires})
	}
	for _, o := range r.operators {
		x := operatorRecord(o)
		d.Operators = append(d.Operators, durableOperator{dc(o.credential), x.SubjectID, x.FleetID, x.Capability, x.CredentialGeneration, x.ShipIDs, x.IssuedAt, x.ExpiresAt, o.revokeAt, o.previousID})
	}
	for _, o := range r.commanders {
		ids := make([]string, 0, len(o.ships))
		for id := range o.ships {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		d.Commanders = append(d.Commanders, durableCommander{dc(o.credential), o.subject, o.fleetID, o.generation, ids, o.issued, o.expires, o.revokeAt, o.previousID})
	}
	return d
}
func (r *Registry) restoreLocked(d durableAuthority) error {
	if d.Schema != authoritySchema || d.FleetID != r.fleetID || len(d.Artifacts) > maxArtifacts || len(d.Ships) > maxShips || len(d.Observers) > maxObservers || len(d.Operators) > maxOperators || len(d.Commanders) > maxOperators || len(d.TxnOrder) > maxTxnResults {
		return storageError()
	}
	r.artifacts = map[string]*artifact{}
	r.ships = map[string]*ship{}
	r.observers = map[string]*observer{}
	r.operators = map[string]*operatorCredential{}
	r.commanders = map[string]*commanderCredential{}
	r.txnOrder = append([]string(nil), d.TxnOrder...)
	for _, x := range d.Artifacts {
		b, e := base64.RawURLEncoding.DecodeString(x.Verifier)
		if e != nil || len(b) != 32 || !opaqueID.MatchString(x.ID) {
			return storageError()
		}
		var v [32]byte
		copy(v[:], b)
		r.artifacts[x.ID] = &artifact{v, x.Expires, x.Used, x.TxnID, x.Result}
	}
	for _, x := range d.Ships {
		c, e := credentialFrom(x.Current)
		if e != nil || !opaqueID.MatchString(x.ID) {
			return storageError()
		}
		s := &ship{id: x.ID, revoked: x.Revoked, current: c, generation: x.Generation}
		if x.Rotation != nil {
			p, e := credentialFrom(x.Rotation.Pending)
			if e != nil {
				return storageError()
			}
			s.rotation = &rotation{p, x.Rotation.Expires, x.Rotation.Proved}
		}
		r.ships[x.ID] = s
	}
	for _, x := range d.Observers {
		c, e := credentialFrom(x.Credential)
		if e != nil {
			return storageError()
		}
		o := &observer{credential: c, ships: map[string]struct{}{}, issued: x.Issued, expires: x.Expires}
		for _, id := range x.Ships {
			if !opaqueID.MatchString(id) {
				return storageError()
			}
			o.ships[id] = struct{}{}
		}
		r.observers[c.id] = o
	}
	for _, x := range d.Operators {
		c, e := credentialFrom(x.Credential)
		if e != nil || !opaqueID.MatchString(x.Subject) || x.FleetID != r.fleetID || !validOperatorCapability(x.Capability) || x.Generation == 0 || len(x.Ships) == 0 || !x.Issued.Before(x.Expires) {
			return storageError()
		}
		ships := map[string]struct{}{}
		for _, id := range x.Ships {
			if !opaqueID.MatchString(id) {
				return storageError()
			}
			ships[id] = struct{}{}
		}
		if r.operators[c.id] != nil {
			return storageError()
		}
		if x.PreviousID != "" && !opaqueID.MatchString(x.PreviousID) {
			return storageError()
		}
		r.operators[c.id] = &operatorCredential{credential: c, subject: x.Subject, fleetID: x.FleetID, capability: x.Capability, generation: x.Generation, ships: ships, issued: x.Issued, expires: x.Expires, revokeAt: x.RevokeAt, previousID: x.PreviousID}
	}
	for _, x := range d.Commanders {
		c, e := credentialFrom(x.Credential)
		if e != nil || !opaqueID.MatchString(x.Subject) || x.FleetID != r.fleetID || x.Generation == 0 || len(x.Ships) == 0 || !x.Issued.Before(x.Expires) || r.commanders[c.id] != nil {
			return storageError()
		}
		ships := map[string]struct{}{}
		for _, id := range x.Ships {
			if !opaqueID.MatchString(id) {
				return storageError()
			}
			ships[id] = struct{}{}
		}
		r.commanders[c.id] = &commanderCredential{credential: c, subject: x.Subject, fleetID: x.FleetID, generation: x.Generation, ships: ships, issued: x.Issued, expires: x.Expires, revokeAt: x.RevokeAt, previousID: x.PreviousID}
	}
	return nil
}
// loadAuthority is the open-time load. It may create the store.
func (r *Registry) loadAuthority() error { return r.readStoreLocked(true) }

// readStoreLocked replaces in-memory state with the durable state. Only the
// open-time load may create a missing store: re-creating it under a live
// registry would publish this process's snapshot as the whole authority and
// discard every credential another process has committed since.
func (r *Registry) readStoreLocked(create bool) error {
	b, exists, err := readAuthority(filepath.Clean(r.storeDir))
	if err != nil {
		return err
	}
	if !exists {
		if !create {
			return storageError()
		}
		d := r.durableLocked()
		b, _ = json.Marshal(d)
		return writeAuthority(filepath.Clean(r.storeDir), append(b, '\n'))
	}
	if len(b) > 4<<20 {
		return storageError()
	}
	var d durableAuthority
	if json.Unmarshal(b, &d) != nil {
		return storageError()
	}
	return r.restoreLocked(d)
}

// beginLocked opens one authority transaction. It takes the cross-process file
// lock and refreshes in-memory state from the durable store, so every decision
// and every mutation in this call observes commits made by any other process,
// and commitLocked can never rewrite the file from a stale snapshot. The caller
// must defer the returned release; it is held across the commit.
//
// A registry with no store is purely in-memory: there is no second writer to
// coordinate with and the snapshot is already current.
func (r *Registry) beginLocked() (durableAuthority, func(), error) {
	noop := func() {}
	if r.storeDir == "" {
		return r.durableLocked(), noop, nil
	}
	release, err := lockAuthorityStore(filepath.Clean(r.storeDir))
	if err != nil {
		return durableAuthority{}, noop, err
	}
	if err := r.readStoreLocked(false); err != nil {
		release()
		return durableAuthority{}, noop, err
	}
	return r.durableLocked(), release, nil
}

func (r *Registry) commitLocked(before durableAuthority) error {
	if r.storeDir == "" {
		return nil
	}
	b, e := json.Marshal(r.durableLocked())
	if e == nil {
		e = writeAuthority(filepath.Clean(r.storeDir), append(b, '\n'))
	}
	if e != nil {
		var uncertain *CommitUncertainError
		if !errors.As(e, &uncertain) {
			_ = r.restoreLocked(before)
		}
		return e
	}
	return nil
}
