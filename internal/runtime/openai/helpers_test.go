package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// fakeEndpoint is an httptest server that speaks the chat-completions and SSE
// shapes. Every test in this package drives the real client path through it —
// there is no mocking of the HTTP layer, because the HTTP layer and the SSE
// parser are the parts most likely to be wrong.
type fakeEndpoint struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Path          string
	Method        string
	Authorization string
	Organization  string
	Accept        string
	Extra         http.Header
	Body          chatRequest
	RawBody       string
}

// newFakeEndpoint starts a server whose /chat/completions handler is h.
func newFakeEndpoint(t *testing.T, h func(w http.ResponseWriter, r *http.Request, rec recordedRequest)) *fakeEndpoint {
	t.Helper()
	f := &fakeEndpoint{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		_ = json.Unmarshal(raw, &body)
		rec := recordedRequest{
			Path:          r.URL.Path,
			Method:        r.Method,
			Authorization: r.Header.Get("Authorization"),
			Organization:  r.Header.Get("OpenAI-Organization"),
			Accept:        r.Header.Get("Accept"),
			Extra:         r.Header.Clone(),
			Body:          body,
			RawBody:       string(raw),
		}
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		f.mu.Unlock()
		h(w, r, rec)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"test-model"}]}`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeEndpoint) baseURL() string { return f.Server.URL + "/v1" }

func (f *fakeEndpoint) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// sseWriter writes SSE frames with a flush after each, so the client sees them
// incrementally the way a real inference server delivers tokens.
type sseWriter struct {
	t testing.TB
	w http.ResponseWriter
	f http.Flusher
}

func newSSEWriter(t testing.TB, w http.ResponseWriter) *sseWriter {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("ResponseWriter is not a Flusher")
	}
	return &sseWriter{t: t, w: w, f: fl}
}

// raw writes a literal line plus the blank separator line.
func (s *sseWriter) raw(line string) {
	fmt.Fprintf(s.w, "%s\n\n", line)
	s.f.Flush()
}

// delta writes a normal content delta frame.
func (s *sseWriter) delta(text string) {
	payload, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"model":   "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}},
	})
	if err != nil {
		s.t.Fatalf("marshal delta: %v", err)
	}
	s.raw("data: " + string(payload))
}

// finish writes the terminating frame(s): a finish_reason choice, optional
// usage, then [DONE].
func (s *sseWriter) finish(reason string, usage *usageBody) {
	chunk := map[string]any{
		"id":      "chatcmpl-test",
		"model":   "test-model",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	}
	if usage != nil {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	payload, _ := json.Marshal(chunk)
	s.raw("data: " + string(payload))
	s.raw("data: [DONE]")
}

// testConfig is a Config pointed at a fake endpoint, with bounds small enough
// that tests hit them quickly.
func testConfig(base string) Config {
	cfg := Config{
		BaseURL:               base,
		Model:                 "test-model",
		Timeout:               10 * time.Second,
		MaxResponseBytes:      64 << 10,
		MaxLineBytes:          4096,
		MaxTranscriptBytes:    DefaultMaxTranscriptBytes,
		MaxTranscriptMessages: DefaultMaxTranscriptMessages,
		MaxPromptBytes:        DefaultMaxPromptBytes,
		MaxSystemPromptBytes:  DefaultMaxSystemPromptBytes,
	}
	return cfg
}

func newTestClient(t *testing.T, cfg Config) *client {
	t.Helper()
	c, err := newClient(cfg)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

// newTestRuntime builds a Runtime against cfg and closes it on cleanup.
func newTestRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		_ = r.Close(ctx)
	})
	return r
}

// turnResult is everything a turn emitted, collected up to and including its
// TurnDone.
type turnResult struct {
	Texts     []string
	Reasoning []string
	Errors    []ErrorEvent
	Done      TurnDone
	All       []runtime.Event
}

func (tr turnResult) Text() string { return strings.Join(tr.Texts, "") }

// collectTurn drains the event channel until a KindTurnDone arrives.
func collectTurn(t *testing.T, evs <-chan runtime.Event, timeout time.Duration) turnResult {
	t.Helper()
	var out turnResult
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				t.Fatalf("event channel closed before turn_done (collected %d events)", len(out.All))
			}
			out.All = append(out.All, ev)
			switch p := ev.Payload.(type) {
			case TextDelta:
				out.Texts = append(out.Texts, p.Text)
			case ReasoningDelta:
				out.Reasoning = append(out.Reasoning, p.Text)
			case ErrorEvent:
				out.Errors = append(out.Errors, p)
			case TurnDone:
				out.Done = p
				return out
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for turn_done (collected %d events)", timeout, len(out.All))
		}
	}
}

// eventStrings flattens every string a consumer could plausibly see, for leak
// assertions.
func eventStrings(evs []runtime.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%v", ev.Payload))
		switch p := ev.Payload.(type) {
		case TextDelta:
			out = append(out, p.Text)
		case ReasoningDelta:
			out = append(out, p.Text)
		case ErrorEvent:
			out = append(out, p.Message, p.Code, p.Type)
		case TurnDone:
			out = append(out, p.Text, p.FinishReason, p.Refusal, p.Model)
		case SessionClosed:
			out = append(out, p.Reason)
		}
	}
	return out
}
