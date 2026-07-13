package fleettunnel

import "testing"

func TestProductionLoopbackHostAcceptsIPv4AndIPv6(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Fatalf("loopback host %q was rejected", host)
		}
	}
	for _, host := range []string{"example.com", "192.0.2.1", ""} {
		if isLoopbackHost(host) {
			t.Fatalf("non-loopback host %q was accepted", host)
		}
	}
}
