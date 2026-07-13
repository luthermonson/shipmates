package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/livesession"
)

type AttachRequest struct {
	Fresh     bool   `json:"fresh"`
	SessionID string `json:"session_id,omitempty"`
	After     uint64 `json:"after,omitempty"`
}

type Transport interface {
	Attach(context.Context, string, AttachRequest) (livesession.Attach, error)
	Heartbeat(context.Context, string, string, string) error
	Sync(context.Context, string, string, string, uint64) (livesession.Attach, error)
	Action(context.Context, string, ActionRequest) (livesession.ControlResult, error)
	ResolveApproval(context.Context, string, ApprovalRequest) (livesession.ApprovalResult, error)
	Release(context.Context, string, string, string) error
}
type ApprovalRequest struct {
	ControllerID string                        `json:"controller_id"`
	SessionID    string                        `json:"session_id"`
	Authority    livesession.ApprovalAuthority `json:"authority"`
	Decision     livesession.ApprovalDecision  `json:"decision"`
}

type ActionRequest struct {
	ControllerID string                       `json:"controller_id"`
	SessionID    string                       `json:"session_id"`
	ThreadID     string                       `json:"thread_id"`
	TurnID       string                       `json:"turn_id,omitempty"`
	Action       livesession.ControllerAction `json:"action"`
	Message      string                       `json:"message,omitempty"`
	Images       []string                     `json:"images,omitempty"`
}

type HTTPTransport struct{}

func (HTTPTransport) Attach(ctx context.Context, persona string, request AttachRequest) (livesession.Attach, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/attach", request)
	if err != nil {
		return livesession.Attach{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return livesession.Attach{}, decodeError(resp.Body)
	}
	var out livesession.Attach
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.ControllerID == "" || out.SessionID == "" {
		return livesession.Attach{}, errors.New("invalid_attach_response")
	}
	return out, nil
}
func (HTTPTransport) Release(ctx context.Context, persona, sessionID, controllerID string) error {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/release", map[string]string{"session_id": sessionID, "controller_id": controllerID})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp.Body)
	}
	return nil
}
func (HTTPTransport) Heartbeat(ctx context.Context, persona, sessionID, controllerID string) error {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/heartbeat", map[string]string{"session_id": sessionID, "controller_id": controllerID})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp.Body)
	}
	return nil
}
func (HTTPTransport) Sync(ctx context.Context, persona, sessionID, controllerID string, after uint64) (livesession.Attach, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/sync", map[string]any{"session_id": sessionID, "controller_id": controllerID, "after": after})
	if err != nil {
		return livesession.Attach{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return livesession.Attach{}, decodeError(resp.Body)
	}
	var out livesession.Attach
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.ControllerID == "" {
		return out, errors.New("invalid_sync_response")
	}
	return out, nil
}
func (HTTPTransport) Action(ctx context.Context, persona string, request ActionRequest) (livesession.ControlResult, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/action", request)
	if err != nil {
		return livesession.ControlResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return livesession.ControlResult{}, decodeError(resp.Body)
	}
	var out livesession.ControlResult
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Snapshot.SessionID == "" {
		return out, errors.New("invalid_action_response")
	}
	return out, nil
}
func (HTTPTransport) ResolveApproval(ctx context.Context, persona string, request ApprovalRequest) (livesession.ApprovalResult, error) {
	resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/approval", request)
	if err != nil {
		return livesession.ApprovalResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return livesession.ApprovalResult{}, decodeError(resp.Body)
	}
	var out livesession.ApprovalResult
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || out.Outcome == "" {
		return out, errors.New("invalid_approval_response")
	}
	return out, nil
}
func decodeError(r io.Reader) error {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(r, 4096)).Decode(&v)
	if v.Code == "" {
		v.Code = "server_error"
	}
	return errors.New(v.Code)
}

// Connection makes release idempotent. Reconnect creates a new Connection and
// therefore a new lease; controller credentials are never reused.
type Connection struct {
	transport  Transport
	Persona    string
	Attach     livesession.Attach
	once       sync.Once
	releaseErr error
}

func Connect(ctx context.Context, t Transport, persona string, request AttachRequest) (*Connection, error) {
	a, err := t.Attach(ctx, persona, request)
	if err != nil {
		return nil, err
	}
	return &Connection{transport: t, Persona: persona, Attach: a}, nil
}
func (c *Connection) Close(ctx context.Context) error {
	c.once.Do(func() { c.releaseErr = c.transport.Release(ctx, c.Persona, c.Attach.SessionID, c.Attach.ControllerID) })
	return c.releaseErr
}
func (c *Connection) Heartbeat(ctx context.Context) error {
	return c.transport.Heartbeat(ctx, c.Persona, c.Attach.SessionID, c.Attach.ControllerID)
}
func (c *Connection) Sync(ctx context.Context, after uint64) (livesession.Attach, error) {
	return c.transport.Sync(ctx, c.Persona, c.Attach.SessionID, c.Attach.ControllerID, after)
}
func (c *Connection) Action(ctx context.Context, action livesession.ControllerAction, snap livesession.Snapshot, message string) (livesession.ControlResult, error) {
	return c.ActionImages(ctx, action, snap, message, nil)
}
func (c *Connection) ActionImages(ctx context.Context, action livesession.ControllerAction, snap livesession.Snapshot, message string, images []string) (livesession.ControlResult, error) {
	return c.transport.Action(ctx, c.Persona, ActionRequest{ControllerID: c.Attach.ControllerID, SessionID: snap.SessionID, ThreadID: snap.ThreadID, TurnID: snap.TurnID, Action: action, Message: message, Images: images})
}
func (c *Connection) ResolveApproval(ctx context.Context, card *livesession.ApprovalCard, decision livesession.ApprovalDecision) (livesession.ApprovalResult, error) {
	if card == nil {
		return livesession.ApprovalResult{}, errors.New("stale_approval")
	}
	return c.transport.ResolveApproval(ctx, c.Persona, ApprovalRequest{ControllerID: c.Attach.ControllerID, SessionID: c.Attach.SessionID, Authority: card.Authority, Decision: decision})
}
