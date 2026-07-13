package livesession

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/policy"
)

const (
	ApprovalOperatorDeadline = 30 * time.Second
	ApprovalResponseDeadline = 2 * time.Second
	maxApprovalIdentityBytes = 1024
	maxApprovalCommandBytes  = 16 << 10
	maxApprovalHistory       = 256
	maxApprovalSummaryBytes  = 160
)

type ApprovalDecision string

const (
	AllowOnce ApprovalDecision = "allow_once"
	DenyOnce  ApprovalDecision = "deny"
)

type AdapterApprovalResult struct{ Accepted bool }

type AdapterApprovalRequest struct{ RequestID, ThreadID, TurnID, PolicySnapshotID string }

// approvalAdapter is deliberately backend-neutral and remains internal to the
// session manager. Production adapters are not wired in this M6 slice.
type approvalAdapter interface {
	ResolveApproval(context.Context, AdapterApprovalRequest, ApprovalDecision) (AdapterApprovalResult, error)
}

func adapterRequest(p *pendingApproval) AdapterApprovalRequest {
	return AdapterApprovalRequest{p.request.BackendRequestID, p.authority.ThreadID, p.authority.TurnID, p.authority.PolicySnapshotID}
}

type NormalizedApprovalRequest struct {
	Kind                                          policy.RequestKind
	CommandExact, ProjectRootID                   string
	SessionID, ThreadID, TurnID, BackendRequestID string
}

type ApprovalAuthority struct {
	Persona, SessionID, Backend, ThreadID, TurnID string
	ControllerID                                  string
	ControllerLeaseGeneration                     uint64
	BackendRequestID, ApprovalRequestFingerprint  string
	PolicySnapshotID, ApprovalID                  string
}

type ApprovalEvaluation struct {
	Effect           policy.Effect
	PolicySnapshotID string
	MatchedRules     []policy.MatchedRule
}

type ApprovalResult struct {
	Code       Code
	Outcome    string
	ReasonCode string
}

// ApprovalCard is returned only on the controller-authenticated attach/sync
// channel. Authority is opaque client correlation state; presentation must use
// only the bounded display fields.
type ApprovalCard struct {
	Authority    ApprovalAuthority    `json:"authority"`
	RequestKind  string               `json:"request_kind"`
	Summary      string               `json:"summary"`
	PolicyEffect string               `json:"policy_effect"`
	MatchedRules []policy.MatchedRule `json:"matched_rules,omitempty"`
	ExpiresInMS  int64                `json:"expires_in_ms"`
}

func (s *Session) approvalCardLocked(now time.Time) *ApprovalCard {
	p := s.approval
	if p == nil || p.authority.ApprovalID == "" || p.decision != "" || p.committed || !now.Before(p.deadline) {
		return nil
	}
	rules := append([]policy.MatchedRule(nil), p.evaluation.MatchedRules...)
	if len(rules) > policy.MaxMatchedRules {
		rules = rules[:policy.MaxMatchedRules]
	}
	return &ApprovalCard{Authority: p.authority, RequestKind: string(p.request.Kind), Summary: approvalSummary(p.request.CommandExact), PolicyEffect: string(p.evaluation.Effect), MatchedRules: rules, ExpiresInMS: p.deadline.Sub(now).Milliseconds()}
}

func approvalSummary(command string) string {
	command = strings.ToValidUTF8(command, "�")
	fields, ok := shellSummaryFields(command)
	if !ok {
		return "[redacted]"
	}
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if header, hasValue := sensitiveHeader(field); header {
			fields[i] = "[redacted-header]"
			if !hasValue {
				// An unquoted header value has no reliable boundary. Redact the
				// remainder rather than guessing where the credential ends.
				fields = append(fields[:i+1], "[redacted]")
			}
			continue
		}
		if headerFlagValue, isHeaderFlag := splitHeaderFlag(field); isHeaderFlag {
			if header, hasValue := sensitiveHeader(headerFlagValue); header {
				fields[i] = "[redacted-header]"
				if !hasValue {
					fields = append(fields[:i+1], "[redacted]")
				}
			}
			continue
		}
		if isHeaderFlag(field) && i+1 < len(fields) {
			if header, hasValue := sensitiveHeader(fields[i+1]); header {
				fields[i+1] = "[redacted-header]"
				if !hasValue {
					fields = append(fields[:i+2], "[redacted]")
				}
				i++
				continue
			}
		}
		if credentialFlag(field) && !strings.Contains(field, "=") {
			if i+1 < len(fields) {
				fields[i+1] = "[redacted]"
				i++
			}
			continue
		}
		if redacted, ok := redactAssignment(field); ok {
			fields[i] = redacted
			continue
		}
		fields[i] = redactURLCredentials(field)
		field = fields[i]
		if strings.HasPrefix(field, "/") || strings.HasPrefix(field, "~/") || strings.Contains(field, ":\\") {
			fields[i] = "[path]"
		}
	}
	s := strings.Join(fields, " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '�'
		}
		return r
	}, s)
	if len(s) <= maxApprovalSummaryBytes {
		return s
	}
	n := maxApprovalSummaryBytes - len("…")
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// shellSummaryFields recognizes only the quoting needed to preserve shell word
// boundaries. It does not interpret expansions. Any incomplete quote or escape
// fails closed because displaying a partial parse could expose a credential.
func shellSummaryFields(s string) ([]string, bool) {
	var fields []string
	var word strings.Builder
	inWord := false
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f') {
			if inWord {
				fields = append(fields, word.String())
				word.Reset()
				inWord = false
			}
			continue
		}
		inWord = true
		switch {
		case c == '\\' && quote != '\'':
			if i+1 >= len(s) {
				return nil, false
			}
			i++
			word.WriteByte(s[i])
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == c:
			quote = 0
		default:
			word.WriteByte(c)
		}
	}
	if quote != 0 {
		return nil, false
	}
	if inWord {
		fields = append(fields, word.String())
	}
	return fields, true
}

func sensitiveHeader(field string) (bool, bool) {
	name, value, found := strings.Cut(field, ":")
	if !found {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "proxy-authorization", "x-api-key":
		return true, strings.TrimSpace(value) != ""
	default:
		return false, false
	}
}

func isHeaderFlag(field string) bool {
	return field == "-H" || strings.EqualFold(field, "--header")
}

func splitHeaderFlag(field string) (string, bool) {
	if strings.HasPrefix(field, "-H") && len(field) > 2 {
		return strings.TrimPrefix(field[2:], "="), true
	}
	if len(field) > len("--header=") && strings.EqualFold(field[:len("--header=")], "--header=") {
		return field[len("--header="):], true
	}
	return "", false
}

var urlCredential = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@:]+:[^/@]+@`)

func credentialFlag(field string) bool {
	name := strings.TrimLeft(strings.SplitN(field, "=", 2)[0], "-")
	return secretName(name) || strings.EqualFold(name, "u") || strings.EqualFold(name, "user")
}

func redactAssignment(field string) (string, bool) {
	parts := strings.SplitN(field, "=", 2)
	if len(parts) != 2 {
		return "", false
	}
	name := strings.TrimLeft(parts[0], "-")
	if !secretName(name) {
		return "", false
	}
	return parts[0] + "=[redacted]", true
}

func secretName(name string) bool {
	n := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(name))
	for _, marker := range []string{"password", "passwd", "passphrase", "secret", "token", "apikey", "accesskey", "privatekey", "clientsecret", "credential", "authorization", "authheader", "bearertoken"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

func redactURLCredentials(field string) string {
	return urlCredential.ReplaceAllString(field, `${1}[redacted]@`)
}

type pendingApproval struct {
	authority     ApprovalAuthority
	request       NormalizedApprovalRequest
	evaluation    ApprovalEvaluation
	adapter       approvalAdapter
	deadline      time.Time
	decision      ApprovalDecision
	committed     bool
	actor, reason string
	done          chan struct{}
	after         func(time.Duration) <-chan time.Time
}

type approvalRecord struct {
	authority ApprovalAuthority
	decision  ApprovalDecision
	result    ApprovalResult
}

func ApprovalRequestFingerprint(r NormalizedApprovalRequest) (string, error) {
	if r.Kind != policy.ProcessExec || r.ProjectRootID == "" || r.CommandExact == "" ||
		!utf8.ValidString(r.ProjectRootID) || !utf8.ValidString(r.CommandExact) ||
		len(r.ProjectRootID) > maxApprovalIdentityBytes || len(r.CommandExact) > maxApprovalCommandBytes {
		return "", failure(InvalidInput)
	}
	h := sha256.New()
	h.Write([]byte("shipmates.approval-request-fingerprint.v1\x00"))
	for _, v := range []string{"1", string(r.Kind), r.ProjectRootID, r.CommandExact} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BeginApproval accepts only an already normalized request and already computed
// M5 evaluation. It never parses protocol data or evaluates policy.
func (m *Manager) BeginApproval(r NormalizedApprovalRequest, e ApprovalEvaluation, lease ApprovalAuthority, a approvalAdapter) (ApprovalAuthority, error) {
	s, err := m.Session(lease.Persona)
	if err != nil {
		return ApprovalAuthority{}, err
	}
	fp, err := ApprovalRequestFingerprint(r)
	if err != nil {
		return ApprovalAuthority{}, err
	}
	if a == nil || r.SessionID == "" || r.ThreadID == "" || r.TurnID == "" || r.BackendRequestID == "" ||
		len(r.BackendRequestID) > maxApprovalIdentityBytes || !validDigest(e.PolicySnapshotID) {
		return ApprovalAuthority{}, failure(InvalidInput)
	}
	s.mu.Lock()
	if s.approvalHistory == nil {
		s.approvalHistory = make(map[string]approvalRecord)
	}
	if old, ok := s.approvalHistory[r.BackendRequestID]; ok {
		s.mu.Unlock()
		if old.authority.ApprovalRequestFingerprint != fp {
			return ApprovalAuthority{}, failure(FailedCode)
		}
		return ApprovalAuthority{}, failure(AlreadyResolved)
	}
	now := m.now()
	valid := lease.SessionID == s.sessionID && lease.Backend == s.snapshotLocked().Backend &&
		lease.ThreadID == s.threadID && lease.TurnID == s.turnID && r.SessionID == s.sessionID &&
		r.ThreadID == s.threadID && r.TurnID == s.turnID && lease.BackendRequestID == r.BackendRequestID &&
		lease.ControllerID != "" && lease.ControllerID == s.controllerID &&
		lease.ControllerLeaseGeneration != 0 && lease.ControllerLeaseGeneration == s.controllerLeaseGeneration &&
		now.Before(s.controllerExpires) && s.state == Working && s.policySnapshot != nil &&
		e.PolicySnapshotID == s.policySnapshot.ID
	if !valid || (e.Effect != policy.Ask && e.Effect != policy.Allow && e.Effect != policy.Deny) {
		s.mu.Unlock()
		return ApprovalAuthority{}, failure(StaleApproval)
	}
	lease.ApprovalRequestFingerprint, lease.PolicySnapshotID = fp, e.PolicySnapshotID
	if s.approval != nil {
		if s.approval.request.BackendRequestID == r.BackendRequestID {
			code := DuplicateApproval
			if s.approval.authority.ApprovalRequestFingerprint != fp {
				code = RequestIDReuse
			}
			s.mu.Unlock()
			go refuseApproval(a, m.after, AdapterApprovalRequest{r.BackendRequestID, r.ThreadID, r.TurnID, e.PolicySnapshotID})
			return ApprovalAuthority{}, failure(code)
		}
		if e.Effect != policy.Deny {
			s.mu.Unlock()
			go refuseApproval(a, m.after, AdapterApprovalRequest{r.BackendRequestID, r.ThreadID, r.TurnID, e.PolicySnapshotID})
			return ApprovalAuthority{}, failure(ApprovalConcurrent)
		}
	}
	if e.Effect == policy.Deny {
		lease.ApprovalID = ""
		p := &pendingApproval{authority: lease, request: r, evaluation: e, adapter: a, decision: DenyOnce, actor: "system", reason: "policy_deny", done: make(chan struct{})}
		p.after = m.after
		s.mu.Unlock()
		go m.deliverPolicyDeny(s, p)
		return lease, nil
	}
	id, idErr := m.newApprovalID()
	if idErr != nil {
		s.mu.Unlock()
		return ApprovalAuthority{}, failure(Internal)
	}
	lease.ApprovalID = id
	p := &pendingApproval{authority: lease, request: r, evaluation: e, adapter: a, deadline: now.Add(ApprovalOperatorDeadline), done: make(chan struct{})}
	p.after = m.after
	s.approval = p
	s.publishLocked("approval.pending", s.threadID, s.turnID, pendingEventData(id, e))
	s.mu.Unlock()
	go func() { <-m.after(ApprovalOperatorDeadline); m.expireApproval(s, p) }()
	go m.watchApprovalLease(s, p)
	return lease, nil
}

func (m *Manager) deliverPolicyDeny(s *Session, p *pendingApproval) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type response struct {
		result AdapterApprovalResult
		err    error
	}
	ch := make(chan response, 1)
	go func() {
		r, err := p.adapter.ResolveApproval(ctx, adapterRequest(p), DenyOnce)
		ch <- response{r, err}
	}()
	ok := false
	select {
	case r := <-ch:
		ok = r.err == nil && r.result.Accepted
	case <-m.after(ApprovalResponseDeadline):
	}
	s.completePolicyDeny(p, ok)
}

func (s *Session) completePolicyDeny(p *pendingApproval, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := ApprovalResult{ReasonCode: p.reason, Outcome: "denied"}
	if !ok {
		result.Code, result.Outcome = ResolutionFailed, "resolution_failed"
	}
	s.publishLocked("request.refused", p.authority.ThreadID, p.authority.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "refused", "policy_snapshot_id": p.evaluation.PolicySnapshotID, "policy_effect": string(policy.Deny), "reason_code": "policy_deny"})
	if len(s.approvalHistory) >= maxApprovalHistory {
		for k := range s.approvalHistory {
			delete(s.approvalHistory, k)
			break
		}
	}
	s.approvalHistory[p.request.BackendRequestID] = approvalRecord{authority: p.authority, decision: DenyOnce, result: result}
	close(p.done)
}

func (m *Manager) watchApprovalLease(s *Session, p *pendingApproval) {
	for {
		s.mu.Lock()
		if s.approval != p || p.committed {
			s.mu.Unlock()
			return
		}
		remaining := s.controllerExpires.Sub(m.now())
		if remaining <= 0 {
			s.cancelApprovalLocked("lease_expired", false)
			s.controllerID = ""
			s.controllerExpires = time.Time{}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		<-m.after(remaining)
	}
}

func pendingEventData(id string, e ApprovalEvaluation) map[string]any {
	rules := make([]map[string]string, 0, minInt(len(e.MatchedRules), policy.MaxMatchedRules))
	for i, r := range e.MatchedRules {
		if i == policy.MaxMatchedRules {
			break
		}
		rules = append(rules, map[string]string{"layer": string(r.Layer), "path": r.Path, "id": r.ID, "effect": string(r.Effect)})
	}
	return map[string]any{"approval_id": id, "request_kind": string(policy.ProcessExec), "policy_effect": string(e.Effect), "policy_snapshot_id": e.PolicySnapshotID, "matched_rules": rules, "expires_in_ms": ApprovalOperatorDeadline.Milliseconds()}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Manager) ResolveApproval(ctx context.Context, auth ApprovalAuthority, decision ApprovalDecision) (ApprovalResult, error) {
	if decision != AllowOnce && decision != DenyOnce {
		return ApprovalResult{}, failure(InvalidInput)
	}
	select {
	case <-ctx.Done():
		return ApprovalResult{}, ctx.Err()
	default:
	}
	s, err := m.Session(auth.Persona)
	if err != nil {
		return ApprovalResult{}, err
	}
	s.mu.Lock()
	if rec, ok := s.approvalHistory[auth.BackendRequestID]; ok && rec.authority == auth {
		res := rec.result
		same := rec.decision == decision
		s.mu.Unlock()
		if same {
			if res.Code == ResolutionFailed {
				return res, failure(ResolutionFailed)
			}
			return res, nil
		}
		return res, failure(AlreadyResolved)
	}
	p := s.approval
	if p == nil || p.authority != auth {
		s.mu.Unlock()
		return ApprovalResult{}, failure(StaleApproval)
	}
	if p.decision != "" {
		done, chosen := p.done, p.decision
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ApprovalResult{}, ctx.Err()
		case <-done:
		}
		s.mu.Lock()
		rec := s.approvalHistory[auth.BackendRequestID]
		s.mu.Unlock()
		if chosen == decision {
			return rec.result, nil
		}
		return rec.result, failure(AlreadyResolved)
	}
	if auth.ControllerID != s.controllerID || auth.ControllerLeaseGeneration != s.controllerLeaseGeneration || !m.now().Before(s.controllerExpires) {
		s.mu.Unlock()
		return ApprovalResult{}, failure(StaleController)
	}
	select {
	case <-ctx.Done():
		s.mu.Unlock()
		return ApprovalResult{}, ctx.Err()
	default:
	}
	if !m.now().Before(p.deadline) {
		p.decision, p.actor, p.reason = DenyOnce, "system", "deadline"
		s.mu.Unlock()
		go m.deliver(context.Background(), s, p, true)
		return ApprovalResult{}, failure(AlreadyResolved)
	}
	p.decision, p.actor, p.reason = decision, "local_controller", "operator"
	s.mu.Unlock()
	go m.deliver(ctx, s, p, false)
	select {
	case <-ctx.Done():
		return ApprovalResult{}, ctx.Err()
	case <-p.done:
	}
	s.mu.Lock()
	rec := s.approvalHistory[auth.BackendRequestID]
	s.mu.Unlock()
	if rec.result.Code == ResolutionFailed {
		return rec.result, failure(ResolutionFailed)
	}
	return rec.result, nil
}

func (m *Manager) expireApproval(s *Session, p *pendingApproval) {
	s.mu.Lock()
	if s.approval != p || p.decision != "" || m.now().Before(p.deadline) {
		s.mu.Unlock()
		return
	}
	p.decision, p.actor, p.reason = DenyOnce, "system", "deadline"
	s.mu.Unlock()
	m.deliver(context.Background(), s, p, true)
}

func (s *Session) cancelApprovalLocked(reason string, expired bool) {
	p := s.approval
	if p == nil || p.committed {
		return
	}
	p.decision, p.actor, p.reason = DenyOnce, "system", reason
	p.committed = true
	go func() { // delivery must never run while the session state lock is held
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type response struct {
			result AdapterApprovalResult
			err    error
		}
		ch := make(chan response, 1)
		go func() {
			r, err := p.adapter.ResolveApproval(ctx, adapterRequest(p), DenyOnce)
			ch <- response{r, err}
		}()
		ok := false
		select {
		case r := <-ch:
			ok = r.err == nil && r.result.Accepted
		case <-p.after(ApprovalResponseDeadline):
		}
		s.completeApproval(p, ok, expired)
	}()
}

func (m *Manager) deliver(decisionCtx context.Context, s *Session, p *pendingApproval, expired bool) {
	s.mu.Lock()
	if s.approval != p || p.committed {
		s.mu.Unlock()
		return
	}
	select {
	case <-decisionCtx.Done():
		p.decision, p.actor, p.reason = "", "", ""
		s.mu.Unlock()
		return
	default:
	}
	if p.decision == AllowOnce && (p.authority.ControllerID != s.controllerID || p.authority.ControllerLeaseGeneration != s.controllerLeaseGeneration || !m.now().Before(s.controllerExpires) || !m.now().Before(p.deadline) || s.turnID != p.authority.TurnID || s.state != Working) {
		p.decision, p.actor, p.reason = DenyOnce, "system", "authority_lost"
	}
	p.committed = true
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(decisionCtx)
	defer cancel()
	type response struct {
		result AdapterApprovalResult
		err    error
	}
	ch := make(chan response, 1)
	go func() {
		r, e := p.adapter.ResolveApproval(ctx, adapterRequest(p), p.decision)
		ch <- response{r, e}
	}()
	ok := false
	select {
	case r := <-ch:
		ok = r.err == nil && r.result.Accepted
	case <-m.after(ApprovalResponseDeadline):
	}
	if !ok && p.decision == AllowOnce {
		// Once allow delivery has been selected, an adapter timeout, cancellation,
		// or failed correlation makes authority unknowable. Close the exact owner
		// before publishing completion so a late write cannot escape tracking.
		s.finish(Failed, s.adapter, codexapp.ProtocolViolation)
	}
	s.completeApproval(p, ok, expired)
}

func (s *Session) completeApproval(p *pendingApproval, ok, expired bool) {
	s.mu.Lock()
	if s.approval != p {
		s.mu.Unlock()
		return
	}
	result := ApprovalResult{ReasonCode: p.reason}
	if !ok {
		result.Code, result.Outcome = ResolutionFailed, "resolution_failed"
	} else if p.decision == AllowOnce {
		result.Outcome = "allowed_once"
	} else {
		result.Outcome = "denied"
	}
	if p.reason == "policy_deny" {
		s.publishLocked("request.refused", p.authority.ThreadID, p.authority.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "refused", "policy_snapshot_id": p.evaluation.PolicySnapshotID, "policy_effect": string(policy.Deny), "reason_code": "policy_deny"})
	} else if expired && ok {
		s.publishLocked("approval.expired", p.authority.ThreadID, p.authority.TurnID, map[string]any{"approval_id": p.authority.ApprovalID, "actor_class": "system", "reason_code": "deadline"})
	} else {
		s.publishLocked("approval.resolved", p.authority.ThreadID, p.authority.TurnID, map[string]any{"approval_id": p.authority.ApprovalID, "outcome": result.Outcome, "actor_class": p.actor, "reason_code": p.reason})
	}
	if len(s.approvalHistory) >= maxApprovalHistory {
		for k := range s.approvalHistory {
			delete(s.approvalHistory, k)
			break
		}
	}
	s.approvalHistory[p.request.BackendRequestID] = approvalRecord{authority: p.authority, decision: p.decision, result: result}
	s.approval = nil
	close(p.done)
	s.mu.Unlock()
}

func refuseApproval(a approvalAdapter, after func(time.Duration) <-chan time.Time, request AdapterApprovalRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{}, 1)
	go func() { _, _ = a.ResolveApproval(ctx, request, DenyOnce); done <- struct{}{} }()
	select {
	case <-done:
	case <-after(ApprovalResponseDeadline):
	}
}

func validDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	return strings.IndexFunc(v, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}
