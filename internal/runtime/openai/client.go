package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// client is the thin HTTP layer: one POST to /chat/completions, one SSE
// stream out. Standard library only — net/http plus encoding/json is the whole
// dependency footprint, which is the point. An SDK would be a large dependency
// for a two-shape API and would not tolerate the off-spec servers we have to
// talk to anyway.
type client struct {
	cfg Config
	hc  *http.Client
}

func newClient(cfg Config) (*client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	hc, err := cfg.httpClient()
	if err != nil {
		return nil, err
	}
	return &client{cfg: cfg, hc: hc}, nil
}

// message is one chat-completions message. Content is a plain string on the
// way out: no image parts, no tool results, because this runtime sends
// neither.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
)

// chatRequest is deliberately the minimum body every OpenAI-compatible server
// accepts. No stream_options (vLLM supports include_usage, llama.cpp and older
// gateways reject unknown fields), no tools, no response_format, no
// max_completion_tokens (that spelling is new-OpenAI-only; max_tokens is the
// one self-hosted servers implement).
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// streamResult is what one completed (or interrupted) response yielded.
type streamResult struct {
	Text            string
	Refusal         string
	FinishReason    string
	Model           string
	Usage           Usage
	MalformedChunks int
	Truncated       bool
}

// APIError is a non-2xx response from the endpoint, with the server's own
// error taxonomy pulled out of the JSON body when there is one. Message is
// scrubbed of the API key before it ever reaches this struct.
type APIError struct {
	Status  int
	Code    string
	Type    string
	Message string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "openai runtime: endpoint returned HTTP %d", e.Status)
	if e.Type != "" {
		fmt.Fprintf(&b, " (%s)", e.Type)
	}
	if e.Code != "" {
		fmt.Fprintf(&b, " [%s]", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// Retryable reports the conservative retry hint: rate limiting and server
// faults only.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

var (
	// errResponseTooLarge means Config.MaxResponseBytes was hit.
	errResponseTooLarge = errors.New("openai runtime: response exceeded the configured byte cap")
	// errLineTooLong means Config.MaxLineBytes was hit by a single SSE line.
	errLineTooLong = errors.New("openai runtime: a single response line exceeded the configured line cap")
	// errTooManyMalformed means the endpoint sent more undecodable SSE
	// payloads than we tolerate.
	errTooManyMalformed = errors.New("openai runtime: too many malformed server-sent-event payloads")
)

// streamChat performs one turn's request and drives the callbacks as deltas
// arrive. It returns whatever was received before an error, so an interrupted
// turn still yields its partial text.
//
// Cancellation: ctx is attached to the request, so cancelling it aborts the
// in-flight HTTP request at the transport — the connection is torn down, the
// server stops generating. That is what makes Caps.Interrupt honest.
func (c *client) streamChat(ctx context.Context, req chatRequest, onText, onReasoning func(string)) (streamResult, error) {
	var res streamResult

	secret, err := c.cfg.apiKey()
	if err != nil {
		return res, err
	}

	body, err := json.Marshal(req)
	if err != nil {
		return res, fmt.Errorf("openai runtime: encoding request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return res, fmt.Errorf("openai runtime: building request: %w", err)
	}
	// Operator headers first so they cannot clobber the ones we own below.
	for k, v := range c.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.cfg.Organization != "" {
		httpReq.Header.Set("OpenAI-Organization", c.cfg.Organization)
	}
	// The one and only place the credential is written.
	if secret != "" {
		httpReq.Header.Set("Authorization", "Bearer "+secret)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return res, fmt.Errorf("openai runtime: request to endpoint failed: %w", scrubErr(err, secret))
	}
	defer func() {
		// Drain a little so keep-alive can reuse the connection, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		return res, c.apiError(resp, secret)
	}

	// Some servers ignore stream:true and answer with a single JSON object.
	// Treat that as a one-delta stream rather than failing the turn.
	if mediaType, _, mErr := mime.ParseMediaType(resp.Header.Get("Content-Type")); mErr == nil && mediaType == "application/json" {
		return c.readWholeCompletion(resp.Body, secret, onText, onReasoning)
	}

	counted := &countingReader{r: resp.Body, limit: c.cfg.MaxResponseBytes}
	br := bufio.NewReaderSize(counted, c.cfg.MaxLineBytes)

	var text strings.Builder
	var refusal strings.Builder
	for {
		line, rerr := readLine(br)
		if len(line) > 0 {
			done, perr := c.consumeSSELine(line, secret, &res, &text, &refusal, onText, onReasoning)
			if perr != nil {
				res.Text = text.String()
				res.Refusal = refusal.String()
				return res, perr
			}
			if done {
				break
			}
		}
		if rerr != nil {
			res.Text = text.String()
			res.Refusal = refusal.String()
			if errors.Is(rerr, io.EOF) {
				// A stream that ends without [DONE] is common enough (proxies,
				// abrupt server shutdown). Whatever we got is the answer.
				break
			}
			if errors.Is(rerr, errResponseTooLarge) {
				res.Truncated = true
			}
			return res, scrubErr(rerr, secret)
		}
	}
	res.Text = text.String()
	res.Refusal = refusal.String()
	if res.FinishReason == "length" {
		res.Truncated = true
	}
	return res, nil
}

// consumeSSELine handles one raw line. Returns done=true on the [DONE]
// sentinel. Unparseable payloads are counted and skipped rather than failing
// the turn, up to maxMalformedChunks — real servers emit heartbeat comments,
// `event:` lines, and the occasional non-conforming chunk, and a demo should
// not die on any of that.
func (c *client) consumeSSELine(line []byte, secret string, res *streamResult, text, refusal *strings.Builder, onText, onReasoning func(string)) (bool, error) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if len(trimmed) == 0 {
		return false, nil // event separator
	}
	if trimmed[0] == ':' {
		return false, nil // comment / heartbeat
	}
	colon := bytes.IndexByte(trimmed, ':')
	if colon < 0 {
		// Not a field line at all. Off-spec; count it and move on.
		res.MalformedChunks++
		if res.MalformedChunks > maxMalformedChunks {
			return false, errTooManyMalformed
		}
		return false, nil
	}
	field := string(bytes.TrimSpace(trimmed[:colon]))
	payload := bytes.TrimSpace(trimmed[colon+1:])
	if field != "data" {
		return false, nil // id:, event:, retry: — nothing we need
	}
	if string(payload) == "[DONE]" {
		return true, nil
	}
	if len(payload) == 0 {
		return false, nil
	}

	var chunk streamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		res.MalformedChunks++
		if res.MalformedChunks > maxMalformedChunks {
			return false, errTooManyMalformed
		}
		return false, nil
	}
	// Some servers report a mid-stream error inside a 200 SSE body.
	if chunk.Error != nil && (chunk.Error.Message != "" || chunk.Error.Type != "") {
		return false, &APIError{
			Status:  http.StatusOK,
			Code:    chunk.Error.Code.String(),
			Type:    chunk.Error.Type,
			Message: scrub(chunk.Error.Message, secret),
		}
	}
	if chunk.Model != "" {
		res.Model = chunk.Model
	}
	if chunk.Usage != nil {
		res.Usage = Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}
	for _, ch := range chunk.Choices {
		if s := scrub(string(ch.Delta.Content), secret); s != "" {
			text.WriteString(s)
			if onText != nil {
				onText(s)
			}
		}
		if s := scrub(ch.Delta.reasoning(), secret); s != "" && onReasoning != nil {
			onReasoning(s)
		}
		if ch.Delta.Refusal != nil {
			if s := scrub(*ch.Delta.Refusal, secret); s != "" {
				refusal.WriteString(s)
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			res.FinishReason = *ch.FinishReason
		}
	}
	return false, nil
}

// readWholeCompletion handles a server that answered with one JSON object
// instead of a stream. The whole message is emitted as a single delta so
// consumers see the same event sequence either way.
func (c *client) readWholeCompletion(body io.Reader, secret string, onText, onReasoning func(string)) (streamResult, error) {
	var res streamResult
	raw, err := io.ReadAll(&countingReader{r: body, limit: c.cfg.MaxResponseBytes})
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			res.Truncated = true
		}
		return res, scrubErr(err, secret)
	}
	var whole wholeCompletion
	if err := json.Unmarshal(raw, &whole); err != nil {
		return res, fmt.Errorf("openai runtime: endpoint sent application/json that is not a chat completion: %w", scrubErr(err, secret))
	}
	if whole.Error != nil && (whole.Error.Message != "" || whole.Error.Type != "") {
		return res, &APIError{
			Status:  http.StatusOK,
			Code:    whole.Error.Code.String(),
			Type:    whole.Error.Type,
			Message: scrub(whole.Error.Message, secret),
		}
	}
	res.Model = whole.Model
	if whole.Usage != nil {
		res.Usage = Usage{
			PromptTokens:     whole.Usage.PromptTokens,
			CompletionTokens: whole.Usage.CompletionTokens,
			TotalTokens:      whole.Usage.TotalTokens,
		}
	}
	for _, ch := range whole.Choices {
		if s := scrub(string(ch.Message.Content), secret); s != "" {
			res.Text += s
			if onText != nil {
				onText(s)
			}
		}
		if s := scrub(ch.Message.reasoning(), secret); s != "" && onReasoning != nil {
			onReasoning(s)
		}
		if ch.Message.Refusal != nil {
			res.Refusal += scrub(*ch.Message.Refusal, secret)
		}
		if ch.FinishReason != nil {
			res.FinishReason = *ch.FinishReason
		}
	}
	if res.FinishReason == "length" {
		res.Truncated = true
	}
	return res, nil
}

// listModels GETs base_url/models. Bounded like everything else; used only by
// Runtime.Probe.
func (c *client) listModels(ctx context.Context) ([]string, error) {
	secret, err := c.cfg.apiKey()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.ModelsEndpoint(), nil)
	if err != nil {
		return nil, fmt.Errorf("openai runtime: building models request: %w", err)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	if c.cfg.Organization != "" {
		req.Header.Set("OpenAI-Organization", c.cfg.Organization)
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai runtime: models request failed: %w", scrubErr(err, secret))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, c.apiError(resp, secret)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return nil, scrubErr(err, secret)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("openai runtime: models response is not JSON: %w", scrubErr(err, secret))
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// apiError builds an *APIError from a non-2xx response, reading a bounded
// prefix of the body. The body is scrubbed: a misconfigured gateway echoing
// the Authorization header into its error text must not become a leak.
func (c *client) apiError(resp *http.Response, secret string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	apiErr := &APIError{Status: resp.StatusCode}
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil {
		apiErr.Message = scrub(strings.TrimSpace(env.Error.Message), secret)
		apiErr.Type = strings.TrimSpace(env.Error.Type)
		apiErr.Code = strings.TrimSpace(env.Error.Code.String())
	}
	if apiErr.Message == "" {
		// Not a JSON error envelope (nginx HTML, plain text). Keep a short,
		// scrubbed, single-line excerpt — enough to diagnose, not enough to
		// dump a page into the event stream.
		excerpt := strings.TrimSpace(scrub(string(raw), secret))
		excerpt = strings.Join(strings.Fields(excerpt), " ")
		if len(excerpt) > 512 {
			excerpt = excerpt[:512] + "…"
		}
		apiErr.Message = excerpt
	}
	return apiErr
}

// --- wire types -------------------------------------------------------------

type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta        deltaBody `json:"delta"`
		FinishReason *string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageBody   `json:"usage"`
	Error *errorDetail `json:"error"`
}

type wholeCompletion struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      deltaBody `json:"message"`
		FinishReason *string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageBody   `json:"usage"`
	Error *errorDetail `json:"error"`
}

// deltaBody covers both the streaming `delta` and the non-streaming `message`
// object; the fields we care about are spelled the same in each.
type deltaBody struct {
	Role    string      `json:"role"`
	Content contentText `json:"content"`
	Refusal *string     `json:"refusal"`
	// Two spellings of the same thing in the wild: vLLM/DeepSeek use
	// reasoning_content, some gateways use reasoning.
	ReasoningContent contentText `json:"reasoning_content"`
	Reasoning        contentText `json:"reasoning"`
}

func (d deltaBody) reasoning() string {
	if d.ReasoningContent != "" {
		return string(d.ReasoningContent)
	}
	return string(d.Reasoning)
}

type usageBody struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type errorEnvelope struct {
	Error *errorDetail `json:"error"`
}

type errorDetail struct {
	Message string     `json:"message"`
	Type    string     `json:"type"`
	Code    flexString `json:"code"`
}

// flexString decodes a field that servers spell as either a string or a
// number ("invalid_request_error" vs 400).
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = flexString(str)
		return nil
	}
	*s = flexString(strings.Trim(string(b), `"`))
	return nil
}

func (s flexString) String() string { return string(s) }

// contentText decodes the several shapes real servers use for message
// content: a string, null, an array of typed parts ([{"type":"text",
// "text":"…"}]), or an object with a text field. Anything else is an error, so
// it lands in TurnDone.MalformedChunks where an operator can see it instead of
// silently vanishing.
type contentText string

func (c *contentText) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*c = ""
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = contentText(s)
		return nil
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" || p.Type == "output_text" {
				sb.WriteString(p.Text)
			}
		}
		*c = contentText(sb.String())
		return nil
	case '{':
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		*c = contentText(obj.Text)
		return nil
	default:
		return fmt.Errorf("unsupported content shape %q", truncateForError(string(b)))
	}
}

// --- bounded reading --------------------------------------------------------

// countingReader fails the read once limit bytes have been delivered. Cheaper
// and more explicit than io.LimitReader, which reports EOF and would look like
// a clean end of stream.
type countingReader struct {
	r     io.Reader
	limit int64
	n     int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.limit > 0 && c.n >= c.limit {
		return 0, errResponseTooLarge
	}
	if c.limit > 0 && int64(len(p)) > c.limit-c.n {
		p = p[:c.limit-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readLine returns one line including its terminator. A line longer than the
// reader's buffer (Config.MaxLineBytes) is an error rather than a silent
// split, because a split line would decode as a malformed chunk and hide the
// real problem.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errLineTooLong
	}
	return line, err
}

// --- secret hygiene ---------------------------------------------------------

// scrub removes the API key from any string that came from, or was influenced
// by, the endpoint. Defence in depth: the key is only ever written to a
// request header, but a gateway that reflects headers into its error body
// would otherwise put it straight into an event stream a human is watching.
func scrub(s, secret string) string {
	if s == "" || secret == "" {
		return s
	}
	if !strings.Contains(s, secret) {
		return s
	}
	return strings.ReplaceAll(s, secret, redactedMarker)
}

// scrubErr rewrites an error's message with the secret removed, preserving
// errors.Is/As behaviour by wrapping the original.
//
// The wrapped original is kept so errors.Is(err, context.Canceled) still works
// on a cancelled request, which the interrupt path depends on. That means a
// caller who deliberately unwraps and prints the inner error could see the
// original text — so nothing in this package does, and nothing outside it
// should: the outer Error() is the one that is safe to surface. Reaching the
// inner error requires errors.Unwrap by hand.
func scrubErr(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	clean := scrub(msg, secret)
	if clean == msg {
		return err
	}
	return &scrubbedError{msg: clean, err: err}
}

const redactedMarker = "[redacted]"

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

func truncateForError(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}
