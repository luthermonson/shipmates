package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// TestMain doubles as a fake `claude` binary: when SHIPMATES_CLAUDE_FAKE=1
// the test executable pretends to be Claude Code's persistent stream-json
// transport — it reads user-message and control_request JSONL lines from
// stdin, emits a stream-json transcript per turn, folds mid-turn user
// messages in as steers, answers interrupts with a control_response + an
// error result, and exits 0 when stdin closes.
func TestMain(m *testing.M) {
	if os.Getenv("SHIPMATES_CLAUDE_FAKE") == "1" {
		runFakeClaude()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeClaude mimics `claude -p --input-format stream-json
// --output-format stream-json`. Behavior knobs (all env vars):
//
//	SHIPMATES_CLAUDE_FAKE_TURN_MS          delay before a turn's result frame
//	SHIPMATES_CLAUDE_FAKE_ECHO_ENV         name of an env var echoed as a text frame
//	SHIPMATES_CLAUDE_FAKE_ECHO_ARGS        emit one "argv:…" text frame per process
//	SHIPMATES_CLAUDE_FAKE_IGNORE_INTERRUPT ack control_requests but never end the turn
//	SHIPMATES_CLAUDE_FAKE_STDERR           write this to stderr and exit 1 on the first turn
//	SHIPMATES_CLAUDE_FAKE_HUGE_FRAME       emit a text frame of this many bytes per turn,
//	                                       before the turn's other output
//
// A turn whose text starts with "approve:" additionally exercises the
// permission control protocol: the fake emits a can_use_tool
// control_request for `Bash` with the rest of the text as the command and
// blocks the turn until a control_response arrives, exactly as claude
// 2.1.153 does. The decision is echoed back as an "approval:<behavior>…"
// text frame so tests can assert on it.
func runFakeClaude() {
	var outMu sync.Mutex
	emit := func(v any) {
		buf, err := json.Marshal(v)
		if err != nil {
			return
		}
		outMu.Lock()
		fmt.Println(string(buf))
		outMu.Unlock()
	}
	textFrame := func(text string) map[string]any {
		return map[string]any{"type": "text", "text": text}
	}

	var turnMS int
	_, _ = fmt.Sscanf(os.Getenv("SHIPMATES_CLAUDE_FAKE_TURN_MS"), "%d", &turnMS)
	ignoreInterrupt := os.Getenv("SHIPMATES_CLAUDE_FAKE_IGNORE_INTERRUPT") == "1"

	if os.Getenv("SHIPMATES_CLAUDE_FAKE_ECHO_ARGS") == "1" {
		emit(textFrame("argv:" + strings.Join(os.Args[1:], " ")))
	}

	// stateMu guards active/interrupt: whether a turn is currently in
	// flight and how to cancel it.
	var stateMu sync.Mutex
	var active bool
	var interrupt chan struct{}
	var wg sync.WaitGroup

	// approvals correlates an emitted can_use_tool request_id to the
	// goroutine blocked waiting for its control_response.
	var approvalMu sync.Mutex
	approvals := map[string]chan map[string]any{}
	approvalSeq := 0

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message struct {
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "user":
			// Auth/config failures: claude prints to stderr and exits without
			// ever emitting a stdout frame.
			if boom := os.Getenv("SHIPMATES_CLAUDE_FAKE_STDERR"); boom != "" {
				fmt.Fprintln(os.Stderr, boom)
				os.Exit(1)
			}
			var text string
			for _, c := range msg.Message.Content {
				if c.Type == "text" {
					text = c.Text
				}
			}
			// Echo the message's content blocks verbatim so a test can prove an
			// attachment actually crossed the wire rather than being dropped.
			if os.Getenv("SHIPMATES_CLAUDE_FAKE_ECHO_CONTENT") == "1" {
				parts := make([]string, 0, len(msg.Message.Content))
				for _, c := range msg.Message.Content {
					if c.Type == "image" {
						parts = append(parts, strings.Join([]string{c.Type, c.Source.Type, c.Source.MediaType, c.Source.Data}, "/"))
						continue
					}
					parts = append(parts, c.Type)
				}
				emit(textFrame("content:" + strings.Join(parts, ",")))
			}
			// Per-turn duration override: a "sleepms:<n>" prefix beats the
			// process-wide TURN_MS knob.
			ms := turnMS
			if strings.HasPrefix(text, "sleepms:") {
				_, _ = fmt.Sscanf(text, "sleepms:%d", &ms)
			}
			stateMu.Lock()
			if active {
				// Mid-turn user message = steer: folded into the active
				// turn (no extra result frame), echoed so tests can prove
				// it reached the child.
				stateMu.Unlock()
				emit(textFrame("steer-received:" + text))
				continue
			}
			active = true
			intr := make(chan struct{}, 1)
			interrupt = intr
			approvalSeq++
			reqID := fmt.Sprintf("fake-approval-%d", approvalSeq)
			stateMu.Unlock()

			wg.Add(1)
			go func(intr chan struct{}, ms int, text, reqID string) {
				defer wg.Done()
				// One pathologically long frame, then business as usual: the
				// process stays alive and keeps emitting frames the consumer
				// can no longer parse.
				if size := os.Getenv("SHIPMATES_CLAUDE_FAKE_HUGE_FRAME"); size != "" {
					var n int
					_, _ = fmt.Sscanf(size, "%d", &n)
					if n > 0 {
						emit(textFrame(strings.Repeat("x", n)))
					}
				}
				emit(map[string]any{
					"type": "assistant",
					"message": map[string]any{
						"content": []map[string]any{{"type": "text", "text": "fake hello"}},
					},
				})
				if name := os.Getenv("SHIPMATES_CLAUDE_FAKE_ECHO_ENV"); name != "" {
					emit(textFrame(fmt.Sprintf("env %s=%s", name, os.Getenv(name))))
				}
				if command, ok := strings.CutPrefix(text, "approve:"); ok {
					answer := make(chan map[string]any, 1)
					approvalMu.Lock()
					approvals[reqID] = answer
					approvalMu.Unlock()
					emit(map[string]any{
						"type":       "control_request",
						"request_id": reqID,
						"request": map[string]any{
							"subtype":      "can_use_tool",
							"tool_name":    "Bash",
							"display_name": "Bash",
							"description":  "fake tool call",
							"tool_use_id":  "toolu_fake",
							"input":        map[string]any{"command": command, "description": "fake tool call"},
							"permission_suggestions": []map[string]any{
								{"type": "setMode", "mode": "acceptEdits", "destination": "session"},
							},
						},
					})
					select {
					case body := <-answer:
						behavior, _ := body["behavior"].(string)
						message, _ := body["message"].(string)
						emit(textFrame("approval:" + behavior + ":" + message))
					case <-intr:
						emit(textFrame("approval:interrupted:"))
					case <-time.After(20 * time.Second):
						emit(textFrame("approval:timeout:"))
					}
				}
				result := map[string]any{"type": "result", "subtype": "success", "is_error": false}
				select {
				case <-time.After(time.Duration(ms) * time.Millisecond):
				case <-intr:
					result = map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true}
				}
				stateMu.Lock()
				active = false
				stateMu.Unlock()
				emit(result)
			}(intr, ms, text, reqID)
		case "control_response":
			var resp struct {
				Response struct {
					RequestID string         `json:"request_id"`
					Response  map[string]any `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(sc.Bytes(), &resp) != nil {
				continue
			}
			approvalMu.Lock()
			ch := approvals[resp.Response.RequestID]
			delete(approvals, resp.Response.RequestID)
			approvalMu.Unlock()
			if ch != nil {
				ch <- resp.Response.Response
			}
		case "control_request":
			if msg.Request.Subtype != "interrupt" {
				continue
			}
			emit(map[string]any{
				"type":     "control_response",
				"response": map[string]any{"subtype": "success", "request_id": msg.RequestID},
			})
			if ignoreInterrupt {
				continue
			}
			stateMu.Lock()
			if active && interrupt != nil {
				select {
				case interrupt <- struct{}{}:
				default:
				}
			}
			stateMu.Unlock()
		}
	}
	wg.Wait()
}

// fakeClaudeBinary returns the path of this test executable and arms the env
// that makes it behave as a fake `claude`. turnMS is how long each fake turn
// stays in flight before its result frame.
func fakeClaudeBinary(t *testing.T, turnMS int) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIPMATES_CLAUDE_FAKE", "1")
	t.Setenv("SHIPMATES_CLAUDE_FAKE_TURN_MS", fmt.Sprintf("%d", turnMS))
	return exe
}

// fakeClaudeRuntime returns a Runtime whose binary is this test executable
// in fake-claude mode, spawning through the default direct supervisor.
func fakeClaudeRuntime(t *testing.T, turnMS int) *Runtime {
	t.Helper()
	return New(Config{Binary: fakeClaudeBinary(t, turnMS)})
}

func startSession(t *testing.T, rt *Runtime) runtime.Session {
	t.Helper()
	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{
		Persona:    "tester",
		ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSendTurn_RefusesConcurrentTurn verifies a second SendTurn on a session
// with a live turn errors instead of silently folding into it.
func TestSendTurn_RefusesConcurrentTurn(t *testing.T) {
	rt := fakeClaudeRuntime(t, 3000)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("first SendTurn: %v", err)
	}
	_, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"})
	if err == nil {
		t.Fatal("second SendTurn succeeded; expected active-turn refusal")
	}
	if !strings.Contains(err.Error(), "already has an active turn") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSendTurn_AllowsNextTurnAfterCompletion verifies the turn slot is
// released once a turn's result frame arrives, and the next turn reuses the
// same persistent process.
func TestSendTurn_AllowsNextTurnAfterCompletion(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("first SendTurn: %v", err)
	}
	// Drain until the first turn's terminal event arrives.
	waitTurnDone(t, rt)

	deadline := time.After(10 * time.Second)
	for {
		_, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"})
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "already has an active turn") {
			t.Fatalf("second SendTurn: %v", err)
		}
		// The slot is released just before the terminal event is emitted;
		// retry briefly.
		select {
		case <-deadline:
			t.Fatal("turn slot never released after completion")
		case <-time.After(20 * time.Millisecond):
		}
	}
	waitTurnDone(t, rt)
}

func waitTurnDone(t *testing.T, rt *Runtime) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("event stream closed before turn completion")
			}
			if ev.Kind == runtime.KindTurnDone || ev.Kind == runtime.KindError {
				return
			}
		case <-timeout:
			t.Fatal("no terminal event within 15s")
		}
	}
}

// waitEventText drains events until a KindText payload containing want
// arrives; fails the test if a terminal event or timeout comes first.
func waitEventText(t *testing.T, rt *Runtime, want string) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatalf("stream closed before text %q", want)
			}
			if ev.Kind == runtime.KindText {
				if s, _ := ev.Payload.(string); strings.Contains(s, want) {
					return
				}
			}
		case <-timeout:
			t.Fatalf("no text event containing %q within 15s", want)
		}
	}
}

// TestSteerTurn_ReachesChildMidTurn verifies SteerTurn writes an
// additional user message to the live process while the turn is in flight
// and that the same turn still terminates with a single result frame.
func TestSteerTurn_ReachesChildMidTurn(t *testing.T) {
	rt := fakeClaudeRuntime(t, 3000)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	// Wait for turn output so we know the child has the turn in flight.
	waitEventText(t, rt, "fake hello")

	if err := rt.SteerTurn(context.Background(), s.ID(), turn.ID(), "port 10 degrees"); err != nil {
		t.Fatalf("SteerTurn: %v", err)
	}
	waitEventText(t, rt, "steer-received:port 10 degrees")
	waitTurnDone(t, rt)
}

// TestSteerTurn_ErrorsWithoutActiveTurn verifies steering outside a turn is
// refused — a user message written between turns would start a NEW turn.
func TestSteerTurn_ErrorsWithoutActiveTurn(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	s := startSession(t, rt)

	err := rt.SteerTurn(context.Background(), s.ID(), "t", "too late")
	if err == nil {
		t.Fatal("SteerTurn without active turn succeeded")
	}
	if !strings.Contains(err.Error(), "no active turn") {
		t.Errorf("unexpected error: %v", err)
	}
	if err := rt.SteerTurn(context.Background(), "nope", "t", "x"); err == nil {
		t.Fatal("SteerTurn on unknown session succeeded")
	}
}

// TestInterruptTurn_ProtocolEndsTurnWithoutKill verifies InterruptTurn uses
// the in-band control_request: the turn ends with an error result frame,
// the process survives, and the session takes another turn on the same
// process (no respawn).
func TestInterruptTurn_ProtocolEndsTurnWithoutKill(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ARGS", "1")
	s := startSession(t, rt)

	// The first turn would run for a minute unless interrupted.
	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "sleepms:60000"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitEventText(t, rt, "fake hello")

	if err := rt.InterruptTurn(context.Background(), s.ID(), turn.ID()); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	// The interrupted turn terminates via the error_during_execution
	// result frame, attributed to the interrupted turn.
	timeout := time.After(15 * time.Second)
	for {
		var ev runtime.Event
		var ok bool
		select {
		case ev, ok = <-rt.Events():
			if !ok {
				t.Fatal("stream closed before interrupt result")
			}
		case <-timeout:
			t.Fatal("no terminal event after interrupt")
		}
		if ev.Kind == runtime.KindError {
			if ev.TurnID != turn.ID() {
				t.Errorf("interrupt result turn id = %q want %q", ev.TurnID, turn.ID())
			}
			break
		}
		if ev.Kind == runtime.KindTurnDone {
			t.Fatal("interrupted turn ended with success result")
		}
	}

	// The process must still be alive: the next turn goes to the same
	// process, so no new argv frame is emitted.
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"}); err != nil {
		t.Fatalf("SendTurn after interrupt: %v", err)
	}
	timeout = time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before second turn completed")
			}
			if ev.Kind == runtime.KindText {
				if s, _ := ev.Payload.(string); strings.HasPrefix(s, "argv:") {
					t.Fatalf("unexpected respawn after protocol interrupt: %s", s)
				}
			}
			if ev.Kind == runtime.KindError {
				t.Fatalf("second turn failed: %v", ev.Payload)
			}
			if ev.Kind == runtime.KindTurnDone {
				return
			}
		case <-timeout:
			t.Fatal("second turn did not complete")
		}
	}
}

// TestInterruptTurn_FallsBackToKill verifies that when the child ignores
// the protocol interrupt, InterruptTurn escalates to the containment
// handle after interruptGrace, the dead process surfaces a KindError
// terminal, and the NEXT turn respawns with --resume (the session id
// already exists in Claude Code's store).
func TestInterruptTurn_FallsBackToKill(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	rt.interruptGrace = 200 * time.Millisecond
	t.Setenv("SHIPMATES_CLAUDE_FAKE_IGNORE_INTERRUPT", "1")
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ARGS", "1")
	s := startSession(t, rt)

	turn, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "sleepms:60000"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitEventText(t, rt, "fake hello")

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rt.InterruptTurn(ctx, s.ID(), turn.ID()); err != nil {
		t.Fatalf("InterruptTurn: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("InterruptTurn returned in %v; expected to wait ~interruptGrace before the kill", elapsed)
	}
	// Process death mid-turn surfaces as a KindError terminal.
	waitTurnDone(t, rt)

	// Next turn must respawn — this time with --resume, not --session-id.
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "two"}); err != nil {
		t.Fatalf("SendTurn after kill: %v", err)
	}
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before respawn argv frame")
			}
			if ev.Kind == runtime.KindText {
				if txt, _ := ev.Payload.(string); strings.HasPrefix(txt, "argv:") {
					if !strings.Contains(txt, "--resume "+s.ID()) {
						t.Fatalf("respawn args = %q; want --resume %s", txt, s.ID())
					}
					if strings.Contains(txt, "--session-id") {
						t.Fatalf("respawn args = %q; must not reuse --session-id", txt)
					}
					return
				}
			}
		case <-timeout:
			t.Fatal("no respawn argv frame after fallback kill")
		}
	}
}

// TestSpawnArgs_SessionIDVsResume verifies the first spawn of a started
// session uses --session-id while a resumed session spawns with --resume.
func TestSpawnArgs_SessionIDVsResume(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ARGS", "1")

	started := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), started.ID(), runtime.TurnInput{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitEventText(t, rt, "--session-id "+started.ID())
	waitTurnDone(t, rt)

	resumed, err := rt.ResumeSession(context.Background(), "11111111-2222-4333-8444-555555555555", runtime.SessionSpec{
		Persona:    "tester",
		ProjectDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), resumed.ID(), runtime.TurnInput{Text: "hi again"}); err != nil {
		t.Fatal(err)
	}
	waitEventText(t, rt, "--resume "+resumed.ID())
	waitTurnDone(t, rt)
}

// TestConcurrentSendTurnAndInterrupt exercises the locking under the race
// detector: many goroutines hammer SendTurn and InterruptTurn on the same
// session while a consumer drains events.
func TestConcurrentSendTurnAndInterrupt(t *testing.T) {
	rt := fakeClaudeRuntime(t, 500)
	s := startSession(t, rt)

	done := make(chan struct{})
	go func() { // drain events so the read loop never blocks
		for range rt.Events() {
		}
		close(done)
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "spin"})
				time.Sleep(10 * time.Millisecond)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = rt.InterruptTurn(context.Background(), s.ID(), "")
				time.Sleep(10 * time.Millisecond)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_ = rt.SteerTurn(context.Background(), s.ID(), "", "adjust")
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream not closed after Close — consumers would hang")
	}
}

// TestClose_ClosesEventStream verifies Close ends a `for range Events()`
// loop even when a turn is mid-flight.
func TestClose_ClosesEventStream(t *testing.T) {
	rt := fakeClaudeRuntime(t, 30000)
	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	terminated := make(chan struct{})
	go func() {
		for range rt.Events() {
		}
		close(terminated)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-terminated:
	case <-time.After(10 * time.Second):
		t.Fatal("Events() still open after Close")
	}
	// Idempotent.
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestCloseSession_TearsDownProcess verifies CloseSession ends the session
// process even with a turn in flight, surfacing a terminal event.
func TestCloseSession_TearsDownProcess(t *testing.T) {
	rt := fakeClaudeRuntime(t, 30000)
	defer rt.Close(context.Background())
	s := startSession(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "one"}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	waitEventText(t, rt, "fake hello")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.CloseSession(ctx, s.ID()); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// Process death mid-turn → error terminal for the active turn.
	waitTurnDone(t, rt)
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "late"}); err == nil {
		t.Fatal("SendTurn on closed session succeeded")
	}
}

// TestSendTurn_AppliesSessionEnvironment verifies SessionSpec.Environment
// (and the SHIPMATES_PERSONA export) reach the spawned process.
func TestSendTurn_AppliesSessionEnvironment(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	defer rt.Close(context.Background())
	t.Setenv("SHIPMATES_CLAUDE_FAKE_ECHO_ENV", "SHIPMATES_TEST_MARKER")

	s, err := rt.StartSession(context.Background(), runtime.SessionSpec{
		Persona:     "tester",
		ProjectDir:  t.TempDir(),
		Environment: map[string]string{"SHIPMATES_TEST_MARKER": "spec-env-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "go"}); err != nil {
		t.Fatal(err)
	}

	timeout := time.After(15 * time.Second)
	var sawEnv bool
	for !sawEnv {
		select {
		case ev, ok := <-rt.Events():
			if !ok {
				t.Fatal("stream closed before env echo")
			}
			if ev.Kind == runtime.KindText {
				if s, _ := ev.Payload.(string); strings.Contains(s, "SHIPMATES_TEST_MARKER=spec-env-value") {
					sawEnv = true
				}
			}
			if ev.Kind == runtime.KindTurnDone || ev.Kind == runtime.KindError {
				if !sawEnv {
					t.Fatalf("turn ended without env echo (last event %v)", ev.Kind)
				}
			}
		case <-timeout:
			t.Fatal("no env echo within 15s")
		}
	}
}

// TestSendTurn_AfterCloseFails verifies a turn cannot slip past Close.
func TestSendTurn_AfterCloseFails(t *testing.T) {
	rt := fakeClaudeRuntime(t, 0)
	s := startSession(t, rt)
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.SendTurn(context.Background(), s.ID(), runtime.TurnInput{Text: "late"}); err == nil {
		t.Fatal("SendTurn after Close succeeded")
	}
}

// TestCapabilities_SteerAndInterrupt locks in the capability flip that came
// with the persistent stream-json transport.
func TestCapabilities_SteerAndInterrupt(t *testing.T) {
	caps := New(Config{}).Capabilities()
	if !caps.Steer {
		t.Error("Caps.Steer = false; stream-json transport supports mid-turn steering")
	}
	if !caps.Interrupt {
		t.Error("Caps.Interrupt = false; stream-json transport supports control_request interrupt")
	}
}
