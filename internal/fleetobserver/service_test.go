//go:build unix

package fleetobserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

type fixtureState struct {
	h            http.Handler
	p            *fleetobserve.Projection
	r            *fleetidentity.Registry
	ship1, ship2 string
	observer     fleetidentity.SecretCredential
}

func fixture(t *testing.T) fixtureState {
	return fixtureSized(t, 3, 2)
}

func fixtureSized(t *testing.T, replay, page int) fixtureState {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	r, err := fleetidentity.NewRegistry("flt_0123456789abcdef", c, bytes.NewReader(bytes.Repeat([]byte("0123456789abcdef"), 2048)))
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(tx string) fleetidentity.EnrollmentResult {
		a, e := r.CreateEnrollment(time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		x, e := r.Enroll(a.ArtifactID, a.Secret, tx)
		if e != nil {
			t.Fatal(e)
		}
		return x
	}
	a := enroll("txn_1111111111111111")
	b := enroll("txn_2222222222222222")
	p, err := fleetobserve.New(fleetobserve.Config{FleetID: a.FleetID, FleetEpoch: "epc_0123456789abcdef", MaxShips: 4, MaxPersonas: 4, MaxSnapshotBytes: 65536, MaxEventBytes: 8192, PerShipIngress: 2, ReplayCapacity: replay, MaxSubscribers: 2, MaxPageSize: page, MaxTerminalMetadata: 32, LeaseDuration: time.Minute, StaleRetention: time.Minute, Clock: c})
	if err != nil {
		t.Fatal(err)
	}
	install := func(id string, g uint64) {
		if e := p.Connect(id, g); e != nil {
			t.Fatal(e)
		}
		if e := p.InstallSnapshot(id, g, fleetobserve.ShipStatusV1{ShipID: id, ShipLabel: "safe", Personas: []fleetobserve.PersonaStatusV1{{Persona: "backend", Installed: true, Session: fleetobserve.SessionIdle, Turn: fleetobserve.TurnNone, Activity: fleetobserve.ActivityIdle, StatusChangedAt: c.Now()}}}); e != nil {
			t.Fatal(e)
		}
	}
	install(a.ShipID, 1)
	install(b.ShipID, 1)
	o, err := r.IssueObserver([]string{a.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(r, p)
	if err != nil {
		t.Fatal(err)
	}
	return fixtureState{s.Handler(), p, r, a.ShipID, b.ShipID, o}
}

func appendStateEvent(t *testing.T, f fixtureState, ship string) {
	t.Helper()
	e := fleetobserve.ObservationEventV1{Persona: "backend", Kind: fleetobserve.SessionStateEvent, Data: fleetobserve.EventDataV1{Session: fleetobserve.SessionWorking}}
	if err := f.p.Enqueue(ship, 1, e); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Drain(ship, 1); err != nil {
		t.Fatal(err)
	}
}

func request(t *testing.T, f fixtureState, method, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	q := httptest.NewRequest(method, path, nil)
	if auth != "" {
		q.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, q)
	return w
}
func bearer(c fleetidentity.SecretCredential) string {
	return "Bearer " + c.CredentialID + "." + c.Secret
}

func TestAuthenticatedScopedReadOnlySurface(t *testing.T) {
	f := fixture(t)
	auth := bearer(f.observer)
	for _, path := range []string{basePath + "/snapshot", basePath + "/ships", basePath + "/ships/" + f.ship1, basePath + "/events", basePath + "/events/stream"} {
		w := request(t, f, "GET", path, auth)
		if w.Code != 200 {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), f.ship2) {
			t.Fatalf("scope leak on %s", path)
		}
	}
	w := request(t, f, "GET", basePath+"/ships/"+f.ship2, auth)
	if w.Code != 404 {
		t.Fatalf("cross-scope=%d", w.Code)
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		w = request(t, f, method, basePath+"/snapshot", auth)
		if w.Code != 405 {
			t.Fatalf("%s=%d", method, w.Code)
		}
	}
	q := httptest.NewRequest("GET", basePath+"/snapshot", strings.NewReader("x"))
	q.Header.Set("Authorization", auth)
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("X-HTTP-Method-Override", "POST")
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, q)
	if w.Code != 400 {
		t.Fatalf("override/body=%d", w.Code)
	}
	for _, path := range []string{"/tell/x", "/api/live/x/tell", basePath + "/proxy/x", basePath + "/approvals", basePath + "/pty/x"} {
		w = request(t, f, "GET", path, auth)
		if w.Code != 404 {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
}

func TestCredentialClassesAndRevocationStaySeparated(t *testing.T) {
	f := fixture(t)
	for _, auth := range []string{"", "Bearer malformed", "Bearer " + f.observer.CredentialID + ".wrong", "Bearer shp_credential.secret"} {
		w := request(t, f, "GET", basePath+"/snapshot", auth)
		if w.Code != 401 || strings.Contains(w.Body.String(), f.ship1) {
			t.Fatalf("auth response=%d %s", w.Code, w.Body.String())
		}
	}
	if err := f.r.RevokeObserver(f.observer.CredentialID); err != nil {
		t.Fatal(err)
	}
	if w := request(t, f, "GET", basePath+"/snapshot", bearer(f.observer)); w.Code != 401 {
		t.Fatalf("revoked=%d", w.Code)
	}
}

func TestCursorGapRetentionAndAllowlistedQueries(t *testing.T) {
	f := fixture(t)
	auth := bearer(f.observer)
	for i := 0; i < 4; i++ {
		e := fleetobserve.ObservationEventV1{Persona: "backend", Kind: fleetobserve.SessionStateEvent, Data: fleetobserve.EventDataV1{Session: fleetobserve.SessionWorking}}
		if err := f.p.Enqueue(f.ship1, 1, e); err != nil {
			t.Fatal(err)
		}
		if err := f.p.Drain(f.ship1, 1); err != nil {
			t.Fatal(err)
		}
	}
	w := request(t, f, "GET", basePath+"/events?after=epc_0123456789abcdef:0&limit=2", auth)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var got fleetobserve.ReadResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Gap == nil || got.Gap.Reason != fleetobserve.HistoryDropped || got.Snapshot == nil {
		t.Fatalf("gap=%#v", got)
	}
	for _, path := range []string{basePath + "/events?after=bad", basePath + "/events?limit=999", basePath + "/events?unknown=1", basePath + "/snapshot?after=x"} {
		w = request(t, f, "GET", path, auth)
		if w.Code != 400 {
			t.Fatalf("%s=%d", path, w.Code)
		}
	}
}

func TestScopedPagesAdvanceAcrossLongHiddenRun(t *testing.T) {
	f := fixtureSized(t, 128, 7)
	after := f.p.Snapshot().SnapshotCursor
	for i := 0; i < 70; i++ {
		appendStateEvent(t, f, f.ship2)
	}
	want := after + 70
	for pages := 0; after < want; pages++ {
		w := request(t, f, "GET", basePath+"/events?after=epc_0123456789abcdef:"+strconv.FormatUint(after, 10)+"&limit=7", bearer(f.observer))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var got fleetobserve.ReadResult
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Events) != 0 || got.NextCursor <= after || got.NextCursor-after > 7 {
			t.Fatalf("page=%d after=%d next=%d visible=%d", pages, after, got.NextCursor, len(got.Events))
		}
		after = got.NextCursor
	}
}

func TestScopedMixedPageUsesRawConsumedBoundaryWithoutDisclosure(t *testing.T) {
	f := fixtureSized(t, 16, 4)
	after := f.p.Snapshot().SnapshotCursor
	appendStateEvent(t, f, f.ship2)
	appendStateEvent(t, f, f.ship1)
	appendStateEvent(t, f, f.ship2)
	appendStateEvent(t, f, f.ship1)
	w := request(t, f, "GET", basePath+"/events?after=epc_0123456789abcdef:"+strconv.FormatUint(after, 10)+"&limit=3", bearer(f.observer))
	var got fleetobserve.ReadResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	if got.NextCursor != after+3 || len(got.Events) != 1 || got.Events[0].Cursor != after+2 || got.Events[0].ShipID != f.ship1 {
		t.Fatalf("mixed page=%#v", got)
	}
	if strings.Contains(w.Body.String(), f.ship2) {
		t.Fatal("hidden ship disclosed")
	}
}

func TestSecurityHeadersAndNoSecretEcho(t *testing.T) {
	f := fixture(t)
	secret := "secret-canary"
	w := request(t, f, "GET", basePath+"/snapshot", "Bearer missing."+secret)
	if w.Code != 401 || strings.Contains(w.Body.String(), secret) {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	for _, h := range []string{"Cache-Control", "Referrer-Policy", "X-Content-Type-Options", "Content-Security-Policy"} {
		if w.Header().Get(h) == "" {
			t.Fatalf("missing %s", h)
		}
	}
}
