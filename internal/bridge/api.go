package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Bounds on everything we read from the coordination server. The server is
// local and trusted to be the right process, but its *payloads* originate with
// agents and tool results, so every read is capped and every request has a
// deadline.
const (
	// maxJSONBody caps /status.json and /pending.json. A roster of hundreds of
	// personas with long Bash commands is a few tens of KB.
	maxJSONBody = 1 << 20 // 1 MiB

	// maxSnapshot caps /pty/{persona}/snapshot. The server's ring is 64 KiB and
	// the mode prefix a few hundred bytes; 1 MiB is generous headroom.
	maxSnapshot = 1 << 20

	// maxErrorBody caps an error response we surface to the operator.
	maxErrorBody = 4096

	// maxInputChunk caps one keystroke POST. The server itself caps at 64 KiB;
	// we are stricter because nothing legitimate approaches this.
	maxInputChunk = 8192
)

// requestTimeout applies to every short request. It deliberately does NOT apply
// to the SSE stream, which is long-lived by design.
const requestTimeout = 5 * time.Second

// ErrNoPTY is returned when an endpoint reports that the persona has no
// PTY-hosted mate (HTTP 404 from the /pty/* family). The bridge treats this as
// "not attached yet", which is a normal state, not a failure.
var ErrNoPTY = errors.New("no pty mate")

// ErrLocked is returned when another viewer holds the PTY writer lock (HTTP
// 409). The bridge claims the lock for itself whenever a mate is focused for
// typing, so a 409 that survives that is genuinely somebody else re-claiming
// mid-keystroke; it is reported in the status line with a retry key.
var ErrLocked = errors.New("another viewer holds the keyboard")

// API is a client for one ship's coordination server. All calls are to
// 127.0.0.1 on the port recorded in the ship's port file; there is no
// authentication because the server exposes none — see client.BaseURL.
type API struct {
	Base string
	// ClientID is this bridge's writer-lock identity. It must never be empty:
	// the server treats an empty ?client= as an internal write that BYPASSES
	// the single-writer lock, which would let the bridge stomp on whoever holds
	// the keyboard.
	ClientID string

	short  *http.Client
	stream *http.Client
}

// NewAPI builds a client against base with a freshly generated writer identity.
func NewAPI(base string) *API {
	return &API{
		Base:     strings.TrimRight(base, "/"),
		ClientID: newClientID(),
		short: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
				TLSHandshakeTimeout: requestTimeout,
			},
		},
		// No client-level Timeout: an SSE body stays open for the life of the
		// session. The bound that matters is on the *headers*, so a server that
		// accepts the connection and then says nothing can't hang a tab forever.
		stream: &http.Client{
			Transport: &http.Transport{
				DisableCompression:    true,
				DisableKeepAlives:     true,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
	}
}

// newClientID produces a per-process writer identity. Prefixed so a human
// reading the server log can tell a bridge apart from a browser tab.
func newClientID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal-ish, but a non-unique id only risks
		// sharing a writer lock with another bridge on the same machine.
		return "bridge-fallback"
	}
	return "bridge-" + hex.EncodeToString(b[:])
}

// MateStatus mirrors internal/server.MateStatus. Duplicated rather than
// imported so the TUI doesn't pull the whole server package (and its PTY host)
// into the client binary path.
type MateStatus struct {
	Persona   string `json:"persona"`
	Status    string `json:"status"`
	Since     string `json:"since,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Input     string `json:"input,omitempty"`
	PendingID string `json:"pending_id,omitempty"`
}

// Live reports whether the mate currently has a process the bridge can talk to.
func (m MateStatus) Live() bool {
	switch m.Status {
	case "working", "idle", "blocked":
		return true
	}
	return false
}

// Pending mirrors one row of /pending.json: a permission request blocking a
// mate until a human decides.
type Pending struct {
	ID      string `json:"id"`
	Persona string `json:"persona"`
	Tool    string `json:"tool"`
	Input   string `json:"input,omitempty"`
}

// Status fetches the machine-wide roster.
func (a *API) Status(ctx context.Context) ([]MateStatus, error) {
	var out []MateStatus
	if err := a.getJSON(ctx, "/status.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PendingList fetches permission requests awaiting a decision.
func (a *API) PendingList(ctx context.Context) ([]Pending, error) {
	var out []Pending
	if err := a.getJSON(ctx, "/pending.json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Resolve answers a pending permission request. duration, when non-empty,
// time-boxes the approval for that long (the server parses it with
// time.ParseDuration); it is only meaningful with behavior "allow".
func (a *API) Resolve(ctx context.Context, id, behavior, duration string) error {
	body := map[string]string{"behavior": behavior}
	if duration != "" {
		body["duration"] = duration
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return a.post(ctx, "/resolve/"+url.PathEscape(id), "application/json", raw)
}

// PTYStart spawns (or finds) the persona's PTY-hosted mate.
//
// Caller beware, and this is the server's semantics not ours: if the persona is
// currently a headless stream-json process, ensurePTY KILLS it and resumes the
// same session under a PTY. That drops an in-flight turn. The bridge therefore
// only calls this after either confirming the mate is idle or getting explicit
// operator consent — the same grace period the web client shows.
func (a *API) PTYStart(ctx context.Context, persona string) error {
	return a.post(ctx, a.ptyPath(persona, "start"), "application/json", nil)
}

// PTYSnapshot returns the mate's ring-buffered screen bytes, or ErrNoPTY when no
// PTY mate exists. Note this endpoint does NOT include the latched DEC private
// mode prefix — only the stream's snapshot event does — so the bridge uses this
// for an instant first paint and lets the stream's snapshot re-prime with modes.
func (a *API) PTYSnapshot(ctx context.Context, persona string) ([]byte, error) {
	req, err := a.newRequest(ctx, http.MethodGet, a.ptyPath(persona, "snapshot"), "", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.short.Do(req)
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if err := statusError(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxSnapshot))
}

// PTYInput sends keystrokes. ErrLocked means another viewer holds the writer
// lock; ErrNoPTY means the mate is gone.
func (a *API) PTYInput(ctx context.Context, persona string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInputChunk {
		data = data[:maxInputChunk]
	}
	return a.post(ctx, a.ptyPath(persona, "input"), "application/octet-stream", data)
}

// PTYResize sets the mate's terminal geometry. ErrLocked means another viewer
// holds the keyboard and their geometry wins — the bridge reflows to it rather
// than fighting, exactly as the web client does.
func (a *API) PTYResize(ctx context.Context, persona string, cols, rows int) error {
	raw, err := json.Marshal(map[string]int{"cols": cols, "rows": rows})
	if err != nil {
		return err
	}
	return a.post(ctx, a.ptyPath(persona, "resize"), "application/json", raw)
}

// PTYTakeover claims the writer lock. The server grants it unconditionally —
// there is no arbitration and no way to refuse.
//
// The bridge calls this whenever a mate is focused for typing, and the operator is
// never asked to. The lock exists to arbitrate between several browser viewers of
// the old web UI; for one operator SSH'd into a pod there is nobody on the other
// side of the argument, so "take the keyboard" was pure ceremony. The endpoint
// stays, the concept does not.
func (a *API) PTYTakeover(ctx context.Context, persona string) error {
	return a.post(ctx, a.ptyPath(persona, "takeover"), "application/json", nil)
}

// PTYRelease drops the writer lock if we hold it (the server ignores it
// otherwise). Sent when a tab is detached and for every tab on the way out, so
// the next typist claims cleanly instead of waiting out the 2-minute lease.
func (a *API) PTYRelease(ctx context.Context, persona string) error {
	return a.post(ctx, a.ptyPath(persona, "release"), "application/json", nil)
}

// StreamURL is the SSE endpoint for a persona's screen bytes.
func (a *API) StreamURL(persona string) string {
	return a.Base + a.ptyPath(persona, "stream")
}

func (a *API) ptyPath(persona, verb string) string {
	return "/pty/" + url.PathEscape(persona) + "/" + verb
}

func (a *API) getJSON(ctx context.Context, path string, out any) error {
	req, err := a.newRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	resp, err := a.short.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if err := statusError(resp); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (a *API) post(ctx context.Context, path, contentType string, body []byte) error {
	req, err := a.newRequest(ctx, http.MethodPost, path, contentType, body)
	if err != nil {
		return err
	}
	resp, err := a.short.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	return statusError(resp)
}

// newRequest builds a request with the writer-lock client id attached to every
// /pty/* call. The id is not a credential and is safe to log.
func (a *API) newRequest(ctx context.Context, method, path, contentType string, body []byte) (*http.Request, error) {
	u := a.Base + path
	if strings.HasPrefix(path, "/pty/") {
		u += "?client=" + url.QueryEscape(a.ClientID)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if contentType != "" && body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// statusError maps HTTP status to the sentinel errors the model branches on,
// bounding and sanitizing any message body before it can reach a rendered line.
func statusError(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNoPTY
	case http.StatusConflict:
		return ErrLocked
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return fmt.Errorf("server %d: %s", resp.StatusCode, ChromeLine(string(msg), 200))
}

// drainClose discards a bounded remainder of the body before closing, so the
// connection can be reused instead of being torn down mid-response.
func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, maxErrorBody))
	_ = rc.Close()
}
