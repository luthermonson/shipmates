package fleeturl

import (
	"strings"
	"testing"
)

// The rule this package exists for: a bearer token and the Admiral's deny list
// must never travel in cleartext to a host that isn't this machine.
func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string // substring; empty means the URL must be accepted
	}{
		// Encrypted transport is always fine.
		{name: "https", in: "https://fleet.example.com"},
		{name: "https with port and path", in: "https://fleet.example.com:8443/api"},
		{name: "wss", in: "wss://fleet.example.com/connect"},

		// Plaintext is fine only when the host is this machine.
		{name: "http loopback v4", in: "http://127.0.0.1:8443"},
		{name: "http loopback other v4", in: "http://127.0.0.2:8443"},
		{name: "http loopback v6", in: "http://[::1]:8443"},
		{name: "http localhost", in: "http://localhost:8443"},
		{name: "http LOCALHOST is case insensitive", in: "http://LOCALHOST:8443"},
		{name: "ws loopback", in: "ws://127.0.0.1:8443/connect"},
		{name: "whitespace is trimmed", in: "  https://fleet.example.com  "},

		// …and refused everywhere else.
		{name: "http to a dns host", in: "http://fleet.example.com", wantErr: "plaintext"},
		{name: "http to a LAN address", in: "http://192.168.1.10:8443", wantErr: "plaintext"},
		{name: "http to a public address", in: "http://203.0.113.9", wantErr: "plaintext"},
		{name: "ws to a dns host", in: "ws://fleet.example.com/connect", wantErr: "plaintext"},
		// A hostname that merely looks loopback is not loopback. This is the
		// classic bypass: 127.0.0.1.evil.com resolves to whatever the attacker
		// wants.
		{name: "a host that merely starts with 127", in: "http://127.0.0.1.evil.com:8443", wantErr: "plaintext"},
		{name: "localhost as a subdomain prefix", in: "http://localhost.evil.com", wantErr: "plaintext"},

		// Shape errors.
		{name: "empty", in: "", wantErr: "empty"},
		{name: "whitespace only", in: "   ", wantErr: "empty"},
		{name: "no scheme", in: "fleet.example.com:8443", wantErr: "scheme"},
		{name: "unsupported scheme", in: "ftp://fleet.example.com", wantErr: "unsupported"},
		{name: "no host", in: "https:///api", wantErr: "no host"},
		{name: "embedded credentials", in: "https://user:pw@fleet.example.com", wantErr: "credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := Validate(tc.in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate(%q) = %v, want it accepted", tc.in, err)
				}
				if u == nil {
					t.Fatalf("Validate(%q) returned a nil URL with no error", tc.in)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%q) was accepted, want an error mentioning %q", tc.in, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate(%q) = %v, want it to mention %q", tc.in, err, tc.wantErr)
			}
		})
	}
}

// The refusal has to tell the operator what to type instead, and must not echo
// a password back into a log line.
func TestValidateErrorIsActionable(t *testing.T) {
	_, err := Validate("http://fleet.example.com")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v, want it to name https as the fix", err)
	}

	_, err = Validate("ws://fleet.example.com/connect")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "wss") {
		t.Errorf("err = %v, want it to name wss as the fix for a ws url", err)
	}

	// url.Redacted() keeps the password out of the message.
	_, err = Validate("http://user:hunter2@fleet.example.com")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v leaks the password", err)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LocalHost", "localhost.localdomain", "127.0.0.1", "127.1.2.3", "::1", " 127.0.0.1 "} {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "fleet.example.com", "127.0.0.1.evil.com", "localhost.evil.com", "10.0.0.1", "0.0.0.0", "169.254.169.254"} {
		if IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = true, want false", host)
		}
	}
}
