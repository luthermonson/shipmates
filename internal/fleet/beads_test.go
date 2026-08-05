package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBeadIDOK(t *testing.T) {
	good := []string{
		"abc123", "ABC123", "bd-0001", "a.b_c-d", "0",
		strings.Repeat("a", 64),
	}
	for _, id := range good {
		if !beadIDOK(id) {
			t.Errorf("beadIDOK(%q) = false, want true", id)
		}
	}

	bad := []struct{ id, why string }{
		{"", "empty"},
		{strings.Repeat("a", 65), "over the 64-char cap"},
		{"-leading-dash", "leading dash reads as a flag"},
		{"has space", "space smuggles request-line framing"},
		{"has\rcr", "CR smuggles request-line framing"},
		{"has\nlf", "LF smuggles request-line framing"},
		{"has\ttab", "tab is not in the id alphabet"},
		{"a/b", "slash escapes the path segment"},
		{"a?b=c", "query separator"},
		{"a#b", "fragment separator"},
		{"..", "parent-directory traversal"},
		{"a..b", "embedded traversal"},
		{"../../etc/passwd", "traversal"},
		{"héllo", "non-ASCII"},
		{"a%2fb", "percent is not in the alphabet"},
	}
	for _, tc := range bad {
		if beadIDOK(tc.id) {
			t.Errorf("beadIDOK(%q) = true, want false (%s)", tc.id, tc.why)
		}
	}
}

func TestHandleBeadAssign_RejectsBadBeadID(t *testing.T) {
	b := newTestFleet(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/assign", b.handleBeadAssign)

	req := httptest.NewRequest("POST", "/api/captain/laptop:captain/bead/..%2f..%2fetc/assign",
		strings.NewReader(`{"ship":"laptop:captain","persona":"data"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a bad bead id, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleBeadAssign_RejectsIncompleteBody(t *testing.T) {
	b := newTestFleet(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/assign", b.handleBeadAssign)

	cases := []struct{ name, body string }{
		{"not json", `oops`},
		{"empty object", `{}`},
		{"missing persona", `{"ship":"laptop:captain"}`},
		{"missing ship", `{"persona":"data"}`},
		{"blank persona", `{"ship":"laptop:captain","persona":"   "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/captain/laptop:captain/bead/abc123/assign",
				strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// A well-formed assign aimed at a captain the fleet has never seen must 404
// from the proxy layer rather than panic or hang.
func TestHandleBeadAssign_UnknownCarryingShip(t *testing.T) {
	b := newTestFleet(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/assign", b.handleBeadAssign)

	req := httptest.NewRequest("POST", "/api/captain/ghost/bead/abc123/assign",
		strings.NewReader(`{"ship":"ghost","persona":"data"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// The nudge is fire-and-forget: it must answer 202 immediately even with no
// ships connected, so the pushing captain never blocks on the fleet.
func TestHandleBeadsNudge_AcceptsWithNoShips(t *testing.T) {
	b := newTestFleet(t, "")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.handleBeadsNudge(rec, httptest.NewRequest("POST", "/api/beads/nudge",
			strings.NewReader(`{"from":"laptop:captain"}`)))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleBeadsNudge blocked — it must not wait on fan-out")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
}

func TestHandleBeadsNudge_ToleratesGarbageBody(t *testing.T) {
	b := newTestFleet(t, "")
	rec := httptest.NewRecorder()
	b.handleBeadsNudge(rec, httptest.NewRequest("POST", "/api/beads/nudge", strings.NewReader(`not json`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 even for an undecodable body, got %d", rec.Code)
	}
}

// deliverDispatch's wake-up tell is the message a mate actually reads. The
// bead id must appear in every command it's told to run — a truncated or
// mis-escaped id sends the mate chasing a nonexistent bead.
func TestDeliverDispatch_TellMessageShape(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	b.captains["laptop:captain"] = &Captain{ClientKey: "laptop:captain", Port: ship.port}
	connectShip(t, b, ship, "laptop:captain")

	err := b.deliverDispatch(context.Background(), queuedDispatch{
		Ship: "laptop:captain", Persona: "data", Bead: "bd-42", Title: "fix the warp core",
	}, true)
	if err != nil {
		t.Fatalf("deliverDispatch: %v", err)
	}

	pulls := ship.hits("POST /beads/pull")
	if len(pulls) != 1 {
		t.Fatalf("want exactly one graph pull, got %d", len(pulls))
	}
	if !strings.Contains(pulls[0].rawPath, "wait=1") {
		t.Errorf("pull must be synchronous (wait=1), got %q", pulls[0].rawPath)
	}

	tells := ship.hits("POST /tell/data")
	if len(tells) != 1 {
		t.Fatalf("want exactly one tell to the target persona, got %d", len(tells))
	}
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(tells[0].body, &msg); err != nil {
		t.Fatalf("tell body: %v", err)
	}
	for _, want := range []string{"[fleet dispatch]", "bd-42", "fix the warp core", "bd show bd-42", "bd update bd-42", "bd close bd-42"} {
		if !strings.Contains(msg.Message, want) {
			t.Errorf("dispatch tell missing %q:\n%s", want, msg.Message)
		}
	}
}

// pull=false is the same-ship case: the bead is already local, so a redundant
// synchronous pull would just add latency to every dispatch.
func TestDeliverDispatch_SkipsPullOnSameShip(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	b.captains["laptop:captain"] = &Captain{ClientKey: "laptop:captain", Port: ship.port}
	connectShip(t, b, ship, "laptop:captain")

	if err := b.deliverDispatch(context.Background(), queuedDispatch{
		Ship: "laptop:captain", Persona: "data", Bead: "bd-7",
	}, false); err != nil {
		t.Fatalf("deliverDispatch: %v", err)
	}
	if n := len(ship.hits("POST /beads/pull")); n != 0 {
		t.Errorf("same-ship dispatch must not pull, got %d pulls", n)
	}
	if n := len(ship.hits("POST /tell/data")); n != 1 {
		t.Errorf("want 1 tell, got %d", n)
	}
}

// A ship that can't sync must fail the dispatch loudly rather than telling a
// mate to work a bead it doesn't have.
func TestDeliverDispatch_PullFailureAborts(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["POST /beads/pull"] = http.StatusInternalServerError
	ship.bodies["POST /beads/pull"] = []byte("no beads workspace")
	b := newTestFleet(t, "")
	b.captains["laptop:captain"] = &Captain{ClientKey: "laptop:captain", Port: ship.port}
	connectShip(t, b, ship, "laptop:captain")

	err := b.deliverDispatch(context.Background(), queuedDispatch{
		Ship: "laptop:captain", Persona: "data", Bead: "bd-7",
	}, true)
	if err == nil {
		t.Fatal("want an error when the target can't pull the graph")
	}
	if !strings.Contains(err.Error(), "could not pull") {
		t.Errorf("unhelpful error: %v", err)
	}
	if n := len(ship.hits("POST /tell/data")); n != 0 {
		t.Errorf("must not tell the mate after a failed pull, got %d tells", n)
	}
}

// assignMux wires just the assign route.
func assignMux(b *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/assign", b.handleBeadAssign)
	return mux
}

// Same-ship reassignment: set the assignee and wake the mate. No cross-ship
// graph pull is needed because the bead is already local.
func TestHandleBeadAssign_SameShip(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	rec := httptest.NewRecorder()
	assignMux(b).ServeHTTP(rec, httptest.NewRequest("POST", "/api/captain/homelab:captain/bead/abc123/assign",
		strings.NewReader(`{"ship":"homelab:captain","persona":"data","title":"warp core"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeJSON[map[string]string](t, rec.Body.Bytes())
	if got["assignee"] != "data@homelab" {
		t.Fatalf("assignee should be persona@shipname, got %q", got["assignee"])
	}
	if got["dispatched"] != "true" {
		t.Errorf("want dispatched=true, got %+v", got)
	}
	if n := len(ship.hits("POST /bead/abc123/update")); n != 1 {
		t.Errorf("want 1 assignee update, got %d", n)
	}
	if n := len(ship.hits("POST /beads/pull")); n != 0 {
		t.Errorf("same-ship assign must not pull the graph, got %d pulls", n)
	}
	if n := len(ship.hits("POST /tell/data")); n != 1 {
		t.Errorf("mate not woken, tells=%d", n)
	}
}

// Cross-ship dispatch: the assignee is written on the ship the operator is
// looking at, then the TARGET ship pulls the graph and its mate is woken.
func TestHandleBeadAssign_CrossShip(t *testing.T) {
	carrier := newFakeShip(t)
	target := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, carrier, "homelab:captain")
	connectShip(t, b, target, "laptop:captain")

	rec := httptest.NewRecorder()
	assignMux(b).ServeHTTP(rec, httptest.NewRequest("POST", "/api/captain/homelab:captain/bead/abc123/assign",
		strings.NewReader(`{"ship":"laptop:captain","persona":"backend","title":"port the api"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeJSON[map[string]string](t, rec.Body.Bytes())
	if got["assignee"] != "backend@laptop" {
		t.Fatalf("assignee: %q", got["assignee"])
	}
	if n := len(carrier.hits("POST /bead/abc123/update")); n != 1 {
		t.Errorf("assignee must be written on the carrying ship, got %d updates", n)
	}
	if n := len(target.hits("POST /bead/abc123/update")); n != 0 {
		t.Errorf("target ship must not be written to directly, got %d updates", n)
	}
	if n := len(target.hits("POST /beads/pull")); n != 1 {
		t.Errorf("target must sync the graph before being told, got %d pulls", n)
	}
	if n := len(target.hits("POST /tell/backend")); n != 1 {
		t.Errorf("target mate not woken, tells=%d", n)
	}
	if n := len(carrier.hits("POST /tell/backend")); n != 0 {
		t.Errorf("the carrying ship must not be told, got %d tells", n)
	}
}

// If the ASSIGNEE write fails there is nothing to dispatch — the operator must
// see the failure, not a false "dispatched".
func TestHandleBeadAssign_UpdateFailureAborts(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["POST /bead/abc123/update"] = http.StatusConflict
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	rec := httptest.NewRecorder()
	assignMux(b).ServeHTTP(rec, httptest.NewRequest("POST", "/api/captain/homelab:captain/bead/abc123/assign",
		strings.NewReader(`{"ship":"homelab:captain","persona":"data"}`)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("the ship's status must pass through, got %d", rec.Code)
	}
	if n := len(ship.hits("POST /tell/data")); n != 0 {
		t.Errorf("must not wake a mate after a failed assignee write, tells=%d", n)
	}
}

// The target ship is offline: the assignee is already on the shared graph, so
// the wake-up is queued for the sweep loop rather than failing the operator's
// action outright.
func TestHandleBeadAssign_QueuesForOfflineTarget(t *testing.T) {
	carrier := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, carrier, "homelab:captain")

	rec := httptest.NewRecorder()
	assignMux(b).ServeHTTP(rec, httptest.NewRequest("POST", "/api/captain/homelab:captain/bead/abc123/assign",
		strings.NewReader(`{"ship":"boat:captain","persona":"data","title":"later"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	got := decodeJSON[map[string]string](t, rec.Body.Bytes())
	if got["queued"] != "true" {
		t.Fatalf("want queued=true for an offline target, got %+v", got)
	}
	if got["assignee"] != "data@boat" {
		t.Errorf("assignee: %q", got["assignee"])
	}
	// The assignee write still happened on the carrying ship — that's what
	// makes the queue safe to drop on restart.
	if n := len(carrier.hits("POST /bead/abc123/update")); n != 1 {
		t.Errorf("assignee not written before queueing, updates=%d", n)
	}

	b.mu.Lock()
	q := append([]queuedDispatch(nil), b.dispatchQ...)
	b.mu.Unlock()
	if len(q) != 1 {
		t.Fatalf("want 1 queued dispatch, got %d", len(q))
	}
	if q[0].Ship != "boat:captain" || q[0].Persona != "data" || q[0].Bead != "abc123" || q[0].Title != "later" {
		t.Errorf("queued entry wrong: %+v", q[0])
	}
	if q[0].At.IsZero() {
		t.Error("queued entry needs a timestamp or the TTL sweep can't expire it")
	}
}

// dispatchQueueTTL is what stops a queue entry from waking a mate about work
// that was assigned days ago.
func TestDispatchQueueTTL(t *testing.T) {
	if dispatchQueueTTL <= 0 {
		t.Fatalf("TTL must be positive, got %v", dispatchQueueTTL)
	}
	if dispatchQueueTTL > 7*24*time.Hour {
		t.Errorf("TTL of %v is long enough to be confusing when it fires", dispatchQueueTTL)
	}
}
