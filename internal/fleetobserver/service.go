//go:build unix

// Package fleetobserver exposes the authenticated, read-only M7 Fleet query
// surface. It owns no listener and contains no action or proxy facility.
package fleetobserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

const (
	basePath     = "/api/fleet/v1"
	maxAuthBytes = 512
	defaultPage  = 100
)

type Service struct {
	identities *fleetidentity.Registry
	projection *fleetobserve.Projection
}

type ErrorV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Code          string `json:"code"`
}

type ShipsV1 struct {
	SchemaVersion    int                         `json:"schema_version"`
	FleetID          string                      `json:"fleet_id"`
	FleetEpoch       string                      `json:"fleet_epoch"`
	SnapshotCursor   uint64                      `json:"snapshot_cursor"`
	Ships            []fleetobserve.ShipStatusV1 `json:"ships"`
	Truncated        bool                        `json:"truncated"`
	OmittedShipCount uint                        `json:"omitted_ship_count"`
}

type ShipV1 struct {
	SchemaVersion int `json:"schema_version"`
	fleetobserve.ShipStatusV1
}

func New(identities *fleetidentity.Registry, projection *fleetobserve.Projection) (*Service, error) {
	if identities == nil || projection == nil {
		return nil, errors.New("invalid_input")
	}
	return &Service{identities: identities, projection: projection}, nil
}

// Handler returns the complete M7 observer HTTP surface. The caller remains
// responsible for an authenticated encrypted production listener.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	secureHeaders(w)
	p, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" || r.Header.Get("X-HTTP-Method-Override") != "" || r.Header.Get("X-Method-Override") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	switch {
	case r.URL.Path == basePath+"/snapshot":
		if !onlyQuery(r, nil) {
			writeError(w, 400, "invalid_request")
			return
		}
		snap := filterSnapshot(s.projection.Snapshot(), p.ShipIDs)
		writeJSON(w, http.StatusOK, snap)
	case r.URL.Path == basePath+"/ships":
		if !onlyQuery(r, nil) {
			writeError(w, 400, "invalid_request")
			return
		}
		snap := filterSnapshot(s.projection.Snapshot(), p.ShipIDs)
		writeJSON(w, http.StatusOK, ShipsV1{1, snap.FleetID, snap.FleetEpoch, snap.SnapshotCursor, snap.Ships, snap.Truncated, snap.OmittedShipCount})
	case strings.HasPrefix(r.URL.Path, basePath+"/ships/"):
		if !onlyQuery(r, nil) {
			writeError(w, 400, "invalid_request")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, basePath+"/ships/")
		if !validOpaque(id) || !allowed(p.ShipIDs, id) {
			writeError(w, 404, "not_found")
			return
		}
		for _, ship := range s.projection.Snapshot().Ships {
			if ship.ShipID == id {
				writeJSON(w, http.StatusOK, ShipV1{SchemaVersion: 1, ShipStatusV1: ship})
				return
			}
		}
		writeError(w, http.StatusNotFound, "not_found")
	case r.URL.Path == basePath+"/events" || r.URL.Path == basePath+"/events/stream":
		s.streamOrPage(w, r, p)
	default:
		writeError(w, http.StatusNotFound, "not_found")
	}
}

func (s *Service) streamOrPage(w http.ResponseWriter, r *http.Request, p fleetidentity.ObserverPrincipal) {
	if !onlyQuery(r, []string{"after", "limit"}) {
		writeError(w, 400, "invalid_request")
		return
	}
	limit := defaultPage
	if bound := s.projection.MaxPageSize(); limit > bound {
		limit = bound
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, 400, "invalid_cursor")
			return
		}
		limit = n
	}
	epoch, after, ok := parseCursor(r.URL.Query().Get("after"))
	if !ok {
		writeError(w, 400, "invalid_cursor")
		return
	}
	result, err := s.projection.Read(epoch, after, limit)
	if err != nil {
		writeError(w, 400, "invalid_cursor")
		return
	}
	setConsumedCursor(&result, after)
	filterRead(&result, p.ShipIDs)
	// The stream representation is a bounded atomic replay/snapshot document.
	// Long-lived transport can poll from NextCursor without widening the API.
	writeJSON(w, http.StatusOK, result)
}

// setConsumedCursor defines the observer API's top-level next_cursor as the
// inclusive raw replay boundary consumed by this response. It deliberately runs
// before scope filtering so an empty visible page still makes progress without
// disclosing any event from another scope.
func setConsumedCursor(r *fleetobserve.ReadResult, after *uint64) {
	if r.Snapshot != nil {
		r.NextCursor = r.Snapshot.SnapshotCursor
		return
	}
	if len(r.Events) > 0 {
		r.NextCursor = r.Events[len(r.Events)-1].Cursor
		return
	}
	if after != nil {
		r.NextCursor = *after
	}
}

func (s *Service) authenticate(r *http.Request) (fleetidentity.ObserverPrincipal, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || len(h) > maxAuthBytes || !strings.HasPrefix(h, "Bearer ") {
		return fleetidentity.ObserverPrincipal{}, false
	}
	v := strings.TrimPrefix(h, "Bearer ")
	if strings.ContainsAny(v, " \t\r\n") || strings.Count(v, ".") != 1 {
		return fleetidentity.ObserverPrincipal{}, false
	}
	id, secret, _ := strings.Cut(v, ".")
	p, err := s.identities.AuthenticateObserver(id, secret)
	return p, err == nil && p.Capability == fleetidentity.ObserveCapability
}

func parseCursor(raw string) (string, *uint64, bool) {
	if raw == "" {
		return "", nil, true
	}
	if len(raw) > 256 || strings.Count(raw, ":") != 1 {
		return "", nil, false
	}
	e, c, _ := strings.Cut(raw, ":")
	if !validOpaque(e) || c == "" || (len(c) > 1 && c[0] == '0') {
		return "", nil, false
	}
	n, err := strconv.ParseUint(c, 10, 64)
	if err != nil {
		return "", nil, false
	}
	return e, &n, true
}

func onlyQuery(r *http.Request, names []string) bool {
	allowedNames := map[string]bool{}
	for _, n := range names {
		allowedNames[n] = true
	}
	for k, values := range r.URL.Query() {
		if !allowedNames[k] || len(values) != 1 {
			return false
		}
	}
	return true
}
func validOpaque(v string) bool {
	return len(v) >= 16 && len(v) <= 128 && !strings.ContainsAny(v, "/\\\x00\r\n\t ")
}

// allowed fails closed. An empty observer scope is not "every ship": it is a
// credential that authorizes nothing, and the fleet-wide read it used to grant
// was invisible to the issuer.
func allowed(scope []string, id string) bool {
	for _, v := range scope {
		if v == id {
			return true
		}
	}
	return false
}

func filterSnapshot(in fleetobserve.FleetSnapshotV1, scope []string) fleetobserve.FleetSnapshotV1 {
	out := in.Ships[:0]
	for _, sh := range in.Ships {
		if allowed(scope, sh.ShipID) {
			out = append(out, sh)
		}
	}
	in.Ships = out
	in.OmittedShipCount = 0
	in.Truncated = false
	return in
}
func filterRead(r *fleetobserve.ReadResult, scope []string) {
	if r.Snapshot != nil {
		x := filterSnapshot(*r.Snapshot, scope)
		r.Snapshot = &x
	}
	out := r.Events[:0]
	for _, e := range r.Events {
		if allowed(scope, e.ShipID) {
			out = append(out, e)
		}
	}
	r.Events = out
}
func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, ErrorV1{1, code})
}
