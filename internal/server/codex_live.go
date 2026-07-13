package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/turninput"
)

type localSteerTargetsResponse struct {
	SchemaVersion uint64                          `json:"schema_version"`
	ProjectScope  string                          `json:"project_scope"`
	Targets       []livesession.RemoteSteerTarget `json:"targets"`
}

type localSteerExactRequest struct {
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

type liveStartRequest struct {
	Prompt string   `json:"prompt"`
	Fresh  bool     `json:"fresh"`
	Images []string `json:"images,omitempty"`
}
type liveTargetRequest struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
	TurnID    string `json:"turn_id"`
	Message   string `json:"message,omitempty"`
}
type liveAttachRequest struct {
	Fresh     bool   `json:"fresh"`
	SessionID string `json:"session_id,omitempty"`
	After     uint64 `json:"after,omitempty"`
}
type liveReleaseRequest struct {
	SessionID    string `json:"session_id"`
	ControllerID string `json:"controller_id"`
}
type liveSyncRequest struct {
	SessionID    string `json:"session_id"`
	ControllerID string `json:"controller_id"`
	After        uint64 `json:"after"`
}
type liveControllerActionRequest struct {
	ControllerID string                       `json:"controller_id"`
	SessionID    string                       `json:"session_id"`
	ThreadID     string                       `json:"thread_id"`
	TurnID       string                       `json:"turn_id,omitempty"`
	Action       livesession.ControllerAction `json:"action"`
	Message      string                       `json:"message,omitempty"`
	Images       []string                     `json:"images,omitempty"`
}
type liveApprovalRequest struct {
	ControllerID string                        `json:"controller_id"`
	SessionID    string                        `json:"session_id"`
	Authority    livesession.ApprovalAuthority `json:"authority"`
	Decision     livesession.ApprovalDecision  `json:"decision"`
}

func (s *Server) projectOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Shipmates-Project") != s.projectScope {
			writeLiveJSON(w, http.StatusConflict, map[string]string{"code": "project_mismatch", "message": "live session request failed"})
			return
		}
		w.Header().Set("X-Shipmates-Project", s.projectScope)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) localControlOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.controlToken
		if s.controlToken == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeLiveJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			return
		}
		if r.Header.Get("X-Shipmates-Project") != s.projectScope {
			writeLiveJSON(w, http.StatusConflict, map[string]string{"code": "project_mismatch"})
			return
		}
		w.Header().Set("X-Shipmates-Project", s.projectScope)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLocalSteerTargets(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	entries := s.liveSessions.ActiveRemoteSteerTargets(time.Now())
	writeLiveJSON(w, http.StatusOK, localSteerTargetsResponse{SchemaVersion: 1, ProjectScope: s.projectScope, Targets: entries})
}

func (s *Server) handleLocalSteerExact(w http.ResponseWriter, r *http.Request) {
	var req localSteerExactRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.SchemaVersion != 1 || req.ProjectScope != s.projectScope || s.remoteSteer == nil {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	res, _ := s.remoteSteer.Steer(r.Context(), req.Operator, req.Request, req.Target, s.liveSessions)
	writeLiveJSON(w, http.StatusOK, res)
}

func (s *Server) handleLocalInterruptExact(w http.ResponseWriter, r *http.Request) {
	var req localInterruptExactRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.SchemaVersion != 1 || req.ProjectScope != s.projectScope || s.remoteInterrupt == nil {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	res, _ := s.remoteInterrupt.Interrupt(r.Context(), req.Operator, req.Request, req.Target, s.liveSessions, s.liveSessions)
	writeLiveJSON(w, http.StatusOK, res)
}

func writeLiveJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeLiveError(w http.ResponseWriter, err error) {
	code := livesession.ErrorCode(err)
	status := http.StatusConflict
	if code == livesession.NotFound {
		status = http.StatusNotFound
	}
	writeLiveJSON(w, status, map[string]any{"code": code, "message": "live session request failed"})
}

func (s *Server) handleCodexLive(w http.ResponseWriter, r *http.Request) {
	var req liveStartRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.Prompt == "" {
		writeLiveJSON(w, 400, map[string]string{"code": "invalid_request"})
		return
	}
	if len(req.Images) != 0 && strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	batch, err := turninput.ValidateImages(s.projectRoot, req.Images)
	if err != nil {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_image"})
		return
	}
	defer batch.Close()
	sess, err := s.liveSessions.StartLive(r.Context(), livesession.StartOptions{Persona: r.PathValue("persona"), Prompt: req.Prompt, Fresh: req.Fresh, Images: batch.Images()})
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, 200, sess.Snapshot())
}

func (s *Server) handleCodexAttach(w http.ResponseWriter, r *http.Request) {
	var req liveAttachRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	persona := r.PathValue("persona")
	sess, err := s.liveSessions.Session(persona)
	if err != nil {
		if livesession.ErrorCode(err) != livesession.NotFound {
			writeLiveError(w, err)
			return
		}
		sess, err = s.liveSessions.StartIdle(r.Context(), livesession.StartIdleOptions{Persona: persona, Fresh: req.Fresh})
		if err != nil {
			writeLiveError(w, err)
			return
		}
	} else if snap := sess.Snapshot(); snap.State == livesession.Stopped || snap.State == livesession.Failed {
		sess, err = s.liveSessions.StartIdle(r.Context(), livesession.StartIdleOptions{Persona: persona, Fresh: req.Fresh})
		if err != nil {
			writeLiveError(w, err)
			return
		}
	} else if req.Fresh {
		writeLiveError(w, &livesession.Error{Code: livesession.Busy})
		return
	}
	attached, err := s.liveSessions.AttachController(persona, req.SessionID, req.After)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, http.StatusOK, attached)
	_ = sess // ownership deliberately remains with the manager after detach
}

func (s *Server) handleCodexRelease(w http.ResponseWriter, r *http.Request) {
	var req liveReleaseRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.SessionID == "" || req.ControllerID == "" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	if err := s.liveSessions.ReleaseController(r.PathValue("persona"), req.SessionID, req.ControllerID); err != nil {
		writeLiveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCodexHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req liveReleaseRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.SessionID == "" || req.ControllerID == "" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	if err := s.liveSessions.HeartbeatController(r.PathValue("persona"), req.SessionID, req.ControllerID); err != nil {
		writeLiveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleCodexSync(w http.ResponseWriter, r *http.Request) {
	var req liveSyncRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.SessionID == "" || req.ControllerID == "" {
		writeLiveJSON(w, 400, map[string]string{"code": "invalid_request"})
		return
	}
	a, err := s.liveSessions.SyncController(r.PathValue("persona"), req.SessionID, req.ControllerID, req.After)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, http.StatusOK, a)
}
func (s *Server) handleCodexControllerAction(w http.ResponseWriter, r *http.Request) {
	var req liveControllerActionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || req.ControllerID == "" || req.SessionID == "" || req.ThreadID == "" ||
		(req.Action == livesession.ControllerMessage && req.Message == "") ||
		(req.Action != livesession.ControllerMessage && req.Action != livesession.ControllerInterrupt) {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	if len(req.Images) != 0 && strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	if len(req.Images) != 0 {
		sess, stateErr := s.liveSessions.Session(r.PathValue("persona"))
		if stateErr != nil || sess.Snapshot().State != livesession.Idle {
			writeLiveJSON(w, http.StatusConflict, map[string]string{"code": "image_wrong_state"})
			return
		}
	}
	batch, err := turninput.ValidateImages(s.projectRoot, req.Images)
	if err != nil {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_image"})
		return
	}
	defer batch.Close()
	res, err := s.liveSessions.ControlControllerInput(r.Context(), req.Action, r.PathValue("persona"), req.ControllerID, req.SessionID, req.ThreadID, req.TurnID, req.Message, batch.Images())
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, http.StatusOK, res)
}
func (s *Server) handleCodexApproval(w http.ResponseWriter, r *http.Request) {
	var req liveApprovalRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.ControllerID == "" || req.SessionID == "" || (req.Decision != livesession.AllowOnce && req.Decision != livesession.DenyOnce) {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request"})
		return
	}
	res, err := s.liveSessions.ResolveControllerApproval(r.Context(), r.PathValue("persona"), req.ControllerID, req.SessionID, req.Authority, req.Decision)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, http.StatusOK, res)
}
func (s *Server) handleCodexFeed(w http.ResponseWriter, r *http.Request) {
	sess, err := s.liveSessions.Session(r.PathValue("persona"))
	if err != nil {
		writeLiveError(w, err)
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	follow := r.URL.Query().Get("follow") == "true"
	enc := json.NewEncoder(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	initial := sess.Feed(after)
	if initial.HistoryDropped {
		w.Header().Set("X-Shipmates-History-Dropped", "true")
	}
	flusher, _ := w.(http.Flusher)
	for {
		f := sess.Feed(after)
		for _, e := range f.Events {
			if err := enc.Encode(e); err != nil {
				return
			}
			after = e.Sequence
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !follow {
			return
		}
		snap := sess.Snapshot()
		if snap.State == livesession.Stopped || snap.State == livesession.Failed || (snap.State == livesession.Idle && after+1 >= f.NextSequence) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-sess.Done():
		case <-sess.Notify():
		case <-time.After(time.Second):
		}
	}
}
func decodeTarget(r *http.Request) (liveTargetRequest, error) {
	var req liveTargetRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	return req, err
}
func (s *Server) handleCodexTell(w http.ResponseWriter, r *http.Request) {
	req, err := decodeTarget(r)
	if err != nil || req.Message == "" {
		writeLiveJSON(w, 400, map[string]string{"code": "invalid_request"})
		return
	}
	res, err := s.liveSessions.Tell(r.Context(), r.PathValue("persona"), req.SessionID, req.ThreadID, req.TurnID, req.Message)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, 200, res)
}
func (s *Server) handleCodexInterrupt(w http.ResponseWriter, r *http.Request) {
	req, err := decodeTarget(r)
	if err != nil {
		writeLiveJSON(w, 400, map[string]string{"code": "invalid_request"})
		return
	}
	if req.TurnID == "" {
		writeLiveJSON(w, http.StatusBadRequest, map[string]string{"code": string(livesession.InvalidTarget), "message": "live session request failed"})
		return
	}
	res, err := s.liveSessions.Interrupt(r.Context(), r.PathValue("persona"), req.SessionID, req.ThreadID, req.TurnID)
	if err != nil {
		writeLiveError(w, err)
		return
	}
	writeLiveJSON(w, 200, res)
}
func (s *Server) closeCodexLive() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.liveSessions.ShutdownAll(ctx)
}
