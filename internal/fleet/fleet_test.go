package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// readHTTPResponse
// ---------------------------------------------------------------------------

// readsFrom runs readHTTPResponse against a canned raw response written onto
// one end of a net.Pipe.
func readsFrom(t *testing.T, raw string) (*proxyResp, error) {
	t.Helper()
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = server.Write([]byte(raw))
	}()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	return readHTTPResponse(client)
}

func TestReadHTTPResponse_ContentLength(t *testing.T) {
	resp, err := readsFrom(t, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\n[1,2,3]")
	if err != nil {
		t.Fatalf("readHTTPResponse: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "[1,2,3]" {
		t.Fatalf("got %d %q", resp.Status, resp.Body)
	}
}

// The regression this function exists for: Go servers switch to
// Transfer-Encoding: chunked once a response outgrows the 2KB write buffer,
// and the old hand-rolled header/body split glued chunk-size framing into the
// body, breaking every JSON consumer downstream.
func TestReadHTTPResponse_DecodesChunked(t *testing.T) {
	raw := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"5\r\n[1,2,\r\n" +
		"2\r\n3]\r\n" +
		"0\r\n\r\n"
	resp, err := readsFrom(t, raw)
	if err != nil {
		t.Fatalf("readHTTPResponse: %v", err)
	}
	if string(resp.Body) != "[1,2,3]" {
		t.Fatalf("chunk framing leaked into the body: %q", resp.Body)
	}
	if !json.Valid(resp.Body) {
		t.Fatalf("decoded body is not valid JSON: %q", resp.Body)
	}
}

func TestReadHTTPResponse_PropagatesErrorStatus(t *testing.T) {
	resp, err := readsFrom(t, "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 4\r\n\r\nnope")
	if err != nil {
		t.Fatalf("readHTTPResponse: %v", err)
	}
	if resp.Status != 503 || string(resp.Body) != "nope" {
		t.Fatalf("got %d %q", resp.Status, resp.Body)
	}
}

func TestReadHTTPResponse_GarbageIsAnError(t *testing.T) {
	if _, err := readsFrom(t, "this is not http\r\n\r\n"); err == nil {
		t.Fatal("want an error for a non-HTTP response")
	}
}

// ---------------------------------------------------------------------------
// writeProxied
// ---------------------------------------------------------------------------

func TestWriteProxied(t *testing.T) {
	t.Run("passes status and body through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeProxied(rec, http.StatusTeapot, []byte(`{"a":1}`), nil)
		if rec.Code != http.StatusTeapot || rec.Body.String() != `{"a":1}` {
			t.Fatalf("got %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("error wins over body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeProxied(rec, http.StatusBadGateway, []byte("ignored"), fmt.Errorf("dial captain: boom"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "ignored") {
			t.Errorf("body should be the error, got %q", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "dial captain") {
			t.Errorf("error text lost: %q", rec.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// authorize (the remotedialer Authorizer)
// ---------------------------------------------------------------------------

func authorizeReq(key, token string) *http.Request {
	r := httptest.NewRequest("GET", "/connect", nil)
	if key != "" {
		r.Header.Set("X-Shipmates-Identity", key)
	}
	r.Header.Set("X-Shipmates-Repo", "homelab")
	r.Header.Set("X-Shipmates-Repo-URL", "https://example.invalid/homelab")
	r.Header.Set("X-Shipmates-Install-ID", "inst-1")
	r.Header.Set("X-Shipmates-Persona", "picard")
	r.Header.Set("X-Shipmates-Port", "4321")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAuthorize_RejectsWrongToken(t *testing.T) {
	b := newTestFleet(t, "s3cret")
	key, authed, err := b.authorize(authorizeReq("homelab:captain", "wrong"))
	if err != nil {
		t.Fatalf("a wrong token should be a clean refusal, not an error: %v", err)
	}
	if authed || key != "" {
		t.Fatalf("wrong token authorized: key=%q authed=%v", key, authed)
	}
	if len(b.captains) != 0 {
		t.Errorf("an unauthorized connect must not register a captain")
	}
}

func TestAuthorize_RequiresIdentityHeader(t *testing.T) {
	b := newTestFleet(t, "")
	_, authed, err := b.authorize(authorizeReq("", ""))
	if err == nil {
		t.Fatal("missing X-Shipmates-Identity must be an error")
	}
	if authed {
		t.Error("must not authorize without an identity")
	}
}

func TestAuthorize_RecordsCaptainAndPreservesFirstSeen(t *testing.T) {
	b := newTestFleet(t, "s3cret")
	key, authed, err := b.authorize(authorizeReq("homelab:captain", "s3cret"))
	if err != nil || !authed || key != "homelab:captain" {
		t.Fatalf("authorize: key=%q authed=%v err=%v", key, authed, err)
	}
	first := b.captains["homelab:captain"]
	if first == nil {
		t.Fatal("captain not registered")
	}
	if first.Repo != "homelab" || first.Persona != "picard" || first.Port != 4321 ||
		first.InstallID != "inst-1" || first.RepoURL != "https://example.invalid/homelab" {
		t.Fatalf("identity headers not captured: %+v", first)
	}
	firstSeen := first.FirstSeen
	if firstSeen.IsZero() {
		t.Fatal("FirstSeen not set")
	}

	// Reconnect with changed metadata: the record updates in place, but the
	// first-seen timestamp is history and must survive.
	time.Sleep(2 * time.Millisecond)
	r := authorizeReq("homelab:captain", "s3cret")
	r.Header.Set("X-Shipmates-Persona", "data")
	r.Header.Set("X-Shipmates-Port", "9999")
	if _, authed, err := b.authorize(r); err != nil || !authed {
		t.Fatalf("reconnect: authed=%v err=%v", authed, err)
	}
	again := b.captains["homelab:captain"]
	if !again.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen changed on reconnect: %v -> %v", firstSeen, again.FirstSeen)
	}
	if again.Persona != "data" || again.Port != 9999 {
		t.Errorf("reconnect did not refresh metadata: %+v", again)
	}
	if !again.LastSeen.After(firstSeen) {
		t.Errorf("LastSeen not advanced: %v", again.LastSeen)
	}
	if len(b.captains) != 1 {
		t.Errorf("reconnect created a duplicate captain: %d entries", len(b.captains))
	}
}

// A malformed port header must not take the connection down — the captain is
// still reachable for everything that doesn't need the port.
func TestAuthorize_ToleratesUnparseablePort(t *testing.T) {
	b := newTestFleet(t, "")
	r := authorizeReq("homelab:captain", "")
	r.Header.Set("X-Shipmates-Port", "not-a-number")
	if _, authed, err := b.authorize(r); err != nil || !authed {
		t.Fatalf("authed=%v err=%v", authed, err)
	}
	if got := b.captains["homelab:captain"].Port; got != 0 {
		t.Errorf("want port 0 for an unparseable header, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// proxy()
// ---------------------------------------------------------------------------

func TestProxy_UnknownCaptainIs404(t *testing.T) {
	b := newTestFleet(t, "")
	_, status, err := b.proxy(context.Background(), "ghost", "GET", "/events", nil)
	if status != http.StatusNotFound || err == nil {
		t.Fatalf("want 404 + error, got %d %v", status, err)
	}
}

// Known captain, dead tunnel: 504, not 404 — the operator needs to tell "I
// never heard of that ship" apart from "that ship is offline right now".
func TestProxy_KnownButDisconnectedIs504(t *testing.T) {
	b := newTestFleet(t, "")
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Port: 1}
	_, status, err := b.proxy(context.Background(), "homelab:captain", "GET", "/events", nil)
	if status != http.StatusGatewayTimeout || err == nil {
		t.Fatalf("want 504 + error, got %d %v", status, err)
	}
}

func TestProxy_ThroughTunnel(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /status.json", []map[string]string{{"persona": "picard", "status": "idle"}})
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	body, status, err := b.proxy(context.Background(), "homelab:captain", "GET", "/status.json", nil)
	if err != nil || status != 200 {
		t.Fatalf("proxy: %d %v", status, err)
	}
	got := decodeJSON[[]map[string]string](t, body)
	if len(got) != 1 || got[0]["persona"] != "picard" {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestProxy_ForwardsBodyAndStatus(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["POST /tell/data"] = http.StatusAccepted
	ship.bodies["POST /tell/data"] = []byte(`{"queued":true}`)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	body, status, err := b.proxy(context.Background(), "homelab:captain", "POST", "/tell/data", []byte(`{"message":"hello"}`))
	if err != nil {
		t.Fatalf("proxy: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status not passed through: %d", status)
	}
	if string(body) != `{"queued":true}` {
		t.Fatalf("body not passed through: %s", body)
	}
	hits := ship.hits("POST /tell/data")
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if string(hits[0].body) != `{"message":"hello"}` {
		t.Errorf("request body mangled: %q", hits[0].body)
	}
	if !strings.HasPrefix(hits[0].ctype, "application/json") {
		t.Errorf("want a JSON content type, got %q", hits[0].ctype)
	}
}

// Bodies large enough to make the ship's server switch to chunked framing must
// arrive intact — this is the readHTTPResponse regression, exercised through
// the real tunnel rather than a canned buffer.
func TestProxy_LargeChunkedResponseSurvives(t *testing.T) {
	ship := newFakeShip(t)
	big := make([]map[string]string, 2000)
	for i := range big {
		big[i] = map[string]string{"time": fmt.Sprintf("2026-01-01T00:00:%02d", i%60), "text": strings.Repeat("x", 40)}
	}
	ship.setJSON("GET /events", big)

	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	body, status, err := b.proxy(context.Background(), "homelab:captain", "GET", "/events", nil)
	if err != nil || status != 200 {
		t.Fatalf("proxy: %d %v", status, err)
	}
	if !json.Valid(body) {
		t.Fatalf("large response body is not valid JSON (%d bytes): %.120q…", len(body), body)
	}
	if got := decodeJSON[[]map[string]string](t, body); len(got) != len(big) {
		t.Fatalf("want %d events, got %d", len(big), len(got))
	}
}

// ---------------------------------------------------------------------------
// route helpers
// ---------------------------------------------------------------------------

func TestProxyHelpers_PathAndQueryConstruction(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/beads", b.proxyGet("/beads.json"))
	mux.HandleFunc("GET /api/captain/{key}/bead/{id}", b.proxyGet2("/bead/%s", "id"))
	mux.HandleFunc("POST /api/captain/{key}/bead", b.proxyPost("/bead"))
	mux.HandleFunc("POST /api/captain/{key}/bead/{id}/close", b.proxyPost2("/bead/%s/close", "id"))
	mux.HandleFunc("POST /api/captain/{key}/pty/{persona}/input", b.proxyPTYPost("/pty/%s/input"))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cases := []struct {
		name    string
		method  string
		url     string
		body    string
		wantRaw string
	}{
		{"get keeps query string", "GET", "/api/captain/homelab:captain/beads?all=1&limit=5", "", "/beads.json?all=1&limit=5"},
		{"get with path param", "GET", "/api/captain/homelab:captain/bead/abc123", "", "/bead/abc123"},
		{"post fixed path", "POST", "/api/captain/homelab:captain/bead", `{"title":"x"}`, "/bead"},
		{"post with path param", "POST", "/api/captain/homelab:captain/bead/abc123/close", `{}`, "/bead/abc123/close"},
		{"pty post keeps query string", "POST", "/api/captain/homelab:captain/pty/data/input?client=w1", `{"data":"ls"}`, "/pty/data/input?client=w1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.URL+tc.url, strings.NewReader(tc.body))
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			var found bool
			for _, h := range ship.allHits() {
				if h.rawPath == tc.wantRaw && h.method == tc.method {
					found = true
				}
			}
			if !found {
				t.Fatalf("ship never saw %s %s; saw %+v", tc.method, tc.wantRaw, ship.allHits())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleCaptains
// ---------------------------------------------------------------------------

// An empty fleet must serialize as [] — a JSON null makes the UI's .map()
// throw, which looks like a fleet outage rather than an empty roster.
func TestHandleCaptains_EmptyIsArrayNotNull(t *testing.T) {
	b := newTestFleet(t, "")
	rec := httptest.NewRecorder()
	b.handleCaptains(rec, httptest.NewRequest("GET", "/api/captains", nil))
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("want [], got %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
}

func TestHandleCaptains_MarksConnectedAndStale(t *testing.T) {
	ship := newFakeShip(t)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")
	// A captain the fleet remembers but that isn't dialed in right now.
	b.mu.Lock()
	b.captains["laptop:captain"] = &Captain{ClientKey: "laptop:captain", Repo: "laptop", Persona: "data"}
	b.mu.Unlock()

	rec := httptest.NewRecorder()
	b.handleCaptains(rec, httptest.NewRequest("GET", "/api/captains", nil))

	type wire struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		Connected bool   `json:"connected"`
	}
	got := decodeJSON[[]wire](t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("want 2 captains, got %d: %s", len(got), rec.Body.String())
	}
	state := map[string]bool{}
	for _, w := range got {
		state[w.ClientKey] = w.Connected
	}
	if !state["homelab:captain"] {
		t.Errorf("live captain reported as disconnected")
	}
	if state["laptop:captain"] {
		t.Errorf("stale captain reported as connected")
	}
}

// ---------------------------------------------------------------------------
// aggregate fan-out
// ---------------------------------------------------------------------------

func TestAggregateHandlers_EmptyFleetReturnsArrays(t *testing.T) {
	b := newTestFleet(t, "")
	for name, h := range map[string]http.HandlerFunc{
		"status":  b.handleAggregateStatus,
		"pending": b.handleAggregatePending,
		"beads":   b.handleAggregateBeads,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/api/"+name, nil))
		if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
			t.Errorf("%s: want [], got %q", name, got)
		}
	}
}

func TestHandleAggregateStatus_TagsShipAndRepo(t *testing.T) {
	ship := newFakeShip(t)
	ship.setJSON("GET /status.json", []map[string]string{
		{"persona": "picard", "status": "working", "tool": "Bash", "since": "2026-01-01T00:00:00Z"},
		{"persona": "data", "status": "blocked", "pending_id": "p1"},
	})
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	rec := httptest.NewRecorder()
	b.handleAggregateStatus(rec, httptest.NewRequest("GET", "/api/status", nil))

	type entry struct {
		ClientKey string `json:"client_key"`
		Repo      string `json:"repo"`
		Persona   string `json:"persona"`
		Status    string `json:"status"`
		Tool      string `json:"tool"`
		PendingID string `json:"pending_id"`
	}
	got := decodeJSON[[]entry](t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("want 2 mates, got %d: %s", len(got), rec.Body.String())
	}
	for _, e := range got {
		if e.ClientKey != "homelab:captain" {
			t.Errorf("entry not tagged with its ship: %+v", e)
		}
		if e.Repo != "homelab" {
			t.Errorf("entry not tagged with its repo: %+v", e)
		}
	}
	if got[0].Tool != "Bash" || got[1].PendingID != "p1" {
		t.Errorf("per-mate fields dropped: %+v", got)
	}
}

// One ship being unreachable or serving junk must not blank the whole board.
func TestHandleAggregateStatus_SurvivesOneBadShip(t *testing.T) {
	good := newFakeShip(t)
	good.setJSON("GET /status.json", []map[string]string{{"persona": "picard", "status": "idle"}})
	bad := newFakeShip(t)
	bad.bodies["GET /status.json"] = []byte("<html>not json</html>")

	b := newTestFleet(t, "")
	connectShip(t, b, good, "homelab:captain")
	connectShip(t, b, bad, "laptop:captain")

	rec := httptest.NewRecorder()
	b.handleAggregateStatus(rec, httptest.NewRequest("GET", "/api/status", nil))
	got := decodeJSON[[]map[string]any](t, rec.Body.Bytes())
	if len(got) != 1 {
		t.Fatalf("want the one good ship's mates, got %d: %s", len(got), rec.Body.String())
	}
	if got[0]["client_key"] != "homelab:captain" {
		t.Errorf("wrong ship survived: %+v", got[0])
	}
}

func TestHandleAggregatePending_FlattensAcrossShips(t *testing.T) {
	a := newFakeShip(t)
	a.setJSON("GET /pending.json", []map[string]string{{"id": "p1", "persona": "picard", "tool": "Bash"}})
	c := newFakeShip(t)
	c.setJSON("GET /pending.json", []map[string]string{{"id": "p2", "persona": "data", "tool": "Write"}})

	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")

	rec := httptest.NewRecorder()
	b.handleAggregatePending(rec, httptest.NewRequest("GET", "/api/pending", nil))
	got := decodeJSON[[]map[string]string](t, rec.Body.Bytes())
	if len(got) != 2 {
		t.Fatalf("want 2 pending entries, got %d: %s", len(got), rec.Body.String())
	}
	byID := map[string]string{}
	for _, e := range got {
		byID[e["id"]] = e["client_key"]
	}
	if byID["p1"] != "homelab:captain" || byID["p2"] != "laptop:captain" {
		t.Fatalf("pending entries mis-attributed: %+v", byID)
	}
}

// Ships sync one shared bead graph, so the same bead shows up on every ship.
// The union must be deduped by id, list every ship carrying it, and come back
// in a stable order — otherwise the UI's list reshuffles on every poll.
func TestHandleAggregateBeads_DedupesAndSorts(t *testing.T) {
	shared := []map[string]any{
		{"id": "bbb", "title": "shared bead", "status": "open"},
		{"id": "aaa", "title": "also shared", "status": "open"},
	}
	a := newFakeShip(t)
	a.setJSON("GET /beads.json", append(append([]map[string]any{}, shared...),
		map[string]any{"id": "zzz", "title": "only on homelab", "status": "open"}))
	c := newFakeShip(t)
	c.setJSON("GET /beads.json", shared)

	b := newTestFleet(t, "")
	connectShip(t, b, a, "homelab:captain")
	connectShip(t, b, c, "laptop:captain")

	rec := httptest.NewRecorder()
	b.handleAggregateBeads(rec, httptest.NewRequest("GET", "/api/beads", nil))

	type bead struct {
		ID    string   `json:"id"`
		Title string   `json:"title"`
		Ships []string `json:"ships"`
	}
	got := decodeJSON[[]bead](t, rec.Body.Bytes())
	if len(got) != 3 {
		t.Fatalf("want 3 deduped beads, got %d: %s", len(got), rec.Body.String())
	}
	if got[0].ID != "aaa" || got[1].ID != "bbb" || got[2].ID != "zzz" {
		t.Fatalf("beads not sorted by id: %+v", got)
	}
	for _, bd := range got[:2] {
		if len(bd.Ships) != 2 {
			t.Errorf("shared bead %s should list both ships, got %v", bd.ID, bd.Ships)
		}
		if bd.Ships[0] != "homelab:captain" || bd.Ships[1] != "laptop:captain" {
			t.Errorf("ships list not sorted for %s: %v", bd.ID, bd.Ships)
		}
	}
	if len(got[2].Ships) != 1 || got[2].Ships[0] != "homelab:captain" {
		t.Errorf("single-ship bead attribution wrong: %v", got[2].Ships)
	}
}

// ---------------------------------------------------------------------------
// handleStream
// ---------------------------------------------------------------------------

func TestHandleStream_UnknownCaptainIs404(t *testing.T) {
	b := newTestFleet(t, "")
	rec := httptest.NewRecorder()
	b.handleStream(rec, httptest.NewRequest("GET", "/api/captain/ghost/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// The watermark is an INDEX, not a timestamp: a broadcast tell produces a
// batch of events sharing one second, and a timestamp watermark silently
// dropped all but the first. Also covers the reset branch — when the captain
// restarts, its log shrinks and everything must replay.
func TestHandleStream_SendsEachEventOnceThenReplaysAfterReset(t *testing.T) {
	ship := newFakeShip(t)
	sameSecond := []map[string]string{
		{"time": "2026-01-01T00:00:00Z", "type": "assistant", "text": "one"},
		{"time": "2026-01-01T00:00:00Z", "type": "assistant", "text": "two"},
		{"time": "2026-01-01T00:00:00Z", "type": "assistant", "text": "three"},
	}
	ship.setJSON("GET /events", sameSecond)

	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/stream", b.handleStream)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/captain/homelab:captain/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("want an SSE stream, got %q", ct)
	}

	// readTexts pulls n `data:` frames off the stream, failing on timeout.
	sc := bufio.NewScanner(resp.Body)
	readTexts := func(n int) []string {
		var out []string
		for len(out) < n {
			if !sc.Scan() {
				t.Fatalf("stream ended after %d/%d frames (%v)", len(out), n, sc.Err())
			}
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatalf("bad SSE frame %q: %v", line, err)
			}
			out = append(out, ev.Text)
		}
		return out
	}

	if got := readTexts(3); !equalStrings(got, []string{"one", "two", "three"}) {
		t.Fatalf("same-second batch not delivered in full: %v", got)
	}

	// The captain restarts: its append-only log resets to a single event.
	ship.setJSON("GET /events", []map[string]string{
		{"time": "2026-01-01T00:00:00Z", "type": "assistant", "text": "after restart"},
	})
	if got := readTexts(1); got[0] != "after restart" {
		t.Fatalf("watermark did not reset after the captain's log shrank: %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// New() / options wiring / store
// ---------------------------------------------------------------------------

func TestNew_VoiceDisabledByDefault(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	b, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.conv != nil {
		t.Errorf("no voice flags should mean no conv config, got %+v", b.conv)
	}
	if b.store != nil {
		t.Errorf("no --store should mean no store")
	}
	if b.dialer == nil || b.policy == nil {
		t.Errorf("dialer and policy must always be wired")
	}
}

func TestNew_WiresConvConfig(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	b, err := New(Options{
		Token:    "  s3cret  ", // trimmed: a trailing newline from a secrets file must not break auth
		LLMURL:   "http://localhost:11434/v1/",
		LLMModel: "qwen2.5:7b",
		LLMKey:   " key ",
		TTSVoice: " af_heart ",
		TTSURL:   " http://localhost:8880/v1/audio/speech ",
		TTSModel: " kokoro ",
		STTURL:   " http://localhost:8080/inference ",
		STTModel: " whisper-1 ",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.token != "s3cret" {
		t.Errorf("token not trimmed: %q", b.token)
	}
	if b.conv == nil {
		t.Fatal("conv config not built")
	}
	// The trailing slash must go, or every request becomes …/v1//chat/completions.
	if b.conv.url != "http://localhost:11434/v1" {
		t.Errorf("llm url not normalized: %q", b.conv.url)
	}
	for name, got := range map[string]string{
		"key": b.conv.key, "voice": b.conv.voice, "ttsURL": b.conv.ttsURL,
		"ttsModel": b.conv.ttsModel, "sttURL": b.conv.sttURL, "sttModel": b.conv.sttModel,
	} {
		if strings.TrimSpace(got) != got {
			t.Errorf("%s not trimmed: %q", name, got)
		}
	}
	if b.conv.client == nil {
		t.Error("conv http client must be set")
	}
	if b.conv.brain != nil {
		t.Error("claude brain must not be built for the openai backend")
	}
}

// Any one voice flag on its own is enough to build the conv config — the /api
// surface must not 503 just because the operator only asked for TTS.
func TestNew_AnySingleVoiceFlagEnablesConv(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	for name, opt := range map[string]Options{
		"llm url":    {LLMURL: "http://x/v1"},
		"tts voice":  {TTSVoice: "en-US-AriaNeural"},
		"tts url":    {TTSURL: "http://x/v1/audio/speech"},
		"stt url":    {STTURL: "http://x/inference"},
		"claude cli": {LLMBackend: "claude-cli"},
	} {
		b, err := New(opt)
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}
		if b.conv == nil {
			t.Errorf("%s: conv config not built", name)
		}
		_ = b.Close()
	}
}

func TestNew_ClaudeCLIBackendBuildsBrain(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	b, err := New(Options{LLMBackend: "claude-cli", LLMModel: "haiku", Addr: "127.0.0.1:8443", Token: "tok"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.conv == nil || b.conv.brain == nil {
		t.Fatal("claude-cli backend did not build a brain")
	}
	if b.conv.brain.model != "haiku" || b.conv.brain.addr != "127.0.0.1:8443" || b.conv.brain.token != "tok" {
		t.Errorf("brain wiring wrong: %+v", b.conv.brain)
	}
}

func TestNew_BadStorePathIsAnError(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	// A path whose parent directory does not exist.
	bad := filepath.Join(t.TempDir(), "no-such-dir", "fleet.db")
	if _, err := New(Options{Store: bad}); err == nil {
		t.Fatal("New must fail when the store can't be opened")
	}
}

func TestStore_RoundTrip(t *testing.T) {
	t.Setenv("SHIPMATES_FLEET_POLICY", filepath.Join(t.TempDir(), "none.yaml"))
	path := filepath.Join(t.TempDir(), "fleet.db")
	b, err := New(Options{Store: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.store == nil {
		t.Fatal("store not opened")
	}

	first := time.Now().Add(-time.Hour)
	c := &Captain{ClientKey: "homelab:captain", Repo: "homelab", InstallID: "i1", Persona: "picard",
		FirstSeen: first, LastSeen: first}
	if err := b.store.upsertCaptain(c); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Reconnect with a new persona: upsert, not a duplicate row.
	c.Persona = "data"
	c.LastSeen = time.Now()
	if err := b.store.upsertCaptain(c); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	var rows int
	var persona string
	var firstSeen int64
	if err := b.store.db.QueryRow(`SELECT COUNT(*) FROM captains`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("want 1 captain row after two upserts, got %d", rows)
	}
	if err := b.store.db.QueryRow(`SELECT persona, first_seen FROM captains`).Scan(&persona, &firstSeen); err != nil {
		t.Fatal(err)
	}
	if persona != "data" {
		t.Errorf("upsert did not refresh persona: %q", persona)
	}
	if firstSeen != first.Unix() {
		t.Errorf("upsert overwrote first_seen: %d want %d", firstSeen, first.Unix())
	}

	for i := 0; i < 3; i++ {
		if err := b.store.insertEvent("homelab:captain", fmt.Sprintf("2026-01-01T00:00:0%d", i), "picard", "assistant", "hi"); err != nil {
			t.Fatalf("insertEvent: %v", err)
		}
	}
	var events int
	if err := b.store.db.QueryRow(`SELECT COUNT(*) FROM events WHERE client_key = ?`, "homelab:captain").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 3 {
		t.Fatalf("want 3 events, got %d", events)
	}
}

// openStore is called on every fleet start against the same file; the schema
// statements must be idempotent.
func TestOpenStore_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	s1, err := openStore(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := s1.db.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openStore(path)
	if err != nil {
		t.Fatalf("reopening an existing store failed: %v", err)
	}
	_ = s2.db.Close()
}

func TestClose_NoStoreIsSafe(t *testing.T) {
	b := &Server{}
	if err := b.Close(); err != nil {
		t.Fatalf("Close on a store-less fleet: %v", err)
	}
}

// authorize mirrors connected captains into the store so an operator can
// replay a ship's history after it disconnects.
func TestAuthorize_MirrorsToStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestFleet(t, "")
	b.store = s
	defer s.db.Close()

	if _, authed, err := b.authorize(authorizeReq("homelab:captain", "")); err != nil || !authed {
		t.Fatalf("authorize: %v", err)
	}
	var repo string
	if err := s.db.QueryRow(`SELECT repo FROM captains WHERE client_key = ?`, "homelab:captain").Scan(&repo); err != nil {
		t.Fatalf("captain not mirrored to the store: %v", err)
	}
	if repo != "homelab" {
		t.Errorf("mirrored repo %q", repo)
	}
}

// Sanity: the embedded UI actually shipped. A missing asset only shows up as a
// blank page at runtime otherwise.
func TestEmbeddedUIAssetsPresent(t *testing.T) {
	for _, name := range []string{"ui/index.html", "ui/login.html", "ui/style.css", "ui/app.js", "ui/conversation.js"} {
		data, err := uiFS.ReadFile(name)
		if err != nil {
			t.Errorf("embedded asset %s missing: %v", name, err)
			continue
		}
		if len(bytes.TrimSpace(data)) == 0 {
			t.Errorf("embedded asset %s is empty", name)
		}
	}
}

// ---------------------------------------------------------------------------
// tell / resolve
// ---------------------------------------------------------------------------

func TestHandleTellAndResolve_ThroughTunnel(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["POST /tell/data"] = http.StatusAccepted
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", b.handleTell)
	mux.HandleFunc("POST /api/captain/{key}/resolve/{id}", b.handleResolve)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/api/captain/homelab:captain/tell/data",
		"application/json", strings.NewReader(`{"message":"/standup"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("tell: want the ship's 202 passed through, got %d", resp.StatusCode)
	}
	tells := ship.hits("POST /tell/data")
	if len(tells) != 1 || !strings.Contains(string(tells[0].body), "/standup") {
		t.Fatalf("tell body not forwarded: %+v", tells)
	}

	resp2, err := ts.Client().Post(ts.URL+"/api/captain/homelab:captain/resolve/p1",
		"application/json", strings.NewReader(`{"behavior":"allow"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp2.StatusCode)
	}
	res := ship.hits("POST /resolve/p1")
	if len(res) != 1 || !strings.Contains(string(res[0].body), "allow") {
		t.Fatalf("resolve body not forwarded: %+v", res)
	}
}

func TestHandleTell_UnknownCaptainIs404(t *testing.T) {
	b := newTestFleet(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/tell/{persona}", b.handleTell)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/captain/ghost/tell/data", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// PTY stream proxy
// ---------------------------------------------------------------------------

func TestCaptainTransport_Errors(t *testing.T) {
	b := newTestFleet(t, "")
	if _, err := b.captainTransport("ghost"); err == nil {
		t.Error("unknown captain must be an error")
	}
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Port: 1}
	_, err := b.captainTransport("homelab:captain")
	if err == nil {
		t.Fatal("a captain with no live session must be an error")
	}
	if !strings.Contains(err.Error(), "not currently connected") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestHandlePTYStreamProxy_NoTunnelIs502(t *testing.T) {
	b := newTestFleet(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/captain/ghost/pty/data/stream", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
}

func TestHandlePTYStreamProxy_PipesShipBytes(t *testing.T) {
	ship := newFakeShip(t)
	ship.bodies["GET /pty/data/stream"] = []byte("data: hello\n\ndata: world\n\n")
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/captain/homelab:captain/pty/data/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("want an SSE content type, got %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "data: hello\n\ndata: world\n\n" {
		t.Fatalf("stream bytes altered: %q", body)
	}
	if n := len(ship.hits("GET /pty/data/stream")); n != 1 {
		t.Errorf("want 1 upstream stream request, got %d", n)
	}
}

// A ship-side error on the stream endpoint must reach the operator with the
// ship's own status, not a generic 502.
func TestHandlePTYStreamProxy_PassesShipStatus(t *testing.T) {
	ship := newFakeShip(t)
	ship.status["GET /pty/data/stream"] = http.StatusNotFound
	ship.bodies["GET /pty/data/stream"] = []byte("no such pty")
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/captain/{key}/pty/{persona}/stream", b.handlePTYStreamProxy)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/captain/homelab:captain/pty/data/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want the ship's 404, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "no such pty") {
		t.Errorf("ship's error text lost: %q", body)
	}
}

// ---------------------------------------------------------------------------
// attach relay transport (proxyRaw)
// ---------------------------------------------------------------------------

// proxyRaw exists because the base proxy hard-codes application/json, which is
// wrong for a multipart relay. The caller's content type must survive.
func TestProxyRaw_PreservesContentType(t *testing.T) {
	ship := newFakeShip(t)
	ship.bodies["POST /attach"] = []byte(`{"attachId":"a1","path":"p","size":3}`)
	b := newTestFleet(t, "")
	connectShip(t, b, ship, "homelab:captain")

	body, status, err := b.proxyRaw(context.Background(), "homelab:captain", "POST", "/attach",
		"multipart/form-data; boundary=xyz", []byte("--xyz--"))
	if err != nil || status != 200 {
		t.Fatalf("proxyRaw: %d %v", status, err)
	}
	if !strings.Contains(string(body), "attachId") {
		t.Fatalf("response body: %s", body)
	}
	hits := ship.hits("POST /attach")
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	if hits[0].ctype != "multipart/form-data; boundary=xyz" {
		t.Fatalf("content type not preserved: %q", hits[0].ctype)
	}
	if string(hits[0].body) != "--xyz--" {
		t.Errorf("body mangled: %q", hits[0].body)
	}
}

func TestProxyRaw_UnknownAndDisconnected(t *testing.T) {
	b := newTestFleet(t, "")
	if _, status, err := b.proxyRaw(context.Background(), "ghost", "POST", "/attach", "text/plain", nil); status != http.StatusNotFound || err == nil {
		t.Fatalf("unknown captain: want 404 + error, got %d %v", status, err)
	}
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Port: 1}
	if _, status, err := b.proxyRaw(context.Background(), "homelab:captain", "POST", "/attach", "text/plain", nil); status != http.StatusGatewayTimeout || err == nil {
		t.Fatalf("disconnected captain: want 504 + error, got %d %v", status, err)
	}
}
