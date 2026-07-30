package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A normal streamed turn: deltas arrive incrementally, finish_reason and usage
// are captured, and the request we sent is the minimal compatible body.
func TestStreamChat_NormalStream(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("Hoisting ")
		s.delta("the ")
		s.delta("mainsail.")
		s.finish("stop", &usageBody{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15})
	})
	cfg := testConfig(srv.baseURL())
	c := newTestClient(t, cfg)

	var deltas []string
	res, err := c.streamChat(context.Background(), chatRequest{
		Model:    cfg.Model,
		Messages: []message{{Role: roleSystem, Content: "be a boat"}, {Role: roleUser, Content: "hello"}},
		Stream:   true,
	}, func(s string) { deltas = append(deltas, s) }, nil)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if res.Text != "Hoisting the mainsail." {
		t.Errorf("Text = %q", res.Text)
	}
	if len(deltas) != 3 {
		t.Errorf("expected 3 incremental deltas, got %d: %q", len(deltas), deltas)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
	if res.Usage != (Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}) {
		t.Errorf("Usage = %+v", res.Usage)
	}
	if res.MalformedChunks != 0 || res.Truncated {
		t.Errorf("unexpected flags: %+v", res)
	}

	reqs := srv.recorded()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/v1/chat/completions" || got.Method != http.MethodPost {
		t.Errorf("wrong request line: %s %s", got.Method, got.Path)
	}
	if !got.Body.Stream {
		t.Error("stream:true not sent")
	}
	if got.Body.Model != "test-model" {
		t.Errorf("model = %q", got.Body.Model)
	}
	if len(got.Body.Messages) != 2 || got.Body.Messages[0].Role != "system" || got.Body.Messages[1].Content != "hello" {
		t.Errorf("messages = %+v", got.Body.Messages)
	}
	if got.Accept != "text/event-stream" {
		t.Errorf("Accept = %q", got.Accept)
	}
	// Compatibility: nothing beyond the universally-supported fields.
	for _, unwanted := range []string{"stream_options", "tools", "tool_choice", "max_completion_tokens", "response_format"} {
		if strings.Contains(got.RawBody, unwanted) {
			t.Errorf("request body contains %q, which not every compatible server accepts: %s", unwanted, got.RawBody)
		}
	}
	// No credential configured, so no Authorization header at all.
	if got.Authorization != "" {
		t.Errorf("Authorization sent for a no-auth endpoint: %q", got.Authorization)
	}
}

func TestStreamChat_SendsAuthAndHeaders(t *testing.T) {
	const secret = "sk-enterprise-0123456789"
	t.Setenv("SHIPMATES_TEST_KEY", secret)
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("ok")
		s.finish("stop", nil)
	})
	cfg := testConfig(srv.baseURL())
	cfg.APIKeyEnv = "SHIPMATES_TEST_KEY"
	cfg.Organization = "org-gpu"
	cfg.Headers = map[string]string{"X-Tenant": "fleet-7"}
	c := newTestClient(t, cfg)

	if _, err := c.streamChat(context.Background(), chatRequest{Model: cfg.Model, Stream: true}, nil, nil); err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	got := srv.recorded()[0]
	if got.Authorization != "Bearer "+secret {
		t.Errorf("Authorization = %q", got.Authorization)
	}
	if got.Organization != "org-gpu" {
		t.Errorf("OpenAI-Organization = %q", got.Organization)
	}
	if got.Extra.Get("X-Tenant") != "fleet-7" {
		t.Errorf("X-Tenant = %q", got.Extra.Get("X-Tenant"))
	}
	// Operator headers must not be able to displace the ones we own.
	if got.Extra.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", got.Extra.Get("Content-Type"))
	}
}

// A configured-but-empty key env var fails before any request leaves the
// process, with an error that names the variable.
func TestStreamChat_MissingAPIKeyEnv(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		t.Error("request should not have been sent without a key")
	})
	cfg := testConfig(srv.baseURL())
	cfg.APIKeyEnv = "SHIPMATES_TEST_ABSENT_KEY"
	t.Setenv("SHIPMATES_TEST_ABSENT_KEY", "")
	c := newTestClient(t, cfg)

	_, err := c.streamChat(context.Background(), chatRequest{Model: cfg.Model, Stream: true}, nil, nil)
	if err == nil {
		t.Fatal("expected an error for an empty key env var")
	}
	if !strings.Contains(err.Error(), "SHIPMATES_TEST_ABSENT_KEY") {
		t.Errorf("error should name the env var: %v", err)
	}
	if len(srv.recorded()) != 0 {
		t.Errorf("a request was sent anyway: %+v", srv.recorded())
	}
}

func TestStreamChat_HTTPErrorWithJSONBody(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model \"nope\" not found","type":"invalid_request_error","code":404}}`))
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	_, err := c.streamChat(context.Background(), chatRequest{Model: "nope", Stream: true}, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError: %v", err, err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("Status = %d", apiErr.Status)
	}
	if apiErr.Type != "invalid_request_error" {
		t.Errorf("Type = %q", apiErr.Type)
	}
	if apiErr.Code != "404" { // numeric code, decoded through flexString
		t.Errorf("Code = %q", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "not found") {
		t.Errorf("Message = %q", apiErr.Message)
	}
	if apiErr.Retryable() {
		t.Error("400 should not be retryable")
	}
	if !strings.Contains(apiErr.Error(), "HTTP 400") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestStreamChat_HTTPErrorNonJSONBody(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>\n<head><title>502 Bad Gateway</title></head>\n" + strings.Repeat("x", 4096)))
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	_, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T: %v", err, err)
	}
	if !apiErr.Retryable() {
		t.Error("502 should be retryable")
	}
	if len(apiErr.Message) > 600 {
		t.Errorf("error excerpt not bounded: %d bytes", len(apiErr.Message))
	}
	if strings.Contains(apiErr.Message, "\n") {
		t.Errorf("error excerpt should be single-line: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "502 Bad Gateway") {
		t.Errorf("excerpt lost the useful part: %q", apiErr.Message)
	}
}

// Off-spec lines are survivable: heartbeat comments, unknown fields, bare
// junk, and one undecodable data payload. The good deltas still arrive and the
// count surfaces for an operator.
func TestStreamChat_MalformedSSELinesAreToleratedAndCounted(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.raw(": keep-alive comment")
		s.raw("event: chunk")
		s.delta("first ")
		s.raw("data: {this is not json}")
		s.raw("a line with no colon at all")
		s.raw("id: 42")
		s.delta("second")
		s.raw("data:")
		s.finish("stop", nil)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if err != nil {
		t.Fatalf("a malformed line should not fail the turn: %v", err)
	}
	if res.Text != "first second" {
		t.Errorf("Text = %q, want %q", res.Text, "first second")
	}
	if res.MalformedChunks != 2 {
		t.Errorf("MalformedChunks = %d, want 2 (bad json + bare line)", res.MalformedChunks)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
}

func TestStreamChat_TooMuchGarbageFails(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		for i := 0; i < maxMalformedChunks+5; i++ {
			s.raw("data: {nope")
		}
		s.finish("stop", nil)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	_, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if !errors.Is(err, errTooManyMalformed) {
		t.Fatalf("err = %v, want errTooManyMalformed", err)
	}
}

// An endpoint that streams forever (or a model that will not shut up) is
// stopped by the byte cap, and what arrived is kept.
func TestStreamChat_OversizedResponseHitsCap(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		chunk := strings.Repeat("a", 512)
		for i := 0; i < 1000; i++ {
			s.delta(chunk)
		}
		s.finish("stop", nil)
	})
	cfg := testConfig(srv.baseURL())
	cfg.MaxResponseBytes = 8 << 10 // 8 KiB
	c := newTestClient(t, cfg)

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("err = %v, want errResponseTooLarge", err)
	}
	if !res.Truncated {
		t.Error("Truncated should be set when the cap stopped the stream")
	}
	if res.Text == "" {
		t.Error("partial text before the cap should be preserved")
	}
	if int64(len(res.Text)) > cfg.MaxResponseBytes {
		t.Errorf("kept %d bytes of text, over the %d byte cap", len(res.Text), cfg.MaxResponseBytes)
	}
}

func TestStreamChat_LineTooLongFails(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta(strings.Repeat("b", 8192)) // one line, well over the 4 KiB cap
		s.finish("stop", nil)
	})
	cfg := testConfig(srv.baseURL())
	cfg.MaxLineBytes = 1024
	c := newTestClient(t, cfg)

	_, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if !errors.Is(err, errLineTooLong) {
		t.Fatalf("err = %v, want errLineTooLong", err)
	}
}

// Mid-stream cancellation: the client stops reading, the request is torn down,
// and the server observes its own request context cancel. This is what makes
// InterruptTurn real rather than cosmetic.
func TestStreamChat_ContextCancelMidStream(t *testing.T) {
	serverSawCancel := make(chan struct{})
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("thinking")
		select {
		case <-r.Context().Done():
			close(serverSawCancel)
		case <-time.After(5 * time.Second):
			t.Error("server never saw the request cancelled")
		}
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel from inside the delta callback: that is the real interrupt shape
	// (a human hits stop after seeing output) and it removes the race between
	// "server flushed" and "client read".
	res, err := c.streamChat(ctx, chatRequest{Model: "m", Stream: true}, func(string) { cancel() }, nil)
	if err == nil {
		t.Fatal("expected an error from the cancelled request")
	}
	if !errors.Is(err, context.Canceled) && !isConnectionAborted(err) {
		t.Errorf("err = %v, want context.Canceled or a torn-down connection", err)
	}
	if res.Text != "thinking" {
		t.Errorf("partial text = %q, want the delta received before cancelling", res.Text)
	}
	select {
	case <-serverSawCancel:
	case <-time.After(5 * time.Second):
		t.Error("server did not observe the cancellation")
	}
}

// Some servers ignore stream:true and answer with a single JSON object. That
// must still produce a turn rather than an error.
func TestStreamChat_NonStreamingJSONFallback(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"served-model","choices":[{"index":0,"message":{"role":"assistant","content":"one shot"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	var deltas []string
	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, func(s string) { deltas = append(deltas, s) }, nil)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if res.Text != "one shot" || res.FinishReason != "stop" || res.Model != "served-model" {
		t.Errorf("res = %+v", res)
	}
	if len(deltas) != 1 || deltas[0] != "one shot" {
		t.Errorf("deltas = %q", deltas)
	}
	if res.Usage.TotalTokens != 5 {
		t.Errorf("Usage = %+v", res.Usage)
	}
}

// The content shapes real servers emit: string, null, typed-part array, and an
// object with a text field.
func TestStreamChat_ContentShapes(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.raw(`data: {"choices":[{"delta":{"role":"assistant","content":null}}]}`)
		s.raw(`data: {"choices":[{"delta":{"content":"plain "}}]}`)
		s.raw(`data: {"choices":[{"delta":{"content":[{"type":"text","text":"parts "},{"type":"image_url","image_url":{"url":"x"}}]}}]}`)
		s.raw(`data: {"choices":[{"delta":{"content":{"text":"object"}}}]}`)
		s.finish("stop", nil)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if res.Text != "plain parts object" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.MalformedChunks != 0 {
		t.Errorf("MalformedChunks = %d; these are all shapes we support", res.MalformedChunks)
	}
}

func TestStreamChat_ReasoningDeltasAreSeparate(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.raw(`data: {"choices":[{"delta":{"reasoning_content":"weighing options"}}]}`)
		s.raw(`data: {"choices":[{"delta":{"reasoning":"still weighing"}}]}`)
		s.delta("the answer")
		s.finish("stop", nil)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	var text, reasoning []string
	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true},
		func(s string) { text = append(text, s) },
		func(s string) { reasoning = append(reasoning, s) })
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if res.Text != "the answer" {
		t.Errorf("reasoning leaked into Text: %q", res.Text)
	}
	if len(reasoning) != 2 {
		t.Errorf("reasoning deltas = %q", reasoning)
	}
	if strings.Join(text, "") != "the answer" {
		t.Errorf("text deltas = %q", text)
	}
}

func TestStreamChat_MidStreamErrorObject(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("starting")
		s.raw(`data: {"error":{"message":"GPU fell over","type":"server_error"}}`)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if !strings.Contains(apiErr.Message, "GPU fell over") || apiErr.Type != "server_error" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if res.Text != "starting" {
		t.Errorf("partial text lost: %q", res.Text)
	}
}

// A stream that just stops, without [DONE], yields what arrived. Proxies and
// abruptly-restarted servers do this.
func TestStreamChat_TruncatedStreamWithoutDone(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("half an ans")
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if res.Text != "half an ans" {
		t.Errorf("Text = %q", res.Text)
	}
}

func TestStreamChat_FinishReasonLengthMarksTruncated(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("as far as I got")
		s.finish("length", nil)
	})
	c := newTestClient(t, testConfig(srv.baseURL()))

	res, err := c.streamChat(context.Background(), chatRequest{Model: "m", Stream: true}, nil, nil)
	if err != nil {
		t.Fatalf("streamChat: %v", err)
	}
	if !res.Truncated {
		t.Error("finish_reason length should set Truncated")
	}
}

func TestClient_ListModels(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {})
	c := newTestClient(t, testConfig(srv.baseURL()))
	ids, err := c.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(ids) != 1 || ids[0] != "test-model" {
		t.Errorf("ids = %q", ids)
	}
}

// scrub is the last line of defence for a server that reflects our credential.
func TestScrub(t *testing.T) {
	const secret = "sk-abc-123"
	got := scrub("Bearer "+secret+" rejected; token "+secret, secret)
	if strings.Contains(got, secret) {
		t.Fatalf("secret survived scrubbing: %q", got)
	}
	if strings.Count(got, redactedMarker) != 2 {
		t.Errorf("scrubbed = %q", got)
	}
	if scrub("nothing to do", secret) != "nothing to do" {
		t.Error("scrub altered a clean string")
	}
	if scrub("keep "+secret, "") != "keep "+secret {
		t.Error("scrub with no secret should be identity")
	}
	// Wrapping preserves errors.Is while cleaning the message.
	wrapped := scrubErr(fmt.Errorf("post failed: %s: %w", secret, context.Canceled), secret)
	if strings.Contains(wrapped.Error(), secret) {
		t.Errorf("scrubErr left the secret in: %v", wrapped)
	}
	if !errors.Is(wrapped, context.Canceled) {
		t.Error("scrubErr broke the error chain")
	}
}
