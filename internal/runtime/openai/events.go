package openai

import "time"

// The event payloads this runtime puts in runtime.Event.Payload. The runtime
// interface deliberately types Payload as `any` and tells consumers to
// type-assert, so each runtime owns its payload vocabulary. These are ours;
// they are small, immutable value types, safe to hand to a renderer.

// TextDelta is the payload of runtime.KindText: one incremental piece of
// assistant output, exactly as the endpoint sent it. Nothing is sanitized or
// re-wrapped here — rendering is the renderer's job — but the total is bounded
// by Config.MaxResponseBytes.
type TextDelta struct {
	Text string
}

// ReasoningDelta is the payload of runtime.KindBackend for servers that split
// chain-of-thought into a separate `reasoning_content` / `reasoning` delta
// field (vLLM with DeepSeek-R1-style models, some gateways). It is surfaced
// under KindBackend rather than KindText so a renderer can choose to hide it,
// and because it is not part of the transcript we send back.
type ReasoningDelta struct {
	Text string
}

// ErrorEvent is the payload of runtime.KindError. Message is already scrubbed
// of the API key (see scrub) even though it may quote the endpoint's own error
// body. HTTPStatus is 0 for transport-level failures.
type ErrorEvent struct {
	Message    string
	HTTPStatus int
	// Code and Type are the endpoint's own error taxonomy when it sent a
	// JSON error body ({"error":{"code":…,"type":…}}). Empty otherwise.
	Code string
	Type string
	// Retryable is a conservative hint: 429 and 5xx only. Not a promise.
	Retryable bool
}

// TurnDone is the payload of runtime.KindTurnDone. Exactly one is emitted per
// turn — after a KindError if the turn failed — so a consumer can always
// close its spinner on the turn boundary.
type TurnDone struct {
	// Text is the complete assistant message as assembled from the deltas.
	// On an interrupted turn this is the partial text received so far, which
	// is also what gets appended to the transcript.
	Text string
	// FinishReason is the endpoint's finish_reason for the last choice:
	// "stop", "length", "content_filter", … Empty when the server never sent
	// one (common on abrupt disconnects and on interrupts).
	FinishReason string
	// Refusal is a model refusal string if the endpoint used the
	// OpenAI `delta.refusal` channel. Almost no compatible server does; see
	// the Caps.Refusal note in runtime.go for why we still report false.
	Refusal string
	// Interrupted is true when InterruptTurn cancelled this turn.
	Interrupted bool
	// Failed is true when the turn ended on an error (a KindError event was
	// emitted immediately before this one).
	Failed bool
	// Truncated is true when output stopped because of a bound rather than
	// the model finishing: our byte cap, or the server's own length limit.
	Truncated bool
	// TranscriptTrimmed is true when appending this turn pushed the session
	// transcript past its bound and older messages were dropped.
	TranscriptTrimmed bool
	// MalformedChunks counts SSE payloads in this response we could not
	// decode and skipped. Non-zero means the endpoint is emitting something
	// off-spec; the turn still succeeded.
	MalformedChunks int
	// Usage is the endpoint's token accounting if it volunteered one. Many
	// compatible servers do not, and we do not request it (stream_options is
	// omitted for maximum compatibility), so zeroes are normal.
	Usage Usage
	// Model is the model string the endpoint reported serving, which is not
	// always the one we asked for (aliases, routers).
	Model string
	// Duration is wall time from request start to turn end.
	Duration time.Duration
}

// SessionClosed is the payload of runtime.KindSessionClosed.
type SessionClosed struct {
	Reason string
}

// Usage mirrors the OpenAI usage block. Zero values mean "not reported".
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}
