package fleetinterrupt

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
)

type failOnReadBody struct {
	read bool
}

func (b *failOnReadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("request body must not be read")
}

func (*failOnReadBody) Close() error { return nil }

func TestHTTPPostAuthenticatesBeforeAnyBodyProcessing(t *testing.T) {
	interrupt := fleetidentity.OperatorPrincipal{
		FleetID: "fleet", SubjectID: "subject", CredentialID: "interrupt",
		Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1,
		ShipIDs: []string{"ship"},
	}
	steer := interrupt
	steer.CredentialID = "steer"
	steer.Capability = fleetidentity.SteerTurnCapability
	a := credentialAuthority{principals: map[string]fleetidentity.OperatorPrincipal{
		"interrupt": interrupt,
		"steer":     steer,
	}}
	s, err := NewService("fleet", a, bytes.NewReader(make([]byte, 128)))
	if err != nil {
		t.Fatal(err)
	}
	h := HTTPHandler{Service: s}

	bodies := []struct {
		name          string
		contentLength int64
	}{
		{name: "malformed", contentLength: 1},
		{name: "oversized", contentLength: maxHTTPBody + 1},
		{name: "unknown_field", contentLength: 1},
		{name: "wrong_scope", contentLength: 1},
		{name: "valid", contentLength: 1},
	}
	credentials := []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "bad", header: "Bearer bad.secret"},
		{name: "non_interrupt", header: "Bearer steer.secret"},
	}

	for _, credential := range credentials {
		for _, bodyCase := range bodies {
			t.Run(credential.name+"/"+bodyCase.name, func(t *testing.T) {
				body := &failOnReadBody{}
				r := httptest.NewRequest(http.MethodPost, "/api/fleet/v1/turn-interrupts", body)
				r.ContentLength = bodyCase.contentLength
				r.Header.Set("Content-Type", "application/json")
				if credential.header != "" {
					r.Header.Set("Authorization", credential.header)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if body.read {
					t.Fatal("unauthenticated request body was read")
				}
				if w.Code != http.StatusUnauthorized || w.Body.String() != "{\"schema_version\":1,\"operation_id\":\"\",\"outcome\":\"refused\",\"reason_code\":\"unauthorized\",\"retry_disposition\":\"none\"}\n" {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestHTTPPostRetainsStrictValidationAfterAuthentication(t *testing.T) {
	p := fleetidentity.OperatorPrincipal{FleetID: "fleet", SubjectID: "subject", CredentialID: "interrupt", Capability: fleetidentity.InterruptTurnCapability, CredentialGeneration: 1, ShipIDs: []string{"ship"}}
	s, err := NewService("fleet", credentialAuthority{principals: map[string]fleetidentity.OperatorPrincipal{"interrupt": p}}, bytes.NewReader(make([]byte, 128)))
	if err != nil {
		t.Fatal(err)
	}
	h := HTTPHandler{Service: s}
	valid := `{"schema_version":1,"fleet_id":"fleet","fleet_epoch":1,"ship_id":"ship","connection_generation":1,"persona":"backend","interrupt_target_ref":"` + interruptRef(1) + `","operation_id":"` + interruptRef(2) + `"}`
	tests := []struct {
		name, body string
		length     int64
		want       int
	}{
		{name: "malformed", body: `{`, length: 1, want: http.StatusBadRequest},
		{name: "unknown_field", body: valid[:len(valid)-1] + `,"extra":true}`, length: int64(len(valid) + 13), want: http.StatusBadRequest},
		{name: "oversized", body: valid, length: maxHTTPBody + 1, want: http.StatusBadRequest},
		{name: "wrong_scope", body: strings.Replace(valid, `"ship_id":"ship"`, `"ship_id":"other"`, 1), length: int64(len(valid) + 1), want: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/fleet/v1/turn-interrupts", strings.NewReader(tc.body))
			r.ContentLength = tc.length
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer interrupt.secret")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

type credentialAuthority struct {
	principals map[string]fleetidentity.OperatorPrincipal
}

func (a credentialAuthority) AuthenticateOperator(id, secret, fleetID, shipID string) (fleetidentity.OperatorPrincipal, error) {
	p, err := a.AuthenticateOperatorCredential(id, secret)
	if err != nil || p.FleetID != fleetID || !principalHasShip(p, shipID) {
		return fleetidentity.OperatorPrincipal{}, errors.New("unauthorized")
	}
	return p, nil
}

func (a credentialAuthority) AuthenticateOperatorCredential(id, secret string) (fleetidentity.OperatorPrincipal, error) {
	p, ok := a.principals[id]
	if !ok || secret != "secret" {
		return fleetidentity.OperatorPrincipal{}, errors.New("unauthorized")
	}
	return p, nil
}

func (credentialAuthority) InspectOperator(string, uint64) (fleetidentity.OperatorCredentialRecord, error) {
	return fleetidentity.OperatorCredentialRecord{}, io.EOF
}
