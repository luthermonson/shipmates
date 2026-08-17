package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/personaname"
)

// AttachResponse is the JSON body Fleet Command returns to the UI on success.
// Kept aligned with the ship's response shape — the fleet is a pass-through.
type AttachResponse struct {
	AttachID string `json:"attachId"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// attachMaxBytes / attachParseLimit / attachCaptionMaxBytes mirror the ship's
// bounds so a payload that would fail on the ship can be rejected at the edge
// with the correct error class (400) instead of a confusing 502 upstream.
const (
	attachMaxBytes        = int64(10 << 20)
	attachParseLimit      = int64(11 << 20)
	attachCaptionMaxBytes = 2 << 10
)

// handleCaptainAttach is the Fleet Command edge for `POST
// /api/captain/{key}/attach`. It parses the operator's multipart upload, forwards
// it verbatim to the target ship's /attach endpoint over the remotedialer
// tunnel, then — on the ship's 200 — synthesizes an auto-tell to the captain
// persona so the mate sees the drop immediately.
func (b *Server) handleCaptainAttach(w http.ResponseWriter, r *http.Request) {
	clientKey := r.PathValue("key")

	// Snapshot the one field this handler needs while holding the lock;
	// authorize rewrites Captain in place on reconnect, so keeping the
	// pointer and reading through it later would race that write.
	b.mu.Lock()
	captain, known := b.captains[clientKey]
	var captainPersona string
	if known {
		captainPersona = captain.Persona
	}
	b.mu.Unlock()
	if !known {
		writeFleetAttachError(w, http.StatusNotFound, "unknown captain")
		return
	}

	// Cap the raw body first — a client streaming a huge upload shouldn't
	// get to spill to disk on the fleet host before the size check runs.
	r.Body = http.MaxBytesReader(w, r.Body, attachParseLimit)
	if err := r.ParseMultipartForm(attachParseLimit); err != nil {
		if _, ok := err.(*http.MaxBytesError); ok {
			writeFleetAttachError(w, http.StatusBadRequest, "attachment exceeds 10 MB limit")
			return
		}
		writeFleetAttachError(w, http.StatusBadRequest, "bad multipart body: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeFleetAttachError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	if header.Size > attachMaxBytes {
		writeFleetAttachError(w, http.StatusBadRequest, "attachment exceeds 10 MB limit")
		return
	}

	caption := strings.TrimSpace(r.FormValue("caption"))
	if len(caption) > attachCaptionMaxBytes {
		writeFleetAttachError(w, http.StatusBadRequest, "caption exceeds 2 KB limit")
		return
	}

	// Re-encode as a fresh multipart body destined for the ship. We can't
	// just tee the operator's original body: parsing consumed it, and the
	// tunnel wants a single Content-Length up front. Buffering to memory
	// is fine at the 10 MB cap; a future PR could stream if we lift the cap.
	relayBody := &bytes.Buffer{}
	mw := multipart.NewWriter(relayBody)
	part, err := mw.CreatePart(header.Header)
	if err != nil {
		writeFleetAttachError(w, http.StatusInternalServerError, "build relay part: "+err.Error())
		return
	}
	if _, err := io.Copy(part, io.LimitReader(file, attachMaxBytes+1)); err != nil {
		writeFleetAttachError(w, http.StatusBadGateway, "read upload: "+err.Error())
		return
	}
	if err := mw.Close(); err != nil {
		writeFleetAttachError(w, http.StatusInternalServerError, "close relay body: "+err.Error())
		return
	}

	relay := b.attachRelay
	if relay == nil {
		relay = b.proxyRaw
	}
	respBody, status, err := relay(r.Context(), clientKey, "POST", "/attach", mw.FormDataContentType(), relayBody.Bytes())
	if err != nil {
		if status == http.StatusGatewayTimeout || status == http.StatusBadGateway {
			writeFleetAttachError(w, http.StatusBadGateway, "ship unreachable")
			return
		}
		writeFleetAttachError(w, status, err.Error())
		return
	}
	if status < 200 || status >= 300 {
		// Pass ship-side error status through, but re-wrap so the shape
		// stays JSON with an "error" key even if the ship returned plain
		// text.
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = "ship rejected upload"
		}
		writeFleetAttachError(w, status, msg)
		return
	}

	var parsed AttachResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		writeFleetAttachError(w, http.StatusBadGateway, "malformed ship response: "+err.Error())
		return
	}

	// Auto-tell the captain persona so the mate discovers the file without
	// the operator needing to type. Delivered on the same tunnel via
	// /tell/<persona>; failures are logged but don't fail the upload —
	// the file already landed and the UI event stream will surface it.
	b.fireAttachAutoTell(r.Context(), clientKey, captainPersona, parsed.Path, caption)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(parsed)
}

// fireAttachAutoTell synthesizes the "[attachment] Admiral sent ..." message
// and delivers it to the captain persona via the ship's /tell endpoint. One
// place, one format — the ship never has to know a tell was auto-generated.
func (b *Server) fireAttachAutoTell(ctx context.Context, clientKey, persona, path, caption string) {
	if strings.TrimSpace(persona) == "" {
		persona = "captain"
	}
	// persona comes off the ship's X-Shipmates-Persona connect header, which
	// is remote input: unvalidated it would frame the proxied request line.
	// The upload already landed, so a junk persona costs the auto-tell only.
	if !personaname.Valid(persona) {
		slog.Warn("attach auto-tell skipped: invalid persona", "captain", clientKey, "persona", persona)
		return
	}
	msg := fmt.Sprintf("[attachment] Admiral sent an image at `%s`", path)
	if caption != "" {
		msg += " — " + caption
	}
	payload, _ := json.Marshal(map[string]string{"message": msg})
	tell := b.attachTell
	if tell == nil {
		tell = b.proxy
	}
	body, status, err := tell(ctx, clientKey, "POST", "/tell/"+url.PathEscape(persona), payload)
	if err != nil || status >= 300 {
		slog.Warn("attach auto-tell failed", "captain", clientKey, "persona", persona, "status", status, "err", err, "body", string(body))
	}
}

// proxyRaw is proxy() with a caller-controlled Content-Type header and body
// bytes. The base proxy hard-codes application/json, which is wrong for a
// multipart relay. Kept in this file so attach owns its transport quirk
// instead of complicating the general proxy path other endpoints rely on.
func (b *Server) proxyRaw(ctx context.Context, clientKey, method, path, contentType string, body []byte) ([]byte, int, error) {
	if err := checkProxyPath(path); err != nil {
		return nil, http.StatusBadRequest, err
	}
	// Snapshot under the lock; see captainDialInfo — reaching through the
	// *Captain after unlocking races authorize's in-place reconnect writes.
	port, token, ok := b.captainDialInfo(clientKey)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("no such captain: %s", clientKey)
	}
	if err := checkDialPort(port, clientKey); err != nil {
		return nil, http.StatusBadGateway, err
	}
	if !b.dialer.HasSession(clientKey) {
		return nil, http.StatusGatewayTimeout, fmt.Errorf("captain %s not currently connected", clientKey)
	}
	dial := b.dialer.Dialer(clientKey)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("dial captain: %w", err)
	}
	defer conn.Close()
	// Generous deadline: a 10 MB body over the tunnel plus the ship's
	// disk-write step can eat more than the 30s the JSON proxy uses.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))

	req := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: captain\r\n%sContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		method, path, authHeaderLine(token), contentType, len(body))
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, http.StatusBadGateway, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			return nil, http.StatusBadGateway, err
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	return respBody, resp.StatusCode, nil
}

// writeFleetAttachError writes a JSON {"error": "..."} body with the given
// HTTP status — the shape the UI expects on every attach failure path.
func writeFleetAttachError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
