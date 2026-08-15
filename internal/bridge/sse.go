package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SSE bounds. The stream itself is unbounded in time — that is the point — so
// the caps are per frame, which is where a hostile or buggy producer could
// otherwise force an unbounded allocation.
const (
	// maxSSELine caps one physical line. The largest legitimate line is the
	// snapshot event: 64 KiB of ring plus a mode prefix, base64-encoded, so
	// roughly 90 KiB. 1 MiB leaves generous headroom and still bounds memory.
	maxSSELine = 1 << 20

	// maxSSEFrame caps the accumulated data of one event across continuation
	// lines.
	maxSSEFrame = 2 << 20

	// sseReadBuf is the transport read buffer. Small enough that partial output
	// reaches the screen at keystroke latency.
	sseReadBuf = 8192
)

// ErrFrameTooLarge means a producer sent a line or frame past the caps above.
// The stream is abandoned rather than truncated, because a truncated base64
// payload would corrupt the terminal grid.
var ErrFrameTooLarge = errors.New("sse frame exceeds size cap")

// StreamEvent is one decoded server-sent event from /pty/{persona}/stream.
//
//	snapshot — the ring buffer plus the latched DEC private mode prefix, sent
//	           once at subscribe time. The bridge RESETS the persona's screen
//	           before applying it, so a re-prime cannot stack on stale content.
//	data     — live screen bytes.
//	exit     — the mate's process ended; no more data will follow.
type StreamEvent struct {
	Kind string
	Data []byte
}

// parseSSE reads an SSE body and calls emit once per complete event. emit
// returning false stops the parse (used to abandon a stream on shutdown).
//
// It is deliberately a plain function over an io.Reader so the wire format can
// be tested without a server or a goroutine.
func parseSSE(r io.Reader, emit func(StreamEvent) bool) error {
	br := bufio.NewReaderSize(r, sseReadBuf)
	var (
		event string
		data  []byte
	)
	flush := func() bool {
		if event == "" && len(data) == 0 {
			return true
		}
		kind := event
		if kind == "" {
			kind = "message"
		}
		decoded, err := decodeSSEData(data)
		event, data = "", nil
		if err != nil {
			// A malformed payload is dropped rather than fed to the emulator;
			// dropping one frame beats corrupting the grid.
			return true
		}
		return emit(StreamEvent{Kind: kind, Data: decoded})
	}

	for {
		line, err := readLine(br)
		if len(line) > 0 || err == nil {
			switch {
			case len(line) == 0: // blank line: dispatch
				if !flush() {
					return nil
				}
			case line[0] == ':': // comment / heartbeat
			default:
				field, value := splitSSEField(line)
				switch field {
				case "event":
					event = value
				case "data":
					if len(data)+len(value)+1 > maxSSEFrame {
						return ErrFrameTooLarge
					}
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, value...)
				}
				// id/retry and unknown fields are ignored.
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				flush() // a final event with no trailing blank line
				return nil
			}
			return err
		}
	}
}

// readLine reads one \n-terminated line with a hard length cap, stripping a
// trailing \r. Unlike bufio.Scanner it distinguishes "line too long" from EOF.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(out)+len(chunk) > maxSSELine {
			return nil, ErrFrameTooLarge
		}
		out = append(out, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(out, "\r\n"), err
	}
	return bytes.TrimRight(out, "\r\n"), nil
}

func splitSSEField(line []byte) (field, value string) {
	f, v, ok := strings.Cut(string(line), ":")
	if !ok {
		return f, ""
	}
	// Per the spec a single leading space after the colon is stripped.
	return f, strings.TrimPrefix(v, " ")
}

// decodeSSEData base64-decodes an event payload. The server encodes screen
// bytes with base64.StdEncoding; the exit event carries an empty payload.
func decodeSSEData(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	out := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(out, data)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

// streamPTY opens the persona's SSE stream and forwards every frame to msgs as
// a ptyFrameMsg, until the context is cancelled or the stream ends.
//
// Sends BLOCK when msgs is full, on purpose. Backpressure propagates to the
// HTTP read, and from there to the server's per-subscriber channel — which
// already drops the oldest frame rather than stalling the agent. So a slow
// bridge degrades to dropped screen updates (the mate keeps working) instead of
// growing memory here.
func (a *API) streamPTY(ctx context.Context, persona string, msgs chan<- any) {
	send := func(m any) bool {
		select {
		case msgs <- m:
			return true
		case <-ctx.Done():
			return false
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.StreamURL(persona), nil)
	if err != nil {
		send(ptyStreamEndMsg{Persona: persona, Err: err})
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	a.authorize(req)

	resp, err := a.stream.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			send(ptyStreamEndMsg{Persona: persona, Err: err})
		}
		return
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		send(ptyStreamEndMsg{Persona: persona, Err: statusError(resp)})
		return
	}

	perr := parseSSE(resp.Body, func(ev StreamEvent) bool {
		switch ev.Kind {
		case "snapshot", "data":
			return send(ptyFrameMsg{Persona: persona, Kind: ev.Kind, Data: ev.Data})
		case "exit":
			send(ptyStreamEndMsg{Persona: persona, Exited: true})
			return false
		}
		return true
	})
	if ctx.Err() != nil {
		return
	}
	if perr != nil {
		send(ptyStreamEndMsg{Persona: persona, Err: fmt.Errorf("stream: %w", perr)})
		return
	}
	// Clean EOF without an exit event: the server or a proxy closed the stream.
	send(ptyStreamEndMsg{Persona: persona})
}
