package fleet

// Credentials for a captain's own local HTTP API.
//
// The captain's API on 127.0.0.1 requires a per-run bearer token. The fleet
// usually runs on another host and cannot read the captain's token file, so
// the captain hands the token over on the tunnel connect
// (X-Shipmates-API-Token) and the fleet replays it on every proxied request.
// The token lives only in memory, is never persisted to the store, and never
// renders into an API response (see Captain.APIToken).

// sanitizeAPIToken bounds what a connecting captain can put in the token
// header. The proxy writes request lines by hand, so a token containing CR or
// LF would let a hostile "captain" inject headers — or a second request —
// into the tunneled stream. Tokens we mint are hex, so anything outside
// [A-Za-z0-9] is dropped whole rather than escaped.
func sanitizeAPIToken(raw string) string {
	if len(raw) < 16 || len(raw) > 256 {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			return ""
		}
	}
	return raw
}

// authHeaderLine renders one HTTP/1.1 header line (with its CRLF) carrying
// the captain's credential, or "" when we hold no token — in which case the
// captain will answer 401 and the operator sees an auth failure rather than a
// silent no-op.
func authHeaderLine(token string) string {
	if token == "" {
		return ""
	}
	return "Authorization: Bearer " + token + "\r\n"
}

// captainAPIToken looks up a connected captain's API credential.
func (b *Server) captainAPIToken(clientKey string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.captains[clientKey]; ok {
		return c.APIToken
	}
	return ""
}

// captainDialInfo snapshots the port and credential of a connected captain
// under the lock. Callers must not reach through the *Captain afterwards:
// authorize rewrites those fields in place when a ship reconnects, so a read
// through the pointer after unlocking is a data race.
func (b *Server) captainDialInfo(clientKey string) (port int, token string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.captains[clientKey]
	if !ok {
		return 0, "", false
	}
	return c.Port, c.APIToken, true
}
