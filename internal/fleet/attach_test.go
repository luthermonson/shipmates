package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newFleetForAttachTest returns a Server with just the fields the attach
// handler touches populated. The real dialer/store are nil — the test hooks
// (attachRelay / attachTell) intercept every tunnel call so no websocket is
// required.
func newFleetForAttachTest() *Server {
	return &Server{
		captains: map[string]*Captain{},
	}
}

// buildFleetAttachRequest builds the operator's multipart upload aimed at
// Fleet Command. Same shape as the ship-side test builder, kept local so
// the two packages don't depend on each other's test helpers.
func buildFleetAttachRequest(t *testing.T, filename, contentType string, payload []byte, caption string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	hdr := map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
	}
	if contentType != "" {
		hdr["Content-Type"] = []string{contentType}
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf, mw.FormDataContentType()
}

// captureCall records the (path, method, contentType, body) that the hook
// received, so tests can assert what the fleet forwarded to the ship.
type capturedCall struct {
	path        string
	method      string
	contentType string
	body        []byte
}

func TestFleetAttachHappyPath(t *testing.T) {
	b := newFleetForAttachTest()
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Persona: "captain", Port: 1234}

	var mu sync.Mutex
	var relayCalls []capturedCall
	var tellCalls []capturedCall

	b.attachRelay = func(ctx context.Context, clientKey, method, path, contentType string, body []byte) ([]byte, int, error) {
		mu.Lock()
		relayCalls = append(relayCalls, capturedCall{path: path, method: method, contentType: contentType, body: append([]byte(nil), body...)})
		mu.Unlock()
		resp := AttachResponse{AttachID: "abc12345", Path: ".shipmates/inbox/abc12345.png", Size: int64(len(body))}
		out, _ := json.Marshal(resp)
		return out, http.StatusOK, nil
	}
	b.attachTell = func(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error) {
		mu.Lock()
		tellCalls = append(tellCalls, capturedCall{path: path, method: method, body: append([]byte(nil), body...)})
		mu.Unlock()
		return []byte(""), http.StatusAccepted, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, ctype := buildFleetAttachRequest(t, "shot.png", "image/png",
		[]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'x', 'y'},
		"look at this")
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/homelab:captain/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, msg)
	}

	var out AttachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AttachID != "abc12345" || out.Path != ".shipmates/inbox/abc12345.png" {
		t.Fatalf("unexpected response: %+v", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(relayCalls) != 1 {
		t.Fatalf("want 1 relay call, got %d", len(relayCalls))
	}
	rc := relayCalls[0]
	if rc.path != "/attach" || rc.method != "POST" {
		t.Errorf("relay path/method wrong: %+v", rc)
	}
	if !strings.HasPrefix(rc.contentType, "multipart/form-data") {
		t.Errorf("relay content-type should be multipart, got %q", rc.contentType)
	}
	if len(tellCalls) != 1 {
		t.Fatalf("want 1 auto-tell, got %d", len(tellCalls))
	}
	tc := tellCalls[0]
	if tc.path != "/tell/captain" {
		t.Errorf("auto-tell path wrong: %q", tc.path)
	}
	var tellBody struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(tc.body, &tellBody); err != nil {
		t.Fatalf("decode tell body: %v", err)
	}
	if !strings.Contains(tellBody.Message, "[attachment]") ||
		!strings.Contains(tellBody.Message, ".shipmates/inbox/abc12345.png") ||
		!strings.Contains(tellBody.Message, "look at this") {
		t.Errorf("auto-tell message missing pieces: %q", tellBody.Message)
	}
}

func TestFleetAttachUnknownCaptain(t *testing.T) {
	b := newFleetForAttachTest() // empty captains map

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, ctype := buildFleetAttachRequest(t, "shot.png", "image/png", []byte("payload"), "")
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/ghost/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var errObj struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errObj)
	if !strings.Contains(errObj.Error, "unknown captain") {
		t.Errorf("expected 'unknown captain' error, got %q", errObj.Error)
	}
}

func TestFleetAttachShipUnreachable(t *testing.T) {
	b := newFleetForAttachTest()
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Persona: "captain"}

	// attachRelay simulates a dial failure — matches what proxyRaw returns
	// when the tunnel is gone. Expect the handler to translate that into a
	// 502 "ship unreachable" response.
	b.attachRelay = func(ctx context.Context, clientKey, method, path, contentType string, body []byte) ([]byte, int, error) {
		return nil, http.StatusBadGateway, fmt.Errorf("dial captain: closed")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, ctype := buildFleetAttachRequest(t, "shot.png", "image/png", []byte("payload"), "")
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/homelab:captain/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", resp.StatusCode)
	}
	var errObj struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&errObj)
	if !strings.Contains(errObj.Error, "ship unreachable") {
		t.Errorf("expected 'ship unreachable', got %q", errObj.Error)
	}
}

func TestFleetAttachTooLarge(t *testing.T) {
	b := newFleetForAttachTest()
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Persona: "captain"}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	big := bytes.Repeat([]byte{0xFF}, int(attachMaxBytes)+1)
	body, ctype := buildFleetAttachRequest(t, "big.png", "image/png", big, "")
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/homelab:captain/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestFleetAttachMissingFile(t *testing.T) {
	b := newFleetForAttachTest()
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Persona: "captain"}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("caption", "no file here")
	_ = mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/homelab:captain/attach", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestFleetAttachShipRejects(t *testing.T) {
	// The ship returned 400 (e.g. wrong content type). The fleet must pass
	// that through unchanged; the auto-tell must NOT fire.
	b := newFleetForAttachTest()
	b.captains["homelab:captain"] = &Captain{ClientKey: "homelab:captain", Persona: "captain"}

	b.attachRelay = func(ctx context.Context, clientKey, method, path, contentType string, body []byte) ([]byte, int, error) {
		return []byte(`{"error":"unsupported attachment type"}`), http.StatusBadRequest, nil
	}
	var tellFired bool
	b.attachTell = func(ctx context.Context, clientKey, method, path string, body []byte) ([]byte, int, error) {
		tellFired = true
		return nil, http.StatusOK, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/captain/{key}/attach", b.handleCaptainAttach)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, ctype := buildFleetAttachRequest(t, "shot.png", "image/png", []byte("payload"), "")
	req, _ := http.NewRequest("POST", ts.URL+"/api/captain/homelab:captain/attach", body)
	req.Header.Set("Content-Type", ctype)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 passthrough, got %d", resp.StatusCode)
	}
	if tellFired {
		t.Fatalf("auto-tell must not fire when ship rejected upload")
	}
}
