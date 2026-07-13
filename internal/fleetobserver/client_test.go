package fleetobserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/fleetobserve"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientUsesOnlyGETHeaderCredentialAndClosedPath(t *testing.T) {
	const credential = "obs_0123456789abcdef.secret-canary"
	h := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != basePath+"/ships" {
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		if strings.Contains(r.URL.String(), credential) || r.Header.Get("Authorization") != "Bearer "+credential {
			t.Fatal("credential placement")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"schema_version":1,"fleet_id":"flt_0123456789abcdef","fleet_epoch":"epc_0123456789abcdef","snapshot_cursor":0,"ships":[],"truncated":false,"omitted_ship_count":0}`))}, nil
	})}
	c := Client{BaseURL: "https://fleet.example", Credential: credential, HTTP: h}
	if _, err := c.Ships(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceCursorUsesAuthoritativeBoundaryWhenEventsAreEmpty(t *testing.T) {
	got, err := AdvanceCursor("epc_0123456789abcdef:4", fleetobserve.ReadResult{SchemaVersion: 1, Events: []fleetobserve.FleetEventV1{}, NextCursor: 104})
	if err != nil || got != "epc_0123456789abcdef:104" {
		t.Fatalf("cursor=%q err=%v", got, err)
	}
}

func TestClientConfigurationFailsClosed(t *testing.T) {
	for _, base := range []string{"http://fleet.example", "https://user:pass@fleet.example", "https://fleet.example?q=x"} {
		c := Client{BaseURL: base, Credential: "obs_0123456789abcdef.secret"}
		if _, err := c.Ships(context.Background()); err == nil {
			t.Fatalf("accepted %q", base)
		}
	}
}

func TestBrowserIsReadOnlyAndCredentialSafe(t *testing.T) {
	b, err := uiFiles.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := uiFiles.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.ToLower(string(b) + string(js))
	for _, forbidden := range []string{"localstorage", "sessionstorage", "websocket", "method:'post'", "method:'put'", "method:'patch'", "method:'delete'", "/tell", "/steer", "/approve", "pty", "attachment", "voice"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("browser contains prohibited surface %q", forbidden)
		}
	}
	if !strings.Contains(all, "read only") {
		t.Fatal("missing read-only label")
	}
	if !strings.Contains(string(js), "cursor=`${epoch}:${r.next_cursor}`") {
		t.Fatal("browser does not advance to authoritative next_cursor")
	}
}
