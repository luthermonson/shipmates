package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/api"
	"github.com/rancher/remotedialer"
)

const (
	autonomousToken   = "autonomous-test-token"
	autonomousKey     = "voyager:captain"
	autonomousPlanID  = "voyage-001"
	autonomousPersona = "builder"
)

// TestAutonomousFleetSinglePlanVoyageSuccess is the primary acceptance test.
// It exercises a real authenticated websocket tunnel and requires one plan to
// make the full voyage: discovery -> dispatch -> blocked -> approved -> working
// -> completed -> observable as done.
func TestAutonomousFleetSinglePlanVoyageSuccess(t *testing.T) {
	h := newAutonomousFleetHarness(t)

	var plans []autonomousPlan
	h.mustJSON(http.MethodGet, "/api/beads", nil, http.StatusOK, &plans)
	if len(plans) != 1 || plans[0].ID != autonomousPlanID {
		t.Fatalf("plan discovery = %#v", plans)
	}

	dispatch := map[string]string{
		"ship": autonomousKey, "persona": autonomousPersona, "title": plans[0].Title,
	}
	var dispatched map[string]string
	h.mustJSON(http.MethodPost,
		"/api/captain/"+autonomousKey+"/bead/"+autonomousPlanID+"/assign",
		dispatch, http.StatusOK, &dispatched)
	if dispatched["dispatched"] != "true" || dispatched["assignee"] != "builder@voyager" {
		t.Fatalf("dispatch response = %#v", dispatched)
	}

	eventually(t, 4*time.Second, "voyage to block on its safety gate", func() bool {
		var pending []api.FleetPending
		if !h.tryJSON(http.MethodGet, "/api/pending", nil, &pending) {
			return false
		}
		return len(pending) == 1 && pending[0].ID == "voyage-gate" && pending[0].Persona == autonomousPersona
	})
	assertFleetStatus(t, h, autonomousPersona, "blocked")

	h.mustJSON(http.MethodPost,
		"/api/captain/"+autonomousKey+"/resolve/voyage-gate",
		map[string]string{"behavior": "allow"}, http.StatusOK, nil)
	eventually(t, 4*time.Second, "approved voyage to start work", func() bool {
		return fleetStatusIs(h, autonomousPersona, "working")
	})

	h.captain.completeVoyage()
	eventually(t, 4*time.Second, "voyage to complete", func() bool {
		return fleetStatusIs(h, autonomousPersona, "done")
	})

	plans = nil
	h.mustJSON(http.MethodGet, "/api/beads", nil, http.StatusOK, &plans)
	if len(plans) != 0 {
		t.Fatalf("completed plan remained in open fleet graph: %#v", plans)
	}
	var completed autonomousPlan
	h.mustJSON(http.MethodGet,
		"/api/captain/"+autonomousKey+"/bead/"+autonomousPlanID,
		nil, http.StatusOK, &completed)
	if completed.Status != "closed" || completed.Assignee != "builder@voyager" {
		t.Fatalf("completed plan = %#v", completed)
	}

	event := h.waitSSEEvent(t, "voyage:completed")
	if event.Persona != autonomousPersona || event.Text != autonomousPlanID {
		t.Fatalf("completion event = %#v", event)
	}
	if !h.captain.wasTold(autonomousPersona, "bd show "+autonomousPlanID) {
		t.Fatal("dispatch did not deliver the plan context to the assigned mate")
	}
}

// TestAutonomousFleetFeatureMatrix covers the operator-facing surfaces around
// the voyage. All upstream AI/audio services are deterministic local fakes.
func TestAutonomousFleetFeatureMatrix(t *testing.T) {
	h := newAutonomousFleetHarness(t)

	t.Run("public and authenticated boundaries", func(t *testing.T) {
		h.mustStatus(http.MethodGet, "/health", nil, false, http.StatusOK)
		h.mustStatus(http.MethodGet, "/api.js", nil, false, http.StatusOK)
		h.mustStatus(http.MethodGet, "/api/captains", nil, false, http.StatusUnauthorized)
		var captains []api.CaptainStatus
		h.mustJSON(http.MethodGet, "/api/captains", nil, http.StatusOK, &captains)
		if len(captains) != 1 || captains[0].ClientKey != autonomousKey || !captains[0].Connected {
			t.Fatalf("captains = %#v", captains)
		}
	})

	t.Run("fleet reads", func(t *testing.T) {
		h.mustStatus(http.MethodGet, "/api/status", nil, true, http.StatusOK)
		h.mustStatus(http.MethodGet, "/api/pending", nil, true, http.StatusOK)
		h.mustStatus(http.MethodGet, "/api/captain/"+autonomousKey+"/feed", nil, true, http.StatusOK)
		h.mustStatus(http.MethodGet, "/api/captain/"+autonomousKey+"/beads", nil, true, http.StatusOK)
		var summary map[string]int
		h.mustJSON(http.MethodGet, "/api/captain/"+autonomousKey+"/beads/summary", nil, http.StatusOK, &summary)
		if summary["open"] != 1 {
			t.Fatalf("beads summary = %#v", summary)
		}
		var policy FleetPolicy
		h.mustJSON(http.MethodGet, "/api/fleet-policy", nil, http.StatusOK, &policy)
		if len(policy.Deny) != 1 || policy.Deny[0] != "Bash(rm *)" {
			t.Fatalf("fleet policy = %#v", policy)
		}
	})

	t.Run("pty controls", func(t *testing.T) {
		base := "/api/captain/" + autonomousKey + "/pty/" + autonomousPersona
		for _, tc := range []struct {
			method string
			path   string
		}{
			{http.MethodPost, "/start"},
			{http.MethodPost, "/input"},
			{http.MethodPost, "/resize"},
			{http.MethodPost, "/takeover"},
			{http.MethodPost, "/release"},
			{http.MethodGet, "/snapshot"},
		} {
			h.mustStatus(tc.method, base+tc.path, []byte(`{}`), true, http.StatusOK)
		}
		if got := h.captain.ptyOperationCount(); got != 6 {
			t.Fatalf("PTY operations = %d, want 6", got)
		}
		resp, stream := h.request(http.MethodGet, base+"/stream", nil, true, "")
		if resp.StatusCode != http.StatusOK || !bytes.Contains(stream, []byte("ZmFrZS10ZXJtaW5hbA==")) {
			t.Fatalf("PTY stream status=%d body=%q", resp.StatusCode, stream)
		}
	})

	t.Run("attachment relay and auto tell", func(t *testing.T) {
		body, contentType := autonomousMultipart(t, "voyage.png", []byte("fake-image"), "inspect this")
		resp, raw := h.request(http.MethodPost,
			"/api/captain/"+autonomousKey+"/attach", body.Bytes(), true, contentType)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attach status %d: %s", resp.StatusCode, raw)
		}
		if h.captain.attachmentCount() != 1 || !h.captain.wasTold("captain", "inspect this") {
			t.Fatal("attachment was not relayed and announced")
		}
	})

	t.Run("voice, conversation, STT, and TTS", func(t *testing.T) {
		var caps map[string]bool
		h.mustJSON(http.MethodGet, "/api/voice/config", nil, http.StatusOK, &caps)
		for _, capability := range []string{"conversation", "stt", "tts"} {
			if !caps[capability] {
				t.Fatalf("voice capability %q disabled: %#v", capability, caps)
			}
		}
		var transcript map[string]string
		h.mustJSONBytes(http.MethodPost, "/api/stt", []byte("fake-wav"), "audio/wav", http.StatusOK, &transcript)
		if transcript["text"] != "dispatch the voyage" {
			t.Fatalf("transcript = %#v", transcript)
		}
		resp, audio := h.request(http.MethodPost, "/api/tts", []byte(`{"text":"voyage complete"}`), true, "application/json")
		if resp.StatusCode != http.StatusOK || string(audio) != "FAKE-MP3" {
			t.Fatalf("tts status=%d body=%q", resp.StatusCode, audio)
		}
		var conversation conversationResponse
		h.mustJSON(http.MethodPost, "/api/conversation", map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "report fleet status"}},
		}, http.StatusOK, &conversation)
		if conversation.Reply != "Aye, Admiral. Voyager is on watch." || len(conversation.ToolsCalled) != 1 {
			t.Fatalf("conversation = %#v", conversation)
		}
		if h.voice.chatCalls.Load() != 2 {
			t.Fatalf("LLM calls = %d, want 2", h.voice.chatCalls.Load())
		}
	})

	t.Run("beads nudge", func(t *testing.T) {
		h.mustStatus(http.MethodPost, "/api/beads/nudge", []byte(`{"from":"voyager:captain"}`), true, http.StatusAccepted)
	})

	t.Run("bead mutations", func(t *testing.T) {
		h.mustStatus(http.MethodPost, "/api/captain/"+autonomousKey+"/bead", []byte(`{"title":"follow-up"}`), true, http.StatusCreated)
		h.mustStatus(http.MethodPost, "/api/captain/"+autonomousKey+"/bead/"+autonomousPlanID+"/update", []byte(`{"assignee":"tester@voyager"}`), true, http.StatusOK)
		h.mustStatus(http.MethodPost, "/api/captain/"+autonomousKey+"/bead/"+autonomousPlanID+"/close", []byte(`{}`), true, http.StatusOK)
	})
}

func TestAutonomousFleetFailurePaths(t *testing.T) {
	h := newAutonomousFleetHarness(t)

	h.mustStatus(http.MethodPost,
		"/api/captain/"+autonomousKey+"/bead/not%20valid/assign",
		[]byte(`{}`), true, http.StatusBadRequest)
	h.mustStatus(http.MethodGet, "/api/captain/unknown/pending", nil, true, http.StatusNotFound)

	var queued map[string]string
	h.mustJSON(http.MethodPost,
		"/api/captain/"+autonomousKey+"/bead/"+autonomousPlanID+"/assign",
		map[string]string{"ship": "offline:captain", "persona": "builder"},
		http.StatusOK, &queued)
	if queued["queued"] != "true" {
		t.Fatalf("offline dispatch = %#v", queued)
	}
	h.fleet.mu.Lock()
	queueLen := len(h.fleet.dispatchQ)
	h.fleet.mu.Unlock()
	if queueLen != 1 {
		t.Fatalf("queued dispatches = %d, want 1", queueLen)
	}

	disabled, err := New(Options{PolicyPath: t.TempDir() + "/missing.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	disabledHTTP := httptest.NewServer(disabled.Handler())
	defer disabledHTTP.Close()
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/conversation", http.StatusServiceUnavailable},
		{"/api/stt", http.StatusNotImplemented},
		{"/api/tts", http.StatusServiceUnavailable},
	} {
		resp, err := http.Post(disabledHTTP.URL+tc.path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s status=%d want=%d", tc.path, resp.StatusCode, tc.want)
		}
	}

	h.cancelTunnel()
	eventually(t, 5*time.Second, "captain to become disconnected", func() bool {
		var captains []api.CaptainStatus
		return h.tryJSON(http.MethodGet, "/api/captains", nil, &captains) &&
			len(captains) == 1 && !captains[0].Connected
	})
	h.mustStatus(http.MethodGet, "/api/captain/"+autonomousKey+"/pending", nil, true, http.StatusGatewayTimeout)
}

type autonomousFleetHarness struct {
	t           *testing.T
	fleet       *Server
	fleetHTTP   *httptest.Server
	captain     *autonomousCaptain
	captainHTTP *httptest.Server
	voice       *autonomousVoice
	voiceHTTP   *httptest.Server
	client      *http.Client
	cancel      context.CancelFunc
	cancelOnce  sync.Once
}

func newAutonomousFleetHarness(t *testing.T) *autonomousFleetHarness {
	t.Helper()
	voice := &autonomousVoice{}
	voiceHTTP := httptest.NewServer(voice.handler())
	policyPath := t.TempDir() + "/fleet-policy.yaml"
	if err := os.WriteFile(policyPath, []byte("deny:\n  - 'Bash(rm *)'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fleet, err := New(Options{
		Token: autonomousToken, PolicyPath: policyPath,
		LLMURL: voiceHTTP.URL + "/v1", LLMModel: "fake-commodore",
		STTURL: voiceHTTP.URL + "/stt", STTModel: "fake-stt",
		TTSURL: voiceHTTP.URL + "/tts", TTSModel: "fake-tts", TTSVoice: "fake-voice",
	})
	if err != nil {
		t.Fatal(err)
	}
	fleetHTTP := httptest.NewServer(fleet.Handler())
	captain := newAutonomousCaptain()
	captainHTTP := httptest.NewServer(captain.handler())

	ctx, cancel := context.WithCancel(context.Background())
	h := &autonomousFleetHarness{
		t: t, fleet: fleet, fleetHTTP: fleetHTTP,
		captain: captain, captainHTTP: captainHTTP,
		voice: voice, voiceHTTP: voiceHTTP,
		client: &http.Client{Timeout: 5 * time.Second}, cancel: cancel,
	}
	t.Cleanup(func() {
		h.cancelTunnel()
		captainHTTP.Close()
		fleetHTTP.Close()
		voiceHTTP.Close()
	})

	captainURL, err := url.Parse(captainHTTP.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(captainURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+autonomousToken)
	headers.Set("X-Shipmates-Identity", autonomousKey)
	headers.Set("X-Shipmates-Repo", "voyager")
	headers.Set("X-Shipmates-Install-ID", "autonomous-install")
	headers.Set("X-Shipmates-Persona", "captain")
	headers.Set("X-Shipmates-Port", portText)
	wsURL := "ws" + strings.TrimPrefix(fleetHTTP.URL, "http") + "/connect"
	go func() {
		_ = remotedialer.ClientConnect(ctx, wsURL, headers, nil,
			func(proto, address string) bool {
				return proto == "tcp" && address == captainURL.Host
			}, nil)
	}()

	eventually(t, 6*time.Second, "captain tunnel to connect", func() bool {
		var captains []api.CaptainStatus
		return h.tryJSON(http.MethodGet, "/api/captains", nil, &captains) &&
			len(captains) == 1 && captains[0].ClientKey == autonomousKey && captains[0].Connected
	})
	return h
}

func (h *autonomousFleetHarness) cancelTunnel() {
	h.cancelOnce.Do(h.cancel)
}

func (h *autonomousFleetHarness) request(method, path string, body []byte, auth bool, contentType string) (*http.Response, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.fleetHTTP.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+autonomousToken)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, raw
}

func (h *autonomousFleetHarness) mustStatus(method, path string, body []byte, auth bool, want int) {
	h.t.Helper()
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	resp, raw := h.request(method, path, body, auth, contentType)
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s status=%d want=%d body=%s", method, path, resp.StatusCode, want, raw)
	}
}

func (h *autonomousFleetHarness) mustJSON(method, path string, in any, want int, out any) {
	h.t.Helper()
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	h.mustJSONBytes(method, path, body, "application/json", want, out)
}

func (h *autonomousFleetHarness) mustJSONBytes(method, path string, body []byte, contentType string, want int, out any) {
	h.t.Helper()
	resp, raw := h.request(method, path, body, true, contentType)
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s status=%d want=%d body=%s", method, path, resp.StatusCode, want, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("decode %s: %v; body=%s", path, err, raw)
		}
	}
}

func (h *autonomousFleetHarness) tryJSON(method, path string, in, out any) bool {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.fleetHTTP.URL+path, reader)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+autonomousToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false
	}
	return out == nil || json.NewDecoder(resp.Body).Decode(out) == nil
}

func (h *autonomousFleetHarness) waitSSEEvent(t *testing.T, eventType string) api.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		h.fleetHTTP.URL+"/api/captain/"+autonomousKey+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+autonomousToken)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event api.Event
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Type == eventType {
			return event
		}
	}
	t.Fatalf("SSE event %q not observed: %v", eventType, scanner.Err())
	return api.Event{}
}

func eventually(t *testing.T, timeout time.Duration, description string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func assertFleetStatus(t *testing.T, h *autonomousFleetHarness, persona, want string) {
	t.Helper()
	if !fleetStatusIs(h, persona, want) {
		var statuses []api.FleetMateStatus
		h.mustJSON(http.MethodGet, "/api/status", nil, http.StatusOK, &statuses)
		t.Fatalf("status for %s never became %s: %#v", persona, want, statuses)
	}
}

func fleetStatusIs(h *autonomousFleetHarness, persona, want string) bool {
	var statuses []api.FleetMateStatus
	if !h.tryJSON(http.MethodGet, "/api/status", nil, &statuses) {
		return false
	}
	for _, status := range statuses {
		if status.Persona == persona {
			return status.Status == want
		}
	}
	return false
}

type autonomousPlan struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Priority *int     `json:"priority,omitempty"`
	Assignee string   `json:"assignee,omitempty"`
	Ships    []string `json:"ships,omitempty"`
}

type autonomousCaptain struct {
	mu          sync.Mutex
	plan        autonomousPlan
	pending     map[string]api.Pending
	events      []api.Event
	status      string
	tells       []autonomousTell
	ptyOps      []string
	attachments int
}

type autonomousTell struct {
	persona string
	message string
}

func newAutonomousCaptain() *autonomousCaptain {
	priority := 1
	return &autonomousCaptain{
		plan:    autonomousPlan{ID: autonomousPlanID, Title: "Ship one deterministic plan", Status: "open", Priority: &priority},
		pending: map[string]api.Pending{}, status: "off",
	}
}

func (c *autonomousCaptain) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /events", c.handleEvents)
	mux.HandleFunc("GET /feed", c.handleFeed)
	mux.HandleFunc("GET /pending", c.handlePending)
	mux.HandleFunc("POST /resolve/{id}", c.handleResolve)
	mux.HandleFunc("GET /status.json", c.handleStatus)
	mux.HandleFunc("GET /beads.json", c.handleBeads)
	mux.HandleFunc("GET /beads/summary", c.handleBeadsSummary)
	mux.HandleFunc("GET /bead/{id}", c.handleBeadShow)
	mux.HandleFunc("POST /bead/{id}/update", c.handleBeadUpdate)
	mux.HandleFunc("POST /bead/{id}/close", c.handleBeadClose)
	mux.HandleFunc("POST /bead", c.handleBeadCreate)
	mux.HandleFunc("POST /beads/pull", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /tell/{persona}", c.handleTell)
	mux.HandleFunc("POST /attach", c.handleAttach)
	mux.HandleFunc("POST /pty/{persona}/start", c.handlePTY)
	mux.HandleFunc("POST /pty/{persona}/input", c.handlePTY)
	mux.HandleFunc("POST /pty/{persona}/resize", c.handlePTY)
	mux.HandleFunc("POST /pty/{persona}/takeover", c.handlePTY)
	mux.HandleFunc("POST /pty/{persona}/release", c.handlePTY)
	mux.HandleFunc("GET /pty/{persona}/snapshot", c.handlePTY)
	mux.HandleFunc("GET /pty/{persona}/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ZmFrZS10ZXJtaW5hbA==\n\n")
	})
	return mux
}

func (c *autonomousCaptain) addEventLocked(persona, eventType, text string) {
	seq := uint64(len(c.events) + 1)
	c.events = append(c.events, api.Event{
		Seq: seq, Time: time.Now().UTC().Format(time.RFC3339Nano),
		Persona: persona, Type: eventType, Text: text,
	})
}

func (c *autonomousCaptain) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	c.mu.Lock()
	out := make([]api.Event, 0, len(c.events))
	for _, event := range c.events {
		if event.Seq > after {
			out = append(out, event)
		}
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (c *autonomousCaptain) handleFeed(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range c.events {
		_, _ = fmt.Fprintf(w, "[%s] %s/%s: %s\n", event.Time, event.Persona, event.Type, event.Text)
	}
}

func (c *autonomousCaptain) handlePending(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	out := make([]api.Pending, 0, len(c.pending))
	for _, pending := range c.pending {
		out = append(out, pending)
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (c *autonomousCaptain) handleResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Behavior string `json:"behavior"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c.mu.Lock()
	defer c.mu.Unlock()
	id := r.PathValue("id")
	if _, ok := c.pending[id]; !ok {
		http.Error(w, "no such pending request", http.StatusNotFound)
		return
	}
	delete(c.pending, id)
	if body.Behavior == "allow" {
		c.status = "working"
		c.addEventLocked(autonomousPersona, "voyage:working", autonomousPlanID)
	} else {
		c.status = "done"
		c.addEventLocked(autonomousPersona, "voyage:denied", autonomousPlanID)
	}
	w.WriteHeader(http.StatusOK)
}

func (c *autonomousCaptain) handleStatus(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	status := c.status
	var pendingID string
	if status == "blocked" {
		pendingID = "voyage-gate"
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, []api.MateStatus{{Persona: autonomousPersona, Status: status, PendingID: pendingID}})
}

func (c *autonomousCaptain) handleBeads(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	plan := c.plan
	c.mu.Unlock()
	if plan.Status == "closed" {
		writeJSON(w, http.StatusOK, []autonomousPlan{})
		return
	}
	writeJSON(w, http.StatusOK, []autonomousPlan{plan})
}

func (c *autonomousCaptain) handleBeadsSummary(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	open := 0
	if c.plan.Status != "closed" {
		open = 1
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"open": open})
}

func (c *autonomousCaptain) handleBeadShow(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	plan := c.plan
	c.mu.Unlock()
	if r.PathValue("id") != plan.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (c *autonomousCaptain) handleBeadUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Assignee string `json:"assignee"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.PathValue("id") != c.plan.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	c.plan.Assignee = body.Assignee
	c.addEventLocked("(fleet)", "bead:update", c.plan.ID)
	w.WriteHeader(http.StatusOK)
}

func (c *autonomousCaptain) handleBeadClose(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.PathValue("id") != c.plan.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	c.plan.Status = "closed"
	w.WriteHeader(http.StatusOK)
}

func (c *autonomousCaptain) handleBeadCreate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusCreated, c.plan)
}

func (c *autonomousCaptain) handleTell(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	persona := r.PathValue("persona")
	c.mu.Lock()
	c.tells = append(c.tells, autonomousTell{persona: persona, message: body.Message})
	if strings.Contains(body.Message, "[fleet dispatch]") && strings.Contains(body.Message, autonomousPlanID) {
		c.status = "blocked"
		c.pending["voyage-gate"] = api.Pending{
			ID: "voyage-gate", Persona: autonomousPersona,
			Tool: "Bash", Input: "go test ./...",
		}
		c.addEventLocked(autonomousPersona, "voyage:blocked", autonomousPlanID)
	}
	c.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (c *autonomousCaptain) handleAttach(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(11 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(file)
	_ = file.Close()
	c.mu.Lock()
	c.attachments++
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, AttachResponse{
		AttachID: "voyage-attach", Path: ".shipmates/inbox/voyage.png", Size: int64(len(raw)),
	})
}

func (c *autonomousCaptain) handlePTY(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.ptyOps = append(c.ptyOps, r.Method+" "+r.URL.Path)
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (c *autonomousCaptain) completeVoyage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status != "working" {
		panic("completeVoyage called before voyage was working")
	}
	c.plan.Status = "closed"
	c.status = "done"
	c.addEventLocked(autonomousPersona, "voyage:completed", autonomousPlanID)
}

func (c *autonomousCaptain) wasTold(persona, contains string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tell := range c.tells {
		if tell.persona == persona && strings.Contains(tell.message, contains) {
			return true
		}
	}
	return false
}

func (c *autonomousCaptain) ptyOperationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ptyOps)
}

func (c *autonomousCaptain) attachmentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attachments
}

type autonomousVoice struct{ chatCalls atomic.Int32 }

func (v *autonomousVoice) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		call := v.chatCalls.Add(1)
		if call == 1 {
			writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id": "fleet-list", "type": "function",
						"function": map[string]string{"name": "list_captains", "arguments": "{}"},
					}},
				},
			}}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]string{"role": "assistant", "content": "Aye, Admiral. Voyager is on watch."},
		}}})
	})
	mux.HandleFunc("POST /stt", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": "  dispatch   the voyage \n"})
	})
	mux.HandleFunc("POST /tts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("FAKE-MP3"))
	})
	return mux
}

func autonomousMultipart(t *testing.T, filename string, payload []byte, caption string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(payload)
	_ = w.WriteField("caption", caption)
	_ = w.Close()
	return buf, w.FormDataContentType()
}
