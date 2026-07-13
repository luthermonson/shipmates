package fleetinterrupt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/livesession"
)

const maxHTTPBody = 8 << 10

// HTTPHandler is the sole public M9 mutation boundary.
type HTTPHandler struct{ Service *Service }

func (h HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secure(w)
	if !sameOrigin(r) {
		writeHTTP(w, http.StatusForbidden, result("", livesession.RemoteInterruptRefused, "unauthorized"))
		return
	}
	if r.URL.Path == "/api/fleet/v1/interrupt-targets" {
		h.serveTargets(w, r)
		return
	}
	if r.URL.Path != "/api/fleet/v1/turn-interrupts" && !strings.HasPrefix(r.URL.Path, "/api/fleet/v1/turn-interrupts/") {
		writeHTTP(w, http.StatusNotFound, map[string]any{"schema_version": 1, "code": "not_found"})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeHTTP(w, http.StatusMethodNotAllowed, map[string]any{"schema_version": 1, "code": "method_not_allowed"})
		return
	}
	id, secret, ok := operatorBearer(r.Header.Get("Authorization"))
	if !ok {
		writeHTTP(w, http.StatusUnauthorized, result("", livesession.RemoteInterruptRefused, "unauthorized"))
		return
	}
	// Authenticate before parsing a target, operation ID, or other scope fact.
	if r.Method == http.MethodGet {
		op := strings.TrimPrefix(r.URL.Path, "/api/fleet/v1/turn-interrupts/")
		if h.Service == nil || r.URL.RawQuery != "" || !validOperationID(op) {
			writeHTTP(w, http.StatusUnauthorized, result("", livesession.RemoteInterruptRefused, "unauthorized"))
			return
		}
		// A dummy authentication cannot reveal whether the operation exists.
		res, found := h.Service.QueryAuthorized(id, secret, op)
		if !found {
			writeHTTP(w, http.StatusUnauthorized, result("", livesession.RemoteInterruptRefused, "unauthorized"))
			return
		}
		writeHTTP(w, http.StatusOK, res)
		return
	}
	if h.Service == nil {
		writeHTTP(w, http.StatusUnauthorized, result("", livesession.RemoteInterruptRefused, "unauthorized"))
		return
	}
	principal, err := h.Service.Authenticate(id, secret)
	if err != nil {
		writeHTTP(w, http.StatusUnauthorized, result("", livesession.RemoteInterruptRefused, "unauthorized"))
		return
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-HTTP-Method-Override") != "" || r.ContentLength < 0 || r.ContentLength > maxHTTPBody {
		writeHTTP(w, http.StatusBadRequest, result("", livesession.RemoteInterruptRefused, "invalid_request"))
		return
	}
	var in SubmitV1
	d := json.NewDecoder(io.LimitReader(r.Body, maxHTTPBody+1))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(&struct{}{}) != io.EOF {
		writeHTTP(w, http.StatusBadRequest, result("", livesession.RemoteInterruptRefused, "invalid_request"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), TotalDeadline)
	defer cancel()
	res := h.Service.SubmitAuthorized(ctx, principal, in)
	status := http.StatusOK
	if res.ReasonCode == "unauthorized" {
		status = http.StatusUnauthorized
	}
	writeHTTP(w, status, res)
}

func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.User == nil && u.Host == r.Host && (u.Scheme == "https" || loopbackHTTP(u))
}

func (h HTTPHandler) serveTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.ContentLength != 0 || r.Header.Get("Content-Type") != "" || r.Header.Get("X-HTTP-Method-Override") != "" {
		writeHTTP(w, http.StatusBadRequest, TargetsV1{SchemaVersion: 1, Targets: []InterruptTargetV1{}})
		return
	}
	id, secret, ok := operatorBearer(r.Header.Get("Authorization"))
	if !ok || h.Service == nil {
		writeHTTP(w, http.StatusUnauthorized, TargetsV1{SchemaVersion: 1, Targets: []InterruptTargetV1{}})
		return
	}
	p, err := h.Service.Authenticate(id, secret)
	if err != nil {
		writeHTTP(w, http.StatusUnauthorized, TargetsV1{SchemaVersion: 1, Targets: []InterruptTargetV1{}})
		return
	}
	epoch := r.URL.Query().Get("observation_epoch")
	raw := r.URL.Query().Get("observation_cursor")
	if len(r.URL.Query()) != 0 && (len(r.URL.Query()) != 2 || epoch == "" || raw == "") {
		writeHTTP(w, http.StatusBadRequest, TargetsV1{SchemaVersion: 1, Targets: []InterruptTargetV1{}})
		return
	}
	var cursor uint64
	if raw != "" {
		cursor, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeHTTP(w, http.StatusBadRequest, TargetsV1{SchemaVersion: 1, Targets: []InterruptTargetV1{}})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), DeliveryDeadline)
	defer cancel()
	writeHTTP(w, http.StatusOK, h.Service.Targets(ctx, p, epoch, cursor))
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

// Targets returns the current interrupt-authorized projection. It is the
// compiled client boundary used by operators before confirming an exact turn.
func (c Client) Targets(ctx context.Context) (TargetsV1, error) {
	base, err := ValidateOperatorURL(c.BaseURL)
	if err != nil {
		return TargetsV1{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base.String(), "/")+"/api/fleet/v1/interrupt-targets", nil)
	if err != nil {
		return TargetsV1{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DeliveryDeadline}
	}
	clone := *hc
	clone.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		req.Header.Del("Authorization")
		return errors.New("redirect_refused")
	}
	resp, err := clone.Do(req)
	if err != nil {
		return TargetsV1{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TargetsV1{}, errors.New("target_discovery_refused")
	}
	var out TargetsV1
	d := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.SchemaVersion != 1 || out.Targets == nil {
		return TargetsV1{}, errors.New("invalid_fleet_response")
	}
	return out, nil
}

func (c Client) Submit(ctx context.Context, in SubmitV1) (livesession.RemoteInterruptResult, error) {
	base, err := ValidateOperatorURL(c.BaseURL)
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base.String(), "/")+"/api/fleet/v1/turn-interrupts", bytes.NewReader(b))
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: TotalDeadline}
	}
	clone := *hc
	clone.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		req.Header.Del("Authorization")
		req.Body = nil
		return errors.New("redirect_refused")
	}
	resp, err := clone.Do(req)
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	defer resp.Body.Close()
	var out livesession.RemoteInterruptResult
	d := json.NewDecoder(io.LimitReader(resp.Body, 2049))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.SchemaVersion != 1 || out.Outcome == "" {
		return livesession.RemoteInterruptResult{}, errors.New("invalid_fleet_response")
	}
	return out, nil
}

// Query returns the durable result for an exact caller-owned operation ID.
// The same interrupt credential that submitted the operation must authorize it.
func (c Client) Query(ctx context.Context, operationID string) (livesession.RemoteInterruptResult, error) {
	if !validOperationID(operationID) {
		return livesession.RemoteInterruptResult{}, errors.New("invalid_operation_id")
	}
	base, err := ValidateOperatorURL(c.BaseURL)
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base.String(), "/")+"/api/fleet/v1/turn-interrupts/"+operationID, nil)
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: TotalDeadline}
	}
	clone := *hc
	clone.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		req.Header.Del("Authorization")
		return errors.New("redirect_refused")
	}
	resp, err := clone.Do(req)
	if err != nil {
		return livesession.RemoteInterruptResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return livesession.RemoteInterruptResult{}, errors.New("operation_query_refused")
	}
	var out livesession.RemoteInterruptResult
	d := json.NewDecoder(io.LimitReader(resp.Body, 2049))
	d.DisallowUnknownFields()
	if d.Decode(&out) != nil || out.SchemaVersion != 1 || out.OperationID != operationID || out.Outcome == "" {
		return livesession.RemoteInterruptResult{}, errors.New("invalid_fleet_response")
	}
	return out, nil
}

func ValidateOperatorURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !loopbackHTTP(u)) {
		return nil, errors.New("invalid_fleet_configuration")
	}
	return u, nil
}
func loopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}
