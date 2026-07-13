package fleetsteer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/project"
)

// LocalControl is a project-scoped, loopback-only client for the running
// Shipmates server. The bearer secret is discovered only from its 0600 file.
type LocalControl struct {
	baseURL, token, scope string
	client                *http.Client
}

type localTargetsResponse struct {
	SchemaVersion uint64                          `json:"schema_version"`
	ProjectScope  string                          `json:"project_scope"`
	Targets       []livesession.RemoteSteerTarget `json:"targets"`
}

type localExactRequest struct {
	SchemaVersion uint64                          `json:"schema_version"`
	ProjectScope  string                          `json:"project_scope"`
	Operator      livesession.RemoteSteerOperator `json:"operator"`
	Request       livesession.RemoteSteerRequest  `json:"request"`
	Target        livesession.RemoteSteerTarget   `json:"target"`
}

type localInterruptExactRequest struct {
	SchemaVersion uint64                              `json:"schema_version"`
	ProjectScope  string                              `json:"project_scope"`
	Operator      livesession.RemoteInterruptOperator `json:"operator"`
	Request       livesession.RemoteInterruptRequest  `json:"request"`
	Target        livesession.RemoteInterruptTarget   `json:"target"`
}

// LocalDiscoveryError reports a closed, non-sensitive stage at which local
// server discovery failed. Callers may inspect Reason; errors never include
// paths, credentials, response bodies, or transport diagnostics.
type LocalDiscoveryReason string

const (
	DiscoveryInvalidProject      LocalDiscoveryReason = "invalid_project"
	DiscoveryMetadataUnavailable LocalDiscoveryReason = "metadata_unavailable"
	DiscoveryMetadataUnreadable  LocalDiscoveryReason = "metadata_unreadable"
	DiscoveryMetadataInvalid     LocalDiscoveryReason = "metadata_invalid"
	DiscoveryAddressInvalid      LocalDiscoveryReason = "address_invalid"
	DiscoveryAddressNotLoopback  LocalDiscoveryReason = "address_not_loopback"
	DiscoveryPIDNotLive          LocalDiscoveryReason = "pid_not_live"
	DiscoveryHealthUnreachable   LocalDiscoveryReason = "health_unreachable"
	DiscoveryHealthRejected      LocalDiscoveryReason = "health_rejected"
	DiscoveryHealthInvalid       LocalDiscoveryReason = "health_invalid"
)

type LocalDiscoveryError struct{ Reason LocalDiscoveryReason }

func (e *LocalDiscoveryError) Error() string {
	return "local_server_unavailable:" + string(e.Reason)
}

func discoveryFailure(reason LocalDiscoveryReason) error { return &LocalDiscoveryError{Reason: reason} }

// LocalTargetQueryReason is a closed, non-sensitive reason for failure after
// local server discovery has completed. It deliberately preserves only the
// request boundary that failed, never URLs, credentials, bodies, or transport
// diagnostics.
type LocalTargetQueryReason string

const (
	TargetQueryRequestConstruction LocalTargetQueryReason = "request_construction"
	TargetQueryConnection          LocalTargetQueryReason = "connection"
	TargetQueryHTTPStatus          LocalTargetQueryReason = "http_status"
	TargetQueryJSONDecode          LocalTargetQueryReason = "json_decode"
	TargetQueryScope               LocalTargetQueryReason = "scope"
	TargetQueryAuth                LocalTargetQueryReason = "auth"
)

type LocalTargetQueryError struct{ Reason LocalTargetQueryReason }

func (e *LocalTargetQueryError) Error() string {
	return "local_server_unavailable:" + string(e.Reason)
}

func targetQueryFailure(reason LocalTargetQueryReason) error {
	return &LocalTargetQueryError{Reason: reason}
}

func DiscoverLocalControl(ctx context.Context, root string) (*LocalControl, error) {
	abs, err := project.CanonicalRoot(root)
	if err != nil {
		return nil, discoveryFailure(DiscoveryInvalidProject)
	}
	scope, err := project.ScopeID(abs)
	if err != nil {
		return nil, discoveryFailure(DiscoveryInvalidProject)
	}
	b, err := project.ReadServerStateFile(abs, "server.json", 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, discoveryFailure(DiscoveryMetadataUnavailable)
		}
		return nil, discoveryFailure(DiscoveryMetadataUnreadable)
	}
	var record struct {
		SchemaVersion uint64 `json:"schema_version"`
		ProjectRoot   string `json:"project_root"`
		ProjectScope  string `json:"project_scope"`
		Address       string `json:"address"`
		PID           int    `json:"pid"`
		ControlToken  string `json:"control_token"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&record) != nil || record.SchemaVersion != 1 || record.ProjectRoot != abs || record.ProjectScope != scope || record.PID < 1 || len(record.ControlToken) < 32 || strings.ContainsAny(record.ControlToken, " \t\r\n") {
		return nil, discoveryFailure(DiscoveryMetadataInvalid)
	}
	host, port, err := net.SplitHostPort(record.Address)
	if err != nil || port == "" {
		return nil, discoveryFailure(DiscoveryAddressInvalid)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, discoveryFailure(DiscoveryAddressNotLoopback)
	}
	alive, err := project.ProcessAlive(record.PID)
	if err != nil || !alive {
		return nil, discoveryFailure(DiscoveryPIDNotLive)
	}
	c := &LocalControl{baseURL: "http://" + net.JoinHostPort(host, port), token: record.ControlToken, scope: scope, client: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, discoveryFailure(DiscoveryHealthUnreachable)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Shipmates-Project") != scope {
		return nil, discoveryFailure(DiscoveryHealthRejected)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3))
	if err != nil || string(body) != "ok" {
		return nil, discoveryFailure(DiscoveryHealthInvalid)
	}
	return c, nil
}

func (c *LocalControl) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Shipmates-Project", c.scope)
}

func (c *LocalControl) Targets(ctx context.Context) ([]livesession.RemoteSteerTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/local/v1/steer-targets", nil)
	if err != nil {
		return nil, targetQueryFailure(TargetQueryRequestConstruction)
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, targetQueryFailure(TargetQueryConnection)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, targetQueryFailure(TargetQueryAuth)
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, targetQueryFailure(TargetQueryScope)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, targetQueryFailure(TargetQueryHTTPStatus)
	}
	if resp.Header.Get("X-Shipmates-Project") != c.scope {
		return nil, targetQueryFailure(TargetQueryScope)
	}
	var out localTargetsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || out.SchemaVersion != 1 {
		return nil, targetQueryFailure(TargetQueryJSONDecode)
	}
	if out.ProjectScope != c.scope {
		return nil, targetQueryFailure(TargetQueryScope)
	}
	return out.Targets, nil
}

func (c *LocalControl) Steer(ctx context.Context, op livesession.RemoteSteerOperator, request livesession.RemoteSteerRequest, target livesession.RemoteSteerTarget) livesession.RemoteSteerResult {
	b, err := json.Marshal(localExactRequest{1, c.scope, op, request, target})
	if err != nil {
		return result(request.OperationID, "indeterminate", "internal_uncertain")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/local/v1/steer-exact", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return result(request.OperationID, "indeterminate", "delivery_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Shipmates-Project") != c.scope {
		return result(request.OperationID, "refused", "stale_target")
	}
	var out livesession.RemoteSteerResult
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || out.SchemaVersion != 1 || out.OperationID != request.OperationID {
		return result(request.OperationID, "indeterminate", "delivery_unknown")
	}
	return out
}

func (c *LocalControl) Interrupt(ctx context.Context, op livesession.RemoteInterruptOperator, request livesession.RemoteInterruptRequest, target livesession.RemoteInterruptTarget) livesession.RemoteInterruptResult {
	b, err := json.Marshal(localInterruptExactRequest{1, c.scope, op, request, target})
	if err != nil {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "internal_uncertain", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/local/v1/interrupt-exact", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "delivery_unknown", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Shipmates-Project") != c.scope {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: request.OperationID, Outcome: livesession.RemoteInterruptRefused, ReasonCode: "stale_target", RetryDisposition: livesession.RemoteInterruptFreshObservation}
	}
	var out livesession.RemoteInterruptResult
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || out.SchemaVersion != 1 || out.OperationID != request.OperationID {
		return livesession.RemoteInterruptResult{SchemaVersion: 1, OperationID: request.OperationID, Outcome: livesession.RemoteInterruptIndeterminate, ReasonCode: "delivery_unknown", RetryDisposition: livesession.RemoteInterruptReplaySameOperation}
	}
	return out
}
