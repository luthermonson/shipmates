package fleetsteer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/luthermonson/shipmates/internal/livesession"
)

const maxHTTPBody = 8 << 10

// HTTPHandler is the sole public M8 mutation surface. It deliberately accepts
// operator bearer credentials only and has no observer-service fallback.
type HTTPHandler struct{ Service *Service }

func (h HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secure(w)
	if r.URL.Path != "/api/fleet/v1/turn-steers" {
		writeHTTP(w, http.StatusNotFound, errorV1{1, "not_found"})
		return
	}
	if r.Method != http.MethodPost {
		writeHTTP(w, http.StatusMethodNotAllowed, errorV1{1, "method_not_allowed"})
		return
	}
	if h.Service == nil || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-HTTP-Method-Override") != "" || r.ContentLength < 0 || r.ContentLength > maxHTTPBody {
		writeHTTP(w, http.StatusBadRequest, result("", "refused", "invalid_request"))
		return
	}
	id, secret, ok := operatorBearer(r.Header.Get("Authorization"))
	if !ok {
		writeHTTP(w, http.StatusUnauthorized, result("", "refused", "unauthorized"))
		return
	}
	var in SubmitV1
	d := json.NewDecoder(io.LimitReader(r.Body, maxHTTPBody+1))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(&struct{}{}) != io.EOF {
		writeHTTP(w, http.StatusBadRequest, result("", "refused", "invalid_request"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TotalDeadline)
	defer cancel()
	res := h.Service.Submit(ctx, id, secret, in)
	status := http.StatusOK
	if res.ReasonCode == "unauthorized" {
		status = http.StatusUnauthorized
	}
	writeHTTP(w, status, res)
}

func operatorBearer(h string) (string, string, bool) {
	if len(h) < 8 || len(h) > 512 || !strings.HasPrefix(h, "Bearer ") {
		return "", "", false
	}
	v := strings.TrimPrefix(h, "Bearer ")
	if strings.ContainsAny(v, " \t\r\n") || strings.Count(v, ".") != 1 {
		return "", "", false
	}
	id, secret, _ := strings.Cut(v, ".")
	return id, secret, id != "" && secret != ""
}

type errorV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Code          string `json:"code"`
}

func secure(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}
func writeHTTP(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type Client struct {
	BaseURL, Credential string
	HTTP                *http.Client
}

func (c Client) Submit(ctx context.Context, in SubmitV1) (livesession.RemoteSteerResult, error) {
	base, err := ValidateOperatorURL(c.BaseURL)
	if err != nil {
		return livesession.RemoteSteerResult{}, err
	}
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base.String(), "/")+"/api/fleet/v1/turn-steers", bytes.NewReader(b))
	if err != nil {
		return livesession.RemoteSteerResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: TotalDeadline}
	}
	// Never allow a bearer credential or steer message to cross an HTTP
	// redirect, including when the caller supplied its own client.
	clone := *hc
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.Header.Del("Authorization")
		req.Body = nil
		return errors.New("redirect_refused")
	}
	resp, err := clone.Do(req)
	if err != nil {
		return livesession.RemoteSteerResult{}, err
	}
	defer resp.Body.Close()
	var out livesession.RemoteSteerResult
	d := json.NewDecoder(io.LimitReader(resp.Body, 2049))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.SchemaVersion != 1 || out.Outcome == "" {
		return livesession.RemoteSteerResult{}, errors.New("invalid_fleet_response")
	}
	return out, nil
}

// ValidateOperatorURL must be called before loading an operator credential.
// Only encrypted remote transport and explicitly local loopback development
// transport are permitted.
func ValidateOperatorURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !operatorLoopbackHTTP(u)) {
		return nil, errors.New("invalid_fleet_configuration")
	}
	return u, nil
}

func operatorLoopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}
