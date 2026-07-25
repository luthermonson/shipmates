// Package codexapp implements the private Codex app-server stdio transport.
// Its exported values are deliberately backend-neutral; JSON-RPC wire values
// remain private to this package.
package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/turninput"
)

const (
	defaultMaxFrame = 8 << 20
	defaultStderr   = 64 << 10
	// DefaultPreExecHelper is the immutable destination owned by the unified
	// installer. An empty project override selects this path.
	DefaultPreExecHelper = "/usr/libexec/shipmates/shipmates-cgroup-launcher"
	// ManagedSessionEnvironment marks commands launched by a Shipmates-owned
	// Codex session so orchestration commands cannot recursively launch crews.
	ManagedSessionEnvironment = "SHIPMATES_MANAGED_SESSION"
)

func effectivePreExecHelper(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return DefaultPreExecHelper
	}
	return configured
}

// Code is a stable, sanitized adapter failure code.
type Code string

const (
	UnsupportedVersion    Code = "unsupported_version"
	UnsupportedCapability Code = "unsupported_capability"
	StartupTimeout        Code = "startup_timeout"
	RequestTimeout        Code = "request_timeout"
	MalformedFrame        Code = "malformed_frame"
	ProtocolViolation     Code = "protocol_violation"
	UnexpectedEOF         Code = "unexpected_eof"
	ChildExit             Code = "child_exit"
	CleanupFailed         Code = "cleanup_failed"
	BackendRejected       Code = "backend_rejected"
	Internal              Code = "internal"
)

var safeMessages = map[Code]string{
	UnsupportedVersion:    "the Codex app-server version is not supported",
	UnsupportedCapability: "the Codex app-server lacks a required capability",
	StartupTimeout:        "the Codex app-server did not start in time",
	RequestTimeout:        "the Codex app-server request did not complete in time",
	MalformedFrame:        "the Codex app-server sent a malformed frame",
	ProtocolViolation:     "the Codex app-server violated the protocol",
	UnexpectedEOF:         "the Codex app-server closed unexpectedly",
	ChildExit:             "the Codex app-server exited unexpectedly",
	CleanupFailed:         "the Codex app-server could not be cleaned up",
	BackendRejected:       "the Codex app-server rejected the request",
	Internal:              "the Codex app-server could not be started",
}

// Error never contains backend text, stderr, environment values, or raw frames.
type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) + ": " + safeMessages[e.Code] }
func failure(code Code) error  { return &Error{Code: code} }
func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}

// Capabilities states the backend-neutral operations guaranteed by the pinned
// protocol range.
type Capabilities struct {
	ThreadStart, ThreadResume, TurnStart, Steer, Interrupt, RequestRefusal, LocalImage bool
}

func requiredCapabilities() Capabilities {
	return Capabilities{true, true, true, true, true, true, true}
}

// StartOptions contains transport-only configuration. Command is an explicit
// argv vector to support a deterministic subprocess fixture; production callers
// omit it and get exactly "codex app-server --stdio".
type StartOptions struct {
	WorkingDirectory string
	Environment      map[string]string
	Command          []string
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	MaxFrameBytes    int
	MaxStderrBytes   int
	MinVersion       string
	MaxVersion       string
	// CredentialFree omits CODEX_HOME so an advisory child cannot inherit
	// reusable Codex credentials from the caller.
	CredentialFree bool
	// TransportCodexHome supplies an explicitly provisioned, isolated Codex
	// home to the trusted app-server transport. It is honored only with
	// CredentialFree and does not restore HOME or ambient provider variables.
	TransportCodexHome          string
	RequireExecutionContainment bool
	ExecutionID                 string
	PreExecHelper               string
}

type Factory struct{}

type Adapter struct {
	cmd            *exec.Cmd
	pidfd          int
	containment    *ExecutionContainment
	containmentErr error
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	mu             sync.Mutex
	nextID         int64
	pending        map[int64]pendingCall
	done           chan struct{}
	waitDone       chan struct{}
	terminal       error
	closing        bool
	closeOnce      sync.Once
	shutdown       time.Duration
	maxFrame       int
	events         chan Event
	policyMu       sync.RWMutex
	policies       map[turnKey]*policy.Snapshot
	approvals      map[string]pendingApproval
}

type pendingApproval struct {
	id                                 int64
	threadID, turnID, policySnapshotID string
	resolving                          bool
}

type turnKey struct {
	threadID, turnID string
}

type pendingCall struct {
	ch         chan rpcMessage
	turnThread string
	snapshot   *policy.Snapshot
}

// Thread and Turn are backend-neutral established identities. Their IDs are
// opaque and are never interpreted as paths or command-line fragments.
type Thread struct{ ID string }
type Turn struct{ ID string }

type ThreadOptions struct {
	WorkingDirectory      string
	DeveloperInstructions string
	Model                 string
	ReadOnly              bool
	Toolless              bool
}

type TurnInput struct {
	Text   string
	Policy *policy.Snapshot
	Images []turninput.ImageDescriptorV1
	Effort string
}

type EventKind string

const (
	TurnCompleted     EventKind = "turn_completed"
	TurnFailed        EventKind = "turn_failed"
	AdapterFault      EventKind = "adapter_fault"
	AgentMessage      EventKind = "agent_message"
	Activity          EventKind = "activity"
	RequestRefused    EventKind = "request_refused"
	ApprovalRequested EventKind = "approval_requested"
)

// Event is the deliberately narrow lifecycle signal needed by the session
// owner. It contains no raw protocol payload or user content.
type Event struct {
	Kind             EventKind
	ThreadID         string
	TurnID           string
	Code             Code
	Text             string
	Category         string
	RequestClass     string
	Partial          bool
	PolicySnapshotID string
	PolicyEffect     policy.Effect
	ReasonCode       string
	MatchedRules     []policy.MatchedRule
	OmittedMatches   int
	BackendRequestID string
	CommandExact     string
	Detail           string
}

type ApprovalDecision string

const (
	AllowOnce ApprovalDecision = "allow_once"
	Deny      ApprovalDecision = "deny"
)

type ApprovalResponse struct {
	RequestID, ThreadID, TurnID, PolicySnapshotID string
}

func (a *Adapter) Events() <-chan Event { return a.events }

type rpcMessage struct {
	ID       json.RawMessage `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	Result   json.RawMessage `json:"result"`
	RPCError json.RawMessage `json:"error"`
}

func (Factory) Start(ctx context.Context, opts StartOptions) (*Adapter, Capabilities, error) {
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 10 * time.Second
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 2 * time.Second
	}
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = defaultMaxFrame
	}
	if opts.MaxStderrBytes <= 0 {
		opts.MaxStderrBytes = defaultStderr
	}
	if opts.MinVersion == "" {
		opts.MinVersion = "0.144.0"
	}
	if opts.MaxVersion == "" {
		opts.MaxVersion = "0.144.999"
	}
	minVersion, ok := parseVersion(opts.MinVersion)
	if !ok {
		return nil, Capabilities{}, failure(Internal)
	}
	maxVersion, ok := parseVersion(opts.MaxVersion)
	if !ok {
		return nil, Capabilities{}, failure(Internal)
	}
	opts.MinVersion, opts.MaxVersion = minVersion, maxVersion
	if opts.WorkingDirectory == "" || !filepath.IsAbs(opts.WorkingDirectory) {
		return nil, Capabilities{}, failure(Internal)
	}
	info, err := os.Stat(opts.WorkingDirectory)
	if err != nil || !info.IsDir() {
		return nil, Capabilities{}, failure(Internal)
	}
	argv := opts.Command
	if len(argv) == 0 {
		argv = []string{"codex", "app-server", "--stdio"}
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, Capabilities{}, failure(Internal)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	if st, statErr := os.Stat(path); statErr != nil || !st.Mode().IsRegular() {
		return nil, Capabilities{}, failure(Internal)
	}
	if opts.RequireExecutionContainment {
		opts.PreExecHelper = effectivePreExecHelper(opts.PreExecHelper)
		path, err = containedExecutable(path)
		if err != nil {
			return nil, Capabilities{}, failure(Internal)
		}
	}
	cmd := exec.Command(path, argv[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = opts.WorkingDirectory
	cmd.Env = controlledEnvironment(opts.Environment, opts.CredentialFree, opts.TransportCodexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, Capabilities{}, failure(Internal)
	}
	var containment *ExecutionContainment
	var pidfd int
	if opts.RequireExecutionContainment {
		if opts.ExecutionID == "" {
			return nil, Capabilities{}, failure(Internal)
		}
		containment, err = StartContainedWithHelperCurrent(opts.PreExecHelper, cmd, opts.ExecutionID)
		if err != nil {
			return nil, Capabilities{}, failure(Internal)
		}
		pidfd = extractContainmentPidfd(containment)
	} else {
		if err := cmd.Start(); err != nil {
			return nil, Capabilities{}, failure(Internal)
		}
		pidfd, err = openProcessIdentity(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, Capabilities{}, failure(Internal)
		}
	}
	a := &Adapter{cmd: cmd, pidfd: pidfd, containment: containment, stdin: stdin, stdout: stdout, nextID: 1, pending: make(map[int64]pendingCall), done: make(chan struct{}), waitDone: make(chan struct{}), shutdown: opts.ShutdownTimeout, maxFrame: opts.MaxFrameBytes, events: make(chan Event, 256), policies: make(map[turnKey]*policy.Snapshot), approvals: make(map[string]pendingApproval)}
	go drainBounded(stderr, opts.MaxStderrBytes)
	go a.wait()
	go a.readLoop()
	startCtx, cancel := context.WithTimeout(ctx, opts.StartupTimeout)
	defer cancel()
	var response struct {
		UserAgent    string        `json:"userAgent"`
		Capabilities *Capabilities `json:"capabilities,omitempty"`
	}
	err = a.call(startCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "shipmates", "version": "1"}, "capabilities": map[string]any{"experimentalApi": false}}, &response)
	if err == nil {
		version, ok := parseVersion(response.UserAgent)
		if !ok || compareVersion(version, opts.MinVersion) < 0 || compareVersion(version, opts.MaxVersion) > 0 {
			err = failure(UnsupportedVersion)
		}
	}
	caps := requiredCapabilities()
	// A capability object is an optional fixture/forward-protocol extension. If
	// advertised it is authoritative; current pinned Codex versions are gated by
	// userAgent because their initialize response has no feature bitmap.
	if err == nil && response.Capabilities != nil && *response.Capabilities != caps {
		err = failure(UnsupportedCapability)
	}
	if err == nil {
		err = a.notify("initialized", nil)
	}
	if err != nil {
		if startCtx.Err() != nil && ctx.Err() == nil {
			err = failure(StartupTimeout)
		}
		cleanupCtx, cc := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cc()
		if closeErr := a.Close(cleanupCtx); closeErr != nil {
			return nil, Capabilities{}, closeErr
		}
		return nil, Capabilities{}, err
	}
	return a, caps, nil
}

func containedExecutable(path string) (string, error) {
	if filepath.Base(path) != "codex.js" {
		return path, nil
	}
	var packageName, target string
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		packageName, target = "codex-linux-x64", "x86_64-unknown-linux-musl"
	case "linux/arm64":
		packageName, target = "codex-linux-arm64", "aarch64-unknown-linux-musl"
	default:
		return "", errors.New("contained Codex native binary is unsupported on this platform")
	}
	packageRoot := filepath.Dir(filepath.Dir(path))
	candidates := []string{
		filepath.Join(filepath.Dir(packageRoot), packageName, "vendor", target, "bin", "codex"),
		filepath.Join(packageRoot, "vendor", target, "bin", "codex"),
	}
	for _, candidate := range candidates {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		f, err := os.Open(resolved)
		if err != nil {
			continue
		}
		var magic [4]byte
		_, readErr := io.ReadFull(f, magic[:])
		_ = f.Close()
		if readErr == nil && string(magic[:]) == "\x7fELF" {
			return resolved, nil
		}
	}
	return "", errors.New("contained Codex native binary is unavailable")
}

// ProcessGroupHandle is the narrow production cleanup seam used by bounded
// advisory workers. It exposes no command, prompt, PID, or protocol data.
type ProcessGroupHandle interface {
	GracefulTerminate(context.Context) error
	ForceKill(context.Context) error
	Wait(context.Context) error
}

type processGroupHandle struct{ adapter *Adapter }

func (a *Adapter) ProcessGroupHandle() ProcessGroupHandle {
	if a == nil {
		return nil
	}
	return &processGroupHandle{adapter: a}
}

func (h *processGroupHandle) GracefulTerminate(ctx context.Context) error {
	if h == nil || h.adapter == nil || h.adapter.pidfd <= 0 {
		return failure(Internal)
	}
	if err := signalProcessIdentity(h.adapter.pidfd, false); err != nil {
		return failure(CleanupFailed)
	}
	if !waitProcess(ctx, h.adapter.waitDone, h.adapter.shutdown) {
		return failure(CleanupFailed)
	}
	if h.adapter.containmentErr != nil {
		return failure(CleanupFailed)
	}
	return nil
}

func (h *processGroupHandle) ForceKill(ctx context.Context) error {
	if h == nil || h.adapter == nil || h.adapter.pidfd <= 0 {
		return failure(Internal)
	}
	if h.adapter.containment != nil {
		if err := h.adapter.containment.killCgroup(); err != nil {
			return failure(CleanupFailed)
		}
	}
	if err := signalProcessIdentity(h.adapter.pidfd, true); err != nil {
		return failure(CleanupFailed)
	}
	return h.Wait(ctx)
}

func (h *processGroupHandle) Wait(ctx context.Context) error {
	if h == nil || h.adapter == nil {
		return failure(Internal)
	}
	if !waitProcess(ctx, h.adapter.waitDone, h.adapter.shutdown) {
		return failure(CleanupFailed)
	}
	if h.adapter.containmentErr != nil {
		return failure(CleanupFailed)
	}
	return nil
}

func controlledEnvironment(extra map[string]string, credentialFree bool, transportCodexHome string) []string {
	allow := map[string]bool{"PATH": true, "HOME": !credentialFree, "CODEX_HOME": !credentialFree, "TMPDIR": true, "TMP": true, "TEMP": true, "SYSTEMROOT": true, "WINDIR": true}
	values := map[string]string{}
	for _, item := range os.Environ() {
		k, v, ok := strings.Cut(item, "=")
		if ok && allow[k] {
			values[k] = v
		}
	}
	for k, v := range extra {
		if credentialFree {
			switch k {
			case "PATH", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "WINDIR":
			default:
				continue
			}
		}
		if !strings.ContainsAny(k, "=\x00") && !strings.ContainsRune(v, 0) {
			values[k] = v
		}
	}
	if credentialFree && transportCodexHome != "" && filepath.IsAbs(transportCodexHome) && !strings.ContainsRune(transportCodexHome, 0) {
		values["CODEX_HOME"] = filepath.Clean(transportCodexHome)
	}
	values[ManagedSessionEnvironment] = "1"
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}

func drainBounded(r io.Reader, limit int) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, int64(limit)))
	_, _ = io.Copy(io.Discard, r)
}

func (a *Adapter) wait() {
	_ = a.cmd.Wait()
	if a.containment != nil {
		a.containmentErr = a.containment.Close()
	} else if a.pidfd > 0 {
		_ = closeProcessIdentity(a.pidfd)
	}
	close(a.waitDone)
}

func (a *Adapter) readLoop() {
	defer close(a.done)
	if a.events != nil {
		defer close(a.events)
	}
	r := bufio.NewReaderSize(a.stdout, 64*1024)
	for {
		line, err := readBoundedFrame(r, a.maxFrame)
		if errors.Is(err, errFrameTooLarge) {
			a.fail(failure(MalformedFrame))
			return
		}
		if err != nil {
			a.mu.Lock()
			closing := a.closing
			a.mu.Unlock()
			if !closing {
				// A child that stops talking is observed through two independent
				// events: stdout reaching EOF, and the reaper's cmd.Wait returning,
				// which makes os/exec close the parent end of this pipe. Whichever
				// wins the race, the meaning is identical, so a closed pipe must
				// classify as EOF and never as a frame fault. Only the adapter ever
				// closes this descriptor, so os.ErrClosed cannot come from the child.
				if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
					a.fail(failure(UnexpectedEOF))
				} else {
					a.fail(failure(MalformedFrame))
				}
			}
			return
		}
		if len(line) == 0 || len(line) > a.maxFrame {
			a.fail(failure(MalformedFrame))
			return
		}
		var m rpcMessage
		if json.Unmarshal(line, &m) != nil {
			a.fail(failure(MalformedFrame))
			return
		}
		if len(m.ID) > 0 && m.Method != "" {
			if !a.refuse(m) {
				a.fail(failure(ProtocolViolation))
				return
			}
			continue
		}
		if len(m.ID) > 0 && m.Method == "" {
			if !a.deliver(m) {
				a.fail(failure(ProtocolViolation))
				return
			}
			continue
		}
		if m.Method != "" {
			a.notification(m)
			continue
		} // unknown notifications carry no authority
		a.fail(failure(ProtocolViolation))
		return
	}
}

var errFrameTooLarge = errors.New("frame_too_large")

func readBoundedFrame(r *bufio.Reader, limit int) ([]byte, error) {
	if limit < 1 {
		return nil, errFrameTooLarge
	}
	frame := make([]byte, 0, min(limit, 64*1024))
	for {
		fragment, err := r.ReadSlice('\n')
		if len(fragment) > limit-len(frame) {
			return nil, errFrameTooLarge
		}
		frame = append(frame, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return frame, err
	}
}

var opaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

func validOpaqueID(id string) bool { return opaqueID.MatchString(id) }

// StartThread, ResumeThread, and StartTurn expose only the small operational
// surface required by the lifecycle owner. Sensitive text is protocol data.
func (a *Adapter) StartThread(ctx context.Context, opts ThreadOptions) (Thread, error) {
	params := threadParams(opts)
	var response struct {
		Thread Thread `json:"thread"`
	}
	if err := a.call(ctx, "thread/start", params, &response); err != nil {
		if ctx.Err() != nil {
			err = failure(RequestTimeout)
		}
		return Thread{}, err
	}
	if !validOpaqueID(response.Thread.ID) {
		return Thread{}, failure(ProtocolViolation)
	}
	return response.Thread, nil
}

func (a *Adapter) ResumeThread(ctx context.Context, threadID string, opts ThreadOptions) (Thread, error) {
	if !validOpaqueID(threadID) {
		return Thread{}, failure(ProtocolViolation)
	}
	params := threadParams(opts)
	params["threadId"] = threadID
	var response struct {
		Thread Thread `json:"thread"`
	}
	if err := a.call(ctx, "thread/resume", params, &response); err != nil {
		if ctx.Err() != nil {
			err = failure(RequestTimeout)
		}
		return Thread{}, err
	}
	if response.Thread.ID != threadID {
		return Thread{}, failure(ProtocolViolation)
	}
	return response.Thread, nil
}

func threadParams(opts ThreadOptions) map[string]any {
	sandbox := "workspace-write"
	if opts.ReadOnly {
		sandbox = "read-only"
	}
	params := map[string]any{
		"cwd": opts.WorkingDirectory, "approvalPolicy": "never",
		"sandbox": sandbox, "developerInstructions": opts.DeveloperInstructions,
	}
	if opts.Model != "" {
		params["model"] = opts.Model
	}
	if opts.Toolless {
		params["tools"] = []any{}
	}
	return params
}

func (a *Adapter) StartTurn(ctx context.Context, threadID string, input TurnInput) (Turn, error) {
	if !validOpaqueID(threadID) || input.Text == "" {
		return Turn{}, failure(ProtocolViolation)
	}
	if err := turninput.RevalidateDescriptors(input.Images); err != nil {
		return Turn{}, failure(BackendRejected)
	}
	var response struct {
		Turn Turn `json:"turn"`
	}
	items := make([]map[string]string, 0, 1+len(input.Images))
	items = append(items, map[string]string{"type": "text", "text": input.Text})
	for _, image := range input.Images {
		items = append(items, map[string]string{"type": "localImage", "path": image.AbsolutePath()})
	}
	params := map[string]any{"threadId": threadID, "input": items}
	if input.Effort != "" {
		params["effort"] = input.Effort
	}
	if err := a.callTurnStart(ctx, threadID, input.Policy, params, &response); err != nil {
		if ctx.Err() != nil {
			err = failure(RequestTimeout)
		}
		return Turn{}, err
	}
	if !validOpaqueID(response.Turn.ID) {
		return Turn{}, failure(ProtocolViolation)
	}
	return response.Turn, nil
}

// InterruptTurn targets an exact established tuple. It is internal plumbing
// for bounded owner shutdown; public interruption remains deferred.
func (a *Adapter) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	if !validOpaqueID(threadID) || !validOpaqueID(turnID) {
		return failure(ProtocolViolation)
	}
	var response map[string]any
	err := a.call(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, &response)
	if ctx.Err() != nil {
		return failure(RequestTimeout)
	}
	return err
}

// SteerTurn injects text into the exact active turn. Text remains protocol
// data and is never reflected in an Event.
func (a *Adapter) SteerTurn(ctx context.Context, threadID, turnID, text string) error {
	if !validOpaqueID(threadID) || !validOpaqueID(turnID) || strings.TrimSpace(text) == "" {
		return failure(ProtocolViolation)
	}
	var response map[string]any
	err := a.call(ctx, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "input": []map[string]string{{"type": "text", "text": text}}}, &response)
	if ctx.Err() != nil {
		return failure(RequestTimeout)
	}
	return err
}

func (a *Adapter) notification(m rpcMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Delta    string `json:"delta"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Query   string `json:"query"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	turnID := p.Turn.ID
	if turnID == "" {
		turnID = p.TurnID
	}
	if !validOpaqueID(p.ThreadID) || !validOpaqueID(turnID) {
		return
	}
	e := Event{ThreadID: p.ThreadID, TurnID: turnID}
	switch m.Method {
	case "turn/completed":
		e.Kind = TurnCompleted
	case "turn/failed":
		e.Kind = TurnFailed
	case "item/agentMessage/delta":
		e.Kind, e.Text, e.Partial = AgentMessage, p.Delta, true
	case "item/completed":
		if p.Item.Type != "agentMessage" {
			return
		}
		e.Kind, e.Text = AgentMessage, p.Item.Text
	case "item/started":
		e.Kind = Activity
		switch p.Item.Type {
		case "commandExecution":
			e.Category = "command"
			e.Detail = p.Item.Command
		case "fileChange":
			e.Category = "file_change"
			var changes []string
			for _, change := range p.Item.Changes {
				if change.Path != "" {
					changes = append(changes, strings.TrimSpace(change.Kind+" "+change.Path))
				}
			}
			e.Detail = strings.Join(changes, ", ")
		case "webSearch":
			e.Category = "web_search"
			e.Detail = p.Item.Query
		case "mcpToolCall":
			e.Category = "connected_tool"
		default:
			e.Category = "other"
		}
	default:
		return
	}
	select {
	case a.events <- e:
		if e.Kind == TurnCompleted || e.Kind == TurnFailed {
			a.policyMu.Lock()
			delete(a.policies, turnKey{threadID: e.ThreadID, turnID: e.TurnID})
			a.policyMu.Unlock()
		}
	default:
		a.fail(failure(ProtocolViolation))
	}
}

func parseID(raw json.RawMessage) (int64, bool) {
	var id int64
	if len(raw) == 0 || json.Unmarshal(raw, &id) != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
func (a *Adapter) deliver(m rpcMessage) bool {
	id, ok := parseID(m.ID)
	if !ok || (len(m.Result) == 0) == (len(m.RPCError) == 0) {
		return false
	}
	a.mu.Lock()
	pending, ok := a.pending[id]
	if ok {
		delete(a.pending, id)
	}
	a.mu.Unlock()
	if !ok {
		return false
	}
	// Install the immutable exact binding before the reader can process a
	// server request immediately following the turn/start response.
	if pending.snapshot != nil && len(m.Result) != 0 {
		var response struct {
			Turn Turn `json:"turn"`
		}
		if json.Unmarshal(m.Result, &response) == nil && validOpaqueID(response.Turn.ID) {
			a.policyMu.Lock()
			if a.policies == nil {
				a.policies = make(map[turnKey]*policy.Snapshot)
			}
			a.policies[turnKey{threadID: pending.turnThread, turnID: response.Turn.ID}] = pending.snapshot
			a.policyMu.Unlock()
		}
	}
	pending.ch <- m
	return true
}
func (a *Adapter) refuse(m rpcMessage) bool {
	id, ok := parseID(m.ID)
	if !ok {
		return false
	}
	var result any
	class := "unknown"
	event := Event{Kind: RequestRefused}
	var identity struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	identityOK := json.Unmarshal(m.Params, &identity) == nil && validOpaqueID(identity.ThreadID) && validOpaqueID(identity.TurnID)
	var snapshot *policy.Snapshot
	if identityOK {
		a.policyMu.RLock()
		snapshot = a.policies[turnKey{threadID: identity.ThreadID, turnID: identity.TurnID}]
		a.policyMu.RUnlock()
	}
	switch m.Method {
	case "item/commandExecution/requestApproval":
		class = "approval"
		result = map[string]string{"decision": "cancel"}
		if snapshot != nil {
			var params struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(m.Params, &params) == nil && params.Command != "" && len(params.Command) <= 16<<10 {
				explanation := policy.Evaluate(snapshot, policy.Request{Kind: policy.ProcessExec, CommandExact: params.Command})
				requestID := strconv.FormatInt(id, 10)
				a.mu.Lock()
				if a.approvals == nil {
					a.approvals = make(map[string]pendingApproval)
				}
				_, duplicate := a.approvals[requestID]
				if !duplicate {
					a.approvals[requestID] = pendingApproval{id: id, threadID: identity.ThreadID, turnID: identity.TurnID, policySnapshotID: snapshot.ID}
				}
				a.mu.Unlock()
				if duplicate {
					return false
				}
				event = Event{Kind: ApprovalRequested, ThreadID: identity.ThreadID, TurnID: identity.TurnID,
					PolicySnapshotID: snapshot.ID, PolicyEffect: explanation.PolicyEffect,
					MatchedRules: explanation.MatchedRules, OmittedMatches: explanation.OmittedMatches,
					BackendRequestID: requestID, CommandExact: params.Command, RequestClass: class}
				select {
				case a.events <- event:
					return true
				default:
					a.fail(failure(ProtocolViolation))
					return false
				}
			}
		}
	case "item/fileChange/requestApproval":
		class = "approval"
		result = map[string]string{"decision": "cancel"}
	case "execCommandApproval", "applyPatchApproval":
		class = "approval"
		result = map[string]string{"decision": "abort"}
	case "item/tool/requestUserInput":
		class = "user_input"
		result = map[string]any{"answers": map[string]any{}}
	default:
		return false
	}
	if a.write(map[string]any{"id": id, "result": result}) != nil {
		return false
	}
	// Requests without an exact immutable turn binding, including delayed and
	// mismatched requests, are refused on the wire but carry no attribution and
	// are deliberately omitted from the normalized event stream.
	if snapshot == nil {
		return true
	}
	if event.ThreadID == "" {
		event.ThreadID, event.TurnID = identity.ThreadID, identity.TurnID
		event.PolicySnapshotID = snapshot.ID
		event.ReasonCode = "unsupported_request"
	}
	event.RequestClass = class
	select {
	case a.events <- event:
	default:
		a.fail(failure(ProtocolViolation))
		return false
	}
	return true
}

// ResolveApproval correlates one response to the exact request and immutable
// turn-policy binding captured by the reader. It writes at most once.
func (a *Adapter) ResolveApproval(ctx context.Context, response ApprovalResponse, decision ApprovalDecision) (bool, error) {
	if decision != AllowOnce && decision != Deny {
		return false, failure(ProtocolViolation)
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	a.mu.Lock()
	p, ok := a.approvals[response.RequestID]
	if !ok || p.resolving || p.threadID != response.ThreadID || p.turnID != response.TurnID || p.policySnapshotID != response.PolicySnapshotID {
		a.mu.Unlock()
		return false, failure(ProtocolViolation)
	}
	p.resolving = true
	a.approvals[response.RequestID] = p
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		a.mu.Lock()
		if current, exists := a.approvals[response.RequestID]; exists && current == p {
			current.resolving = false
			a.approvals[response.RequestID] = current
		}
		a.mu.Unlock()
		return false, ctx.Err()
	default:
	}
	wire := "cancel"
	if decision == AllowOnce {
		wire = "accept"
	}
	err := a.write(map[string]any{"id": p.id, "result": map[string]string{"decision": wire}})
	a.mu.Lock()
	delete(a.approvals, response.RequestID)
	a.mu.Unlock()
	if err != nil {
		return false, err
	}
	return true, nil
}

func (a *Adapter) call(ctx context.Context, method string, params any, out any) error {
	return a.callBound(ctx, method, params, out, "", nil)
}

func (a *Adapter) callTurnStart(ctx context.Context, threadID string, snapshot *policy.Snapshot, params any, out any) error {
	return a.callBound(ctx, "turn/start", params, out, threadID, snapshot)
}

func (a *Adapter) callBound(ctx context.Context, method string, params any, out any, turnThread string, snapshot *policy.Snapshot) error {
	a.mu.Lock()
	id := a.nextID
	a.nextID++
	ch := make(chan rpcMessage, 1)
	a.pending[id] = pendingCall{ch: ch, turnThread: turnThread, snapshot: snapshot}
	a.mu.Unlock()
	if err := a.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		a.remove(id)
		return err
	}
	select {
	case m := <-ch:
		if len(m.RPCError) > 0 {
			// A well-formed correlated JSON-RPC error is an authoritative refusal,
			// not uncertain transport health. Backend error data remains opaque.
			return failure(BackendRejected)
		}
		if json.Unmarshal(m.Result, out) != nil {
			return failure(MalformedFrame)
		}
		return nil
	case <-a.done:
		a.mu.Lock()
		err := a.terminal
		a.mu.Unlock()
		if err == nil {
			err = failure(ChildExit)
		}
		return err
	case <-ctx.Done():
		a.remove(id)
		return ctx.Err()
	}
}

func (a *Adapter) remove(id int64) { a.mu.Lock(); delete(a.pending, id); a.mu.Unlock() }
func (a *Adapter) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return a.write(m)
}
func (a *Adapter) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return failure(Internal)
	}
	b = append(b, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return failure(ChildExit)
	}
	if _, err = a.stdin.Write(b); err != nil {
		return failure(ChildExit)
	}
	return nil
}
func (a *Adapter) fail(err error) {
	a.mu.Lock()
	if a.terminal == nil {
		a.terminal = err
		select {
		case a.events <- Event{Kind: AdapterFault, Code: ErrorCode(err)}:
		default:
		}
	}
	a.mu.Unlock()
}

// Close gracefully closes stdin, then terminates/kills and always waits for the
// owned child. It is safe to call concurrently and repeatedly.
func (a *Adapter) Close(ctx context.Context) error {
	a.closeOnce.Do(func() { a.mu.Lock(); a.closing = true; _ = a.stdin.Close(); a.mu.Unlock() })
	if waitProcess(ctx, a.waitDone, a.shutdown) {
		if a.containmentErr != nil {
			return failure(CleanupFailed)
		}
		return nil
	}
	h := a.ProcessGroupHandle()
	if h != nil {
		_ = h.GracefulTerminate(ctx)
	} else {
		_ = a.cmd.Process.Signal(os.Interrupt)
	}
	if waitProcess(ctx, a.waitDone, a.shutdown) {
		if a.containmentErr != nil {
			return failure(CleanupFailed)
		}
		return nil
	}
	if h != nil {
		_ = h.ForceKill(context.Background())
	} else {
		_ = a.cmd.Process.Kill()
	}
	if waitProcess(context.Background(), a.waitDone, a.shutdown) {
		if a.containmentErr != nil {
			return failure(CleanupFailed)
		}
		return nil
	}
	return failure(CleanupFailed)
}

func waitProcess(ctx context.Context, done <-chan struct{}, limit time.Duration) bool {
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

var versionRE = regexp.MustCompile(`(?:codex(?:-cli|_cli_rs)?[/ ]|codex-cli )?(\d+\.\d+\.\d+)`)

func parseVersion(s string) (string, bool) {
	m := versionRE.FindStringSubmatch(s)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}
func compareVersion(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
