package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

func TestName(t *testing.T) {
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	if r.Name() != "openai" {
		t.Errorf("Name() = %q", r.Name())
	}
}

// Caps is a promise to the rest of shipmates. Pin every field, because an
// optimistic one would have callers presenting an unmediated model as a
// supervised agent.
func TestCapabilities_AreHonest(t *testing.T) {
	r := newTestRuntime(t, testConfig("http://127.0.0.1:1/v1"))
	got := r.Capabilities()
	want := runtime.Caps{
		Streaming:   true,  // SSE deltas become KindText events
		Interrupt:   true,  // InterruptTurn cancels the HTTP request for real
		Steer:       false, // no mid-completion injection exists in the API
		Attachments: false, // no arbitrary compatible endpoint can be assumed to take images
		Refusal:     false, // no dependable cross-server refusal channel
		Containment: false, // this runtime spawns no processes
		Environment: false, // ...so there is no process environment to set
		Approvals:   false, // no tool calls are executed, so nothing to approve
	}
	if got != want {
		t.Errorf("Caps = %+v, want %+v", got, want)
	}

	// The false ones must be backed by refusals, not silence — and the refusal
	// has to happen before any argument validation or side effect, so a caller
	// can probe cheaply.
	ctx := context.Background()
	if err := r.SteerTurn(ctx, "s", "t", "faster"); !isUnsupported(err, "SteerTurn") {
		t.Errorf("SteerTurn err = %v", err)
	}
	ok, err := r.ResolveApproval(ctx, runtime.ApprovalResponse{}, runtime.ApprovalDecision{Allow: true})
	if ok || !isUnsupported(err, "ResolveApproval") {
		t.Errorf("ResolveApproval = %v, %v", ok, err)
	}
	// Environment is refused even with an otherwise invalid spec, i.e. before
	// argument validation.
	if _, err := r.StartSession(ctx, runtime.SessionSpec{Environment: map[string]string{"A": "b"}}); !isUnsupported(err, "Environment") {
		t.Errorf("StartSession(Environment) err = %v", err)
	}
	if _, err := r.ResumeSession(ctx, "nope", runtime.SessionSpec{Environment: map[string]string{"A": "b"}}); !isUnsupported(err, "Environment") {
		t.Errorf("ResumeSession(Environment) err = %v", err)
	}
}

// isUnsupported checks both halves of the interface's error contract: the
// concrete type carries the runtime and feature, and the sentinel matches so a
// caller can degrade without knowing the type.
func isUnsupported(err error, feature string) bool {
	if !errors.Is(err, runtime.ErrFeatureUnsupported) {
		return false
	}
	var target *runtime.ErrUnsupported
	if !errors.As(err, &target) {
		return false
	}
	return target.Runtime == "openai" && strings.Contains(target.Feature, feature)
}

// The end-to-end path a room sees: prompt in, deltas out, one turn boundary.
func TestFullTurn_StreamsTextAndClosesWithTurnDone(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("Aye. ")
		s.delta("Two knots ")
		s.delta("to starboard.")
		s.finish("stop", &usageBody{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27})
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()

	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if !strings.HasPrefix(sess.ID(), "openai-sess-") {
		t.Errorf("session id = %q", sess.ID())
	}
	turn, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "which way?"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	got := collectTurn(t, r.Events(), 10*time.Second)
	if got.Text() != "Aye. Two knots to starboard." {
		t.Errorf("streamed text = %q", got.Text())
	}
	if len(got.Texts) != 3 {
		t.Errorf("expected 3 KindText events (streaming, not one blob), got %d", len(got.Texts))
	}
	if got.Done.Text != got.Text() {
		t.Errorf("TurnDone.Text = %q, deltas = %q", got.Done.Text, got.Text())
	}
	if got.Done.FinishReason != "stop" || got.Done.Interrupted || got.Done.Failed || got.Done.Truncated {
		t.Errorf("TurnDone = %+v", got.Done)
	}
	if got.Done.Usage.TotalTokens != 27 {
		t.Errorf("Usage = %+v", got.Done.Usage)
	}
	if got.Done.Duration <= 0 {
		t.Errorf("Duration = %v", got.Done.Duration)
	}
	if len(got.Errors) != 0 {
		t.Errorf("unexpected error events: %+v", got.Errors)
	}
	// Every event is attributed to the session and turn so a room can
	// demultiplex one shared channel.
	for _, ev := range got.All {
		if ev.SessionID != sess.ID() || ev.TurnID != turn.ID() {
			t.Errorf("event %v has session=%q turn=%q", ev.Kind, ev.SessionID, ev.TurnID)
		}
		if ev.Timestamp.IsZero() {
			t.Errorf("event %v has no timestamp", ev.Kind)
		}
	}
	if got.All[len(got.All)-1].Kind != runtime.KindTurnDone {
		t.Errorf("last event = %v, want turn_done", got.All[len(got.All)-1].Kind)
	}
}

// The API is stateless, so the runtime owns the transcript: turn two must carry
// turn one's exchange, behind the persona system prompt.
func TestSendTurn_TranscriptIsResentEachTurn(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("answer-" + fmt.Sprint(len(rec.Body.Messages)))
		s.finish("stop", nil)
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	for _, prompt := range []string{"first", "second"} {
		if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: prompt}); err != nil {
			t.Fatalf("SendTurn(%q): %v", prompt, err)
		}
		collectTurn(t, r.Events(), 10*time.Second)
	}

	reqs := srv.recorded()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	first, second := reqs[0].Body.Messages, reqs[1].Body.Messages
	if len(first) != 2 {
		t.Fatalf("first request messages = %+v, want system+user", first)
	}
	if first[0].Role != roleSystem || !strings.Contains(first[0].Content, "Runtime contract") {
		t.Errorf("first message should be the persona system prompt, got %+v", first[0])
	}
	if first[1].Role != roleUser || first[1].Content != "first" {
		t.Errorf("first user message = %+v", first[1])
	}
	if len(second) != 4 {
		t.Fatalf("second request messages = %d, want system+user+assistant+user", len(second))
	}
	roles := []string{second[0].Role, second[1].Role, second[2].Role, second[3].Role}
	if want := []string{roleSystem, roleUser, roleAssistant, roleUser}; fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	if second[2].Content != "answer-2" {
		t.Errorf("assistant turn not carried forward: %q", second[2].Content)
	}
	if second[3].Content != "second" {
		t.Errorf("second user message = %q", second[3].Content)
	}
}

// InterruptTurn must actually stop generation: the server observes the request
// context cancel, partial text is kept, and TurnDone says it was interrupted.
func TestInterruptTurn_StopsTheRequestForReal(t *testing.T) {
	serverSawCancel := make(chan struct{})
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("I am going to talk for a very long time")
		select {
		case <-r.Context().Done():
			close(serverSawCancel)
		case <-time.After(10 * time.Second):
			t.Error("server never saw the interrupt")
		}
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "monologue please"})
	if err != nil {
		t.Fatal(err)
	}

	// Interrupt only once the operator has actually seen output — that is the
	// real shape of "stop", and it means the delta is already downstream when
	// the connection is torn down.
	var firstDelta string
	select {
	case ev := <-r.Events():
		td, ok := ev.Payload.(TextDelta)
		if ev.Kind != runtime.KindText || !ok {
			t.Fatalf("first event = %v %T, want a text delta", ev.Kind, ev.Payload)
		}
		firstDelta = td.Text
	case <-time.After(10 * time.Second):
		t.Fatal("no delta arrived to interrupt after")
	}
	if err := r.InterruptTurn(ctx, sess.ID(), turn.ID()); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}

	got := collectTurn(t, r.Events(), 10*time.Second)
	got.Texts = append([]string{firstDelta}, got.Texts...)
	if !got.Done.Interrupted {
		t.Errorf("TurnDone.Interrupted = false: %+v", got.Done)
	}
	if got.Done.Failed {
		t.Error("an interrupt is not a failure")
	}
	if len(got.Errors) != 0 {
		t.Errorf("an interrupt should not emit an error event: %+v", got.Errors)
	}
	if got.Text() != "I am going to talk for a very long time" {
		t.Errorf("partial text = %q", got.Text())
	}
	select {
	case <-serverSawCancel:
	case <-time.After(10 * time.Second):
		t.Fatal("the HTTP request was not actually cancelled")
	}

	// The partial answer stays in the transcript: the operator saw it, so the
	// model should too.
	s := sess.(*session)
	if n := s.transcriptLen(); n != 2 {
		t.Errorf("transcript has %d messages, want user+partial assistant", n)
	}

	// A second interrupt for a turn that is no longer running is an error, not
	// a silent success.
	if err := r.InterruptTurn(ctx, sess.ID(), turn.ID()); err == nil {
		t.Error("interrupting a finished turn should report there is nothing in flight")
	}
	if err := r.InterruptTurn(ctx, "no-such-session", "t"); err == nil {
		t.Error("interrupting an unknown session should error")
	}
}

func TestSendTurn_RefusesWhatItCannotDo(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.finish("stop", nil)
	})
	cfg := testConfig(srv.baseURL())
	cfg.MaxPromptBytes = 64
	r := newTestRuntime(t, cfg)
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	// Attachments: refused, not silently dropped.
	_, err = r.SendTurn(ctx, sess.ID(), runtime.TurnInput{
		Text:        "look at this",
		Attachments: []runtime.Attachment{{Path: "/x.png", Kind: "image", MediaType: "image/png"}},
	})
	if !isUnsupported(err, "Attachments") {
		t.Errorf("attachment err = %v", err)
	}
	// Empty and oversized prompts.
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "   "}); err == nil {
		t.Error("empty prompt accepted")
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: strings.Repeat("x", 65)}); err == nil {
		t.Error("oversized prompt accepted")
	}
	// Unknown session.
	if _, err := r.SendTurn(ctx, "openai-sess-nope", runtime.TurnInput{Text: "hi"}); err == nil {
		t.Error("unknown session accepted")
	}
	// Per-session environment: refused, matching Caps.Environment == false.
	if _, err := r.StartSession(ctx, runtime.SessionSpec{
		Persona: "captain", ProjectDir: t.TempDir(),
		Environment: map[string]string{"FOO": "bar"},
	}); !isUnsupported(err, "Environment") {
		t.Errorf("Environment err = %v", err)
	}
	if _, err := r.StartSession(ctx, runtime.SessionSpec{Persona: ""}); err == nil {
		t.Error("empty persona accepted")
	}
}

// One turn at a time per session: two completions racing on one transcript
// cannot both be coherent, so the second is refused rather than queued.
func TestSendTurn_OneTurnAtATime(t *testing.T) {
	release := make(chan struct{})
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("working")
		<-release
		s.finish("stop", nil)
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatal(err)
	}
	_, err = r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "two"})
	if !errors.Is(err, ErrTurnInFlight) {
		t.Errorf("second SendTurn err = %v, want ErrTurnInFlight", err)
	}
	close(release)
	collectTurn(t, r.Events(), 10*time.Second)
	// Once the first turn is done the session is usable again.
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "three"}); err != nil {
		t.Errorf("SendTurn after completion: %v", err)
	}
	collectTurn(t, r.Events(), 10*time.Second)
}

// An HTTP error becomes one KindError followed by a KindTurnDone, so a caller
// waiting on a turn boundary is never left hanging.
func TestTurn_HTTPErrorEmitsErrorThenTurnDone(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"queue is full, 40 requests ahead","type":"rate_limit_error","code":"rate_limited"}}`))
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "hello"}); err != nil {
		t.Fatal(err)
	}

	got := collectTurn(t, r.Events(), 10*time.Second)
	if len(got.Errors) != 1 {
		t.Fatalf("expected 1 error event, got %+v", got.Errors)
	}
	e := got.Errors[0]
	if e.HTTPStatus != http.StatusTooManyRequests || e.Type != "rate_limit_error" || e.Code != "rate_limited" {
		t.Errorf("ErrorEvent = %+v", e)
	}
	if !e.Retryable {
		t.Error("429 should be flagged retryable")
	}
	if !strings.Contains(e.Message, "queue is full") {
		t.Errorf("message lost the server's explanation: %q", e.Message)
	}
	if !got.Done.Failed {
		t.Error("TurnDone.Failed should be set")
	}
	// Order matters: error before the boundary.
	kinds := []runtime.Kind{}
	for _, ev := range got.All {
		kinds = append(kinds, ev.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]runtime.Kind{runtime.KindError, runtime.KindTurnDone}) {
		t.Errorf("event kinds = %v", kinds)
	}
}

func TestTurn_TimeoutIsReported(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("starting slowly")
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	cfg := testConfig(srv.baseURL())
	cfg.Timeout = 250 * time.Millisecond
	r := newTestRuntime(t, cfg)
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "take your time"}); err != nil {
		t.Fatal(err)
	}
	got := collectTurn(t, r.Events(), 10*time.Second)
	if !got.Done.Failed {
		t.Errorf("TurnDone = %+v", got.Done)
	}
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Message, "timeout") {
		t.Errorf("errors = %+v", got.Errors)
	}
	if got.Text() != "starting slowly" {
		t.Errorf("partial text before the timeout should still have been streamed, got %q", got.Text())
	}
}

// The whole point of requirement 2, tested from the outside: with a key in the
// environment and a hostile endpoint that reflects the Authorization header
// into both an error body and a content delta, the key must not appear in any
// error, any event payload, or the transcript.
func TestAPIKeyNeverLeaks(t *testing.T) {
	const secret = "sk-live-DO-NOT-LOG-9f8e7d6c5b4a"
	t.Setenv("SHIPMATES_TEST_LEAK_KEY", secret)

	mode := make(chan string, 4)
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		auth := r.Header.Get("Authorization")
		switch <-mode {
		case "error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			// A real gateway has done exactly this.
			fmt.Fprintf(w, `{"error":{"message":"rejected credential %s","type":"auth_error"}}`, auth)
		default:
			s := newSSEWriter(t, w)
			s.delta("your header was " + auth + " ")
			s.delta("and your key was " + secret)
			s.finish("stop", nil)
		}
	})
	cfg := testConfig(srv.baseURL())
	cfg.APIKeyEnv = "SHIPMATES_TEST_LEAK_KEY"
	r := newTestRuntime(t, cfg)
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	assertClean := func(what string, strs []string) {
		for _, s := range strs {
			if strings.Contains(s, secret) {
				t.Fatalf("%s leaked the API key: %q", what, s)
			}
		}
	}

	// 1. Error path: the server quotes the credential back at us.
	mode <- "error"
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "auth please"}); err != nil {
		t.Fatal(err)
	}
	got := collectTurn(t, r.Events(), 10*time.Second)
	if len(got.Errors) != 1 {
		t.Fatalf("expected an error event, got %+v", got.All)
	}
	if !strings.Contains(got.Errors[0].Message, redactedMarker) {
		t.Errorf("the reflected credential should have been redacted, got %q", got.Errors[0].Message)
	}
	assertClean("error event", eventStrings(got.All))

	// 2. Success path: the server echoes the credential in content deltas.
	mode <- "stream"
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "echo it"}); err != nil {
		t.Fatal(err)
	}
	got = collectTurn(t, r.Events(), 10*time.Second)
	assertClean("stream events", eventStrings(got.All))
	if !strings.Contains(got.Done.Text, redactedMarker) {
		t.Errorf("echoed key should be redacted in the turn text, got %q", got.Done.Text)
	}

	// 3. The transcript we would resend must be clean too.
	s := sess.(*session)
	s.mu.Lock()
	var transcript []string
	for _, m := range s.messages {
		transcript = append(transcript, m.Content)
	}
	s.mu.Unlock()
	assertClean("transcript", transcript)

	// 4. A configured-but-empty key produces an actionable error naming only
	// the variable — and no request.
	t.Setenv("SHIPMATES_TEST_LEAK_KEY", "")
	mode <- "stream"
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "no key now"}); err != nil {
		t.Fatal(err)
	}
	got = collectTurn(t, r.Events(), 10*time.Second)
	if len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Message, "SHIPMATES_TEST_LEAK_KEY") {
		t.Fatalf("expected a named-env-var error, got %+v", got.Errors)
	}
	assertClean("missing key error", eventStrings(got.All))
	<-mode // drain the unused mode token
}

func TestResumeSession_HonestAboutInMemoryTranscripts(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("hi")
		s.finish("stop", nil)
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "remember this"}); err != nil {
		t.Fatal(err)
	}
	collectTurn(t, r.Events(), 10*time.Second)

	// Same process, still live: the transcript is intact.
	again, err := r.ResumeSession(ctx, sess.ID(), runtime.SessionSpec{Persona: "captain"})
	if err != nil {
		t.Fatalf("ResumeSession(live): %v", err)
	}
	if again.(*session).transcriptLen() != 2 {
		t.Errorf("resumed session lost its transcript")
	}
	// Wrong persona for that ID: refused rather than silently rebound.
	if _, err := r.ResumeSession(ctx, sess.ID(), runtime.SessionSpec{Persona: "cook"}); err == nil {
		t.Error("resuming under a different persona should be refused")
	}
	// Unknown ID: says the transcript is gone instead of starting fresh and
	// pretending to remember.
	_, err = r.ResumeSession(ctx, "openai-sess-from-a-previous-process", runtime.SessionSpec{Persona: "captain"})
	if !errors.Is(err, ErrTranscriptNotPersisted) {
		t.Errorf("err = %v, want ErrTranscriptNotPersisted", err)
	}
	if !strings.Contains(err.Error(), "in-memory") {
		t.Errorf("error should explain why: %v", err)
	}
}

func TestCloseSession_AndClose(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("mid-sentence")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "go"}); err != nil {
		t.Fatal(err)
	}
	// CloseSession cancels the in-flight turn and announces the close.
	if err := r.CloseSession(ctx, sess.ID()); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	sawClosed := false
	deadline := time.After(10 * time.Second)
	for !sawClosed {
		select {
		case ev := <-r.Events():
			if ev.Kind == runtime.KindSessionClosed {
				if ev.SessionID != sess.ID() {
					t.Errorf("session_closed for %q", ev.SessionID)
				}
				sawClosed = true
			}
		case <-deadline:
			t.Fatal("no session_closed event")
		}
	}
	// Idempotent for an unknown/second call.
	if err := r.CloseSession(ctx, sess.ID()); err != nil {
		t.Errorf("second CloseSession: %v", err)
	}
	if err := r.CloseSession(ctx, "nope"); err != nil {
		t.Errorf("CloseSession(unknown): %v", err)
	}

	// Close is idempotent, closes the event channel, and refuses further work.
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	for {
		_, ok := <-r.Events()
		if !ok {
			break
		}
	}
	if _, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain"}); !errors.Is(err, ErrRuntimeClosed) {
		t.Errorf("StartSession after Close = %v", err)
	}
	if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: "x"}); !errors.Is(err, ErrRuntimeClosed) {
		t.Errorf("SendTurn after Close = %v", err)
	}
}

// The transcript is bounded: a long-running room must not grow the process
// until it dies, and the loss is reported rather than hidden.
func TestTranscriptBounds(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("ok")
		s.finish("stop", nil)
	})
	cfg := testConfig(srv.baseURL())
	cfg.MaxTranscriptMessages = 3
	r := newTestRuntime(t, cfg)
	ctx := context.Background()
	sess, err := r.StartSession(ctx, runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var sawTrim bool
	for i := 0; i < 4; i++ {
		if _, err := r.SendTurn(ctx, sess.ID(), runtime.TurnInput{Text: fmt.Sprintf("turn %d", i)}); err != nil {
			t.Fatal(err)
		}
		got := collectTurn(t, r.Events(), 10*time.Second)
		sawTrim = sawTrim || got.Done.TranscriptTrimmed
	}
	s := sess.(*session)
	if n := s.transcriptLen(); n > 3 {
		t.Errorf("transcript has %d messages, over the bound of 3", n)
	}
	if !sawTrim {
		t.Error("trimming was never reported on TurnDone")
	}
	// A trimmed transcript still starts with a user message: no history that
	// reads as the model talking to itself.
	s.mu.Lock()
	first := s.messages[0].Role
	s.mu.Unlock()
	if first != roleUser {
		t.Errorf("trimmed transcript starts with %q", first)
	}
	// And the system prompt is never trimmed away.
	last := srv.recorded()[len(srv.recorded())-1]
	if last.Body.Messages[0].Role != roleSystem {
		t.Errorf("system prompt lost after trimming: %+v", last.Body.Messages)
	}
}

func TestNewFromSettings_WiresTheFactoryShape(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {
		s := newSSEWriter(t, w)
		s.delta("from settings")
		s.finish("stop", nil)
	})
	rt, err := NewFromSettings(context.Background(), map[string]any{
		"base_url": srv.baseURL(),
		"model":    "test-model",
		"timeout":  "30s",
	})
	if err != nil {
		t.Fatalf("NewFromSettings: %v", err)
	}
	defer func() {
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	}()
	if rt.Name() != "openai" {
		t.Errorf("Name() = %q", rt.Name())
	}
	sess, err := rt.StartSession(context.Background(), runtime.SessionSpec{Persona: "captain", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), sess.ID(), runtime.TurnInput{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	got := collectTurn(t, rt.Events(), 10*time.Second)
	if got.Text() != "from settings" {
		t.Errorf("text = %q", got.Text())
	}

	// Bad settings fail at construction, not at first turn.
	if _, err := NewFromSettings(context.Background(), map[string]any{"model": "m"}); err == nil {
		t.Error("missing base_url accepted")
	}
}

func TestProbe(t *testing.T) {
	srv := newFakeEndpoint(t, func(w http.ResponseWriter, r *http.Request, rec recordedRequest) {})
	r := newTestRuntime(t, testConfig(srv.baseURL()))
	ids, err := r.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(ids) != 1 || ids[0] != "test-model" {
		t.Errorf("ids = %q", ids)
	}
}
