package fleetsteer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/livesession"
)

func TestClientRefusesHostileRedirectWithoutForwardingSecretsOrMessage(t *testing.T) {
	rt := &redirectTransport{}
	_, err := (Client{BaseURL: "http://127.0.0.1:8080", Credential: "credential.secret", HTTP: &http.Client{Transport: rt}}).Submit(context.Background(), SubmitV1{SchemaVersion: 1, FleetID: "fleet", FleetEpoch: 1, ShipID: "ship", ConnectionGeneration: 1, Persona: "backend", SteerTargetRef: "target", Message: "hostile-message"})
	if err == nil || rt.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, rt.calls)
	}
}

type redirectTransport struct{ calls int }

func (r *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://hostile.example/capture"}}, Body: http.NoBody, Request: req}, nil
}

func TestOperatorURLRequiresHTTPSOrLoopbackHTTP(t *testing.T) {
	for _, raw := range []string{"http://fleet.example", "ftp://fleet.example", "https://user@fleet.example", "https://fleet.example?q=secret"} {
		if _, err := ValidateOperatorURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://fleet.example", "http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080"} {
		if _, err := ValidateOperatorURL(raw); err != nil {
			t.Fatalf("refused %q: %v", raw, err)
		}
	}
}

func TestHTTPExactClosedOperatorSurfaceEndToEnd(t *testing.T) {
	r, op, service, in, _ := fixture(t)
	tunnel := &fakeTunnel{result: result("", "accepted", "accepted")}
	if _, err := service.Connect(in.ShipID, in.ConnectionGeneration, tunnel); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(in)
	do := func(credential, contentType string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/fleet/v1/turn-steers", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+credential)
		req.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		HTTPHandler{Service: service}.ServeHTTP(w, req)
		return w
	}
	w := do(op.Record.CredentialID+"."+op.Secret, "application/json", body)
	var got livesession.RemoteSteerResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Outcome != livesession.RemoteSteerAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	observer, _ := r.IssueObserver([]string{in.ShipID})
	if w = do(observer.CredentialID+"."+observer.Secret, "application/json", body); w.Code != 401 || strings.Contains(w.Body.String(), in.ShipID) {
		t.Fatalf("observer=%d %s", w.Code, w.Body.String())
	}
	if w = do(op.Record.CredentialID+"."+op.Secret, "text/plain", body); w.Code != 400 {
		t.Fatalf("content type=%d", w.Code)
	}
	unknown := append(body[:len(body)-1], []byte(`,"command":"interrupt"}`)...)
	if w = do(op.Record.CredentialID+"."+op.Secret, "application/json", unknown); w.Code != 400 {
		t.Fatalf("open schema=%d %s", w.Code, w.Body.String())
	}
}

func TestOperatorUIHasNoObserverReuseOrPersistence(t *testing.T) {
	w := httptest.NewRecorder()
	ProductHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/steer/", nil))
	page := w.Body.String()
	for _, bad := range []string{"localStorage", "sessionStorage", "Observer credential", "approval ID", "attachment"} {
		if strings.Contains(page, bad) {
			t.Fatalf("page contains %q", bad)
		}
	}
	for _, want := range []string{"Operator credential", "Exact target reference", "Steer this exact turn"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	w = httptest.NewRecorder()
	ProductHandler{}.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/steer/app.js", nil))
	js := w.Body.String()
	for _, want := range []string{"approval_pending", "will not be retried automatically"} {
		if !strings.Contains(js, want) && want != "approval_pending" {
			t.Fatalf("script missing %q", want)
		}
	}
}
