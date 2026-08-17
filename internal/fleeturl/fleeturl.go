// Package fleeturl validates the URL a ship — or an operator's CLI — uses to
// reach Fleet Command.
//
// Everything that travels on that URL is sensitive in both directions. Going
// up it carries the fleet's shared secret in an `Authorization: Bearer`
// header, the ship's own per-run API token in the tunnel connect headers, and
// operator commands that inject prompts into live agents. Coming down it
// carries `/api/fleet-policy` — the Admiral's unconditional deny list, the one
// permission layer no ship-side rule can override.
//
// On plaintext http/ws to a remote host, both halves are lost: anyone on the
// path reads the token, and a man in the middle can serve `{"deny":[]}` and
// silently switch the fleet-wide floor off. So plaintext is refused unless the
// host is loopback, where the "network" is the machine itself and https would
// only buy a certificate dance for local development.
//
// This mirrors the rule the openai runtime already applies to its base_url
// (internal/runtime/openai/config.go) — one rule, two call sites, same shape.
package fleeturl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Validate parses raw and returns it as a *url.URL, refusing anything that
// would put fleet traffic on the wire in cleartext.
//
// Accepted schemes are http, https, ws and wss. http/ws are accepted only when
// the host is loopback (127.0.0.1, ::1, localhost) — the local-development
// case. Credentials embedded in the URL are refused outright: they would be
// logged by every proxy on the way and are not how the fleet token is passed.
func Validate(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("fleet url is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("fleet url is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https", "wss":
	case "http", "ws":
		if !IsLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("fleet url %q is plaintext %s to a non-loopback host; the fleet token and the fleet deny list must not travel in cleartext — use %s", u.Redacted(), u.Scheme, secureScheme(u.Scheme))
		}
	case "":
		return nil, fmt.Errorf("fleet url %q has no scheme (use https://host:port)", raw)
	default:
		return nil, fmt.Errorf("fleet url scheme %q unsupported (use http, https, ws, or wss)", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("fleet url %q has no host", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("fleet url must not embed credentials; use $SHIPMATES_FLEET_TOKEN or --token-file")
	}
	return u, nil
}

// secureScheme names the encrypted counterpart of a plaintext scheme, so the
// error tells the operator exactly what to type instead.
func secureScheme(plaintext string) string {
	if plaintext == "ws" {
		return "wss"
	}
	return "https"
}

// IsLoopbackHost reports whether host refers to this machine. Names are
// matched literally — a DNS lookup would let a hostile resolver decide what
// counts as loopback, which is the opposite of what this guard is for.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
