package livesession

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/turninput"
)

func TestFakeLiveAppServer(t *testing.T) {
	if os.Getenv("SHIPMATES_FAKE_LIVE_SERVER") != "1" {
		return
	}
	in := bufio.NewScanner(os.Stdin)
	reply := func(id json.RawMessage, result any) {
		b, _ := json.Marshal(map[string]any{"id": json.RawMessage(id), "result": result})
		fmt.Println(string(b))
	}
	interrupts := 0
	turns := 0
	for in.Scan() {
		if record := os.Getenv("SHIPMATES_FAKE_APPROVAL_RECORD"); record != "" && strings.Contains(in.Text(), `"id":91`) && !strings.Contains(in.Text(), `"method"`) {
			_ = os.WriteFile(record, in.Bytes(), 0o600)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "" {
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, map[string]any{"userAgent": "codex-cli 0.144.1"})
		case "initialized":
		case "thread/start":
			if os.Getenv("SHIPMATES_FAKE_FAIL_THREAD") == "1" {
				os.Exit(7)
			}
			reply(req.ID, map[string]any{"thread": map[string]string{"id": os.Getenv("SHIPMATES_FAKE_THREAD")}})
		case "thread/resume":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(req.Params, &p)
			_ = os.WriteFile(os.Getenv("SHIPMATES_FAKE_RECORD"), []byte(p.ThreadID), 0o600)
			reply(req.ID, map[string]any{"thread": map[string]string{"id": p.ThreadID}})
		case "turn/start":
			if record := os.Getenv("SHIPMATES_FAKE_INPUT_RECORD"); record != "" {
				_ = os.WriteFile(record, req.Params, 0o600)
			}
			if os.Getenv("SHIPMATES_FAKE_REJECT_TURN") == "1" {
				fmt.Println(`{"id":` + string(req.ID) + `,"error":{"code":-32000,"message":"SECRET_TURN_ERROR"}}`)
				continue
			}
			turns++
			turnID := fmt.Sprintf("turn-%d", turns)
			if record := os.Getenv("SHIPMATES_FAKE_TURN_RECORD"); record != "" {
				f, err := os.OpenFile(record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					os.Exit(8)
				}
				_, _ = fmt.Fprintln(f, turnID)
				_ = f.Close()
			}
			reply(req.ID, map[string]any{"turn": map[string]string{"id": turnID}})
			if turns == 2 && os.Getenv("SHIPMATES_FAKE_DELAYED_REQUESTS") == "1" {
				time.Sleep(20 * time.Millisecond)
				fmt.Println(`{"id":92,"method":"item/commandExecution/requestApproval","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-1","command":"SECRET_DELAYED"}}`)
				fmt.Println(`{"id":93,"method":"item/commandExecution/requestApproval","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-2","command":"SECRET_CURRENT"}}`)
			}
			if os.Getenv("SHIPMATES_FAKE_LIVE_EVENTS") == "1" {
				fmt.Println(`{"method":"item/started","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-1","item":{"type":"commandExecution","command":"SECRET_COMMAND"}}}`)
				fmt.Println(`{"id":91,"method":"item/commandExecution/requestApproval","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-1","command":"SECRET_APPROVAL"}}`)
			}
		case "turn/steer":
			reply(req.ID, map[string]any{"accepted": true})
			fmt.Println(`{"method":"item/started","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-1","item":{"type":"webSearch","query":"SECRET_QUERY"}}}`)
			fmt.Println(`{"method":"item/completed","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turnId":"turn-1","item":{"type":"agentMessage","text":"safe answer"}}}`)
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turn":{"id":"turn-1"}}}`)
		case "turn/interrupt":
			interrupts++
			switch os.Getenv("SHIPMATES_FAKE_INTERRUPT") {
			case "error-once":
				if interrupts == 1 {
					fmt.Println(`{"id":` + string(req.ID) + `,"error":{"code":-32000,"message":"SECRET_ERROR"}}`)
					continue
				}
			case "cancel-once":
				if interrupts == 1 {
					continue
				}
			case "complete-then-error":
				fmt.Println(`{"method":"turn/completed","params":{"threadId":"` + os.Getenv("SHIPMATES_FAKE_THREAD") + `","turn":{"id":"turn-1"}}}`)
				time.Sleep(20 * time.Millisecond)
				fmt.Println(`{"id":` + string(req.ID) + `,"error":{"code":-32000,"message":"SECRET_ERROR"}}`)
				continue
			}
			reply(req.ID, map[string]any{"accepted": true})
		default:
			os.Exit(3)
		}
	}
}

func TestLiveTurnLocalImagesAreTextFirstCountOnlyAndRevalidated(t *testing.T) {
	fixture(t, "backend")
	imageRoot := t.TempDir()
	imagePath := filepath.Join(imageRoot, "input.png")
	record := filepath.Join(t.TempDir(), "turn.json")
	env := map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-image",
		"SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "resume"), "SHIPMATES_FAKE_INPUT_RECORD": record,
	}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if err := os.WriteFile(imagePath, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := turninput.ValidateImages(imageRoot, []string{"input.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	snap := s.Snapshot()
	if _, err := m.StartNextTurnInput(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "inspect", batch.Images()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Input []map[string]string `json:"input"`
	}
	if json.Unmarshal(raw, &params) != nil || len(params.Input) != 2 || params.Input[0]["type"] != "text" || params.Input[0]["text"] != "inspect" || params.Input[1]["type"] != "localImage" || params.Input[1]["path"] != imagePath {
		t.Fatalf("turn input=%s", raw)
	}
	feed, _ := json.Marshal(s.Feed(0))
	if !strings.Contains(string(feed), `"image_count":1`) || strings.Contains(string(feed), imagePath) || strings.Contains(string(feed), "input.png") {
		t.Fatalf("unsafe feed=%s", feed)
	}

	// A changed descriptor is refused at the adapter edge before a second frame
	// can overwrite the record of the accepted request.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("GIF89afixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	m2 := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s2, err := m2.StartIdle(context.Background(), StartIdleOptions{Persona: "backend", Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	snap2 := s2.Snapshot()
	_, err = m2.StartNextTurnInput(context.Background(), snap2.Persona, snap2.SessionID, snap2.ThreadID, "again", batch.Images())
	if codexapp.ErrorCode(err) != codexapp.BackendRejected {
		t.Fatalf("changed image error=%v", err)
	}
	after, _ := os.ReadFile(record)
	if string(after) != string(raw) {
		t.Fatalf("changed image reached app-server: %s", after)
	}
}

func TestInitialLiveImagesAllFormatsOrderedAndNonPersistent(t *testing.T) {
	fixture(t, "backend")
	root := t.TempDir()
	paths := []string{"space.png", "-dash.jpg", "雪.gif", "last.webp"}
	headers := [][]byte{
		{0x89, 'P', 'N', 'G', 13, 10, 26, 10},
		{0xff, 0xd8, 0xff, 0xe0},
		[]byte("GIF87a"), []byte("RIFF0000WEBPVP8L"),
	}
	for i := range paths {
		if err := os.WriteFile(filepath.Join(root, paths[i]), headers[i], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := turninput.ValidateImages(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	record := filepath.Join(t.TempDir(), "initial-turn.json")
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-initial-images", "SHIPMATES_FAKE_INPUT_RECORD": record,
	}, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "compare", Fresh: true, Images: batch.Images()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Input []map[string]string `json:"input"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || len(params.Input) != 5 || params.Input[0]["type"] != "text" || params.Input[0]["text"] != "compare" {
		t.Fatalf("initial input=%s err=%v", raw, err)
	}
	for i, path := range paths {
		if params.Input[i+1]["type"] != "localImage" || params.Input[i+1]["path"] != filepath.Join(root, path) {
			t.Fatalf("image %d order=%v", i, params.Input)
		}
	}
	public, _ := json.Marshal(struct {
		Snapshot Snapshot `json:"snapshot"`
		Feed     []Event  `json:"feed"`
	}{s.Snapshot(), s.Feed(0).Events})
	for _, canary := range append(paths, root) {
		if strings.Contains(string(public), canary) {
			t.Fatalf("image path leaked into state/feed: %s", public)
		}
	}
	if !strings.Contains(string(public), `"image_count":4`) {
		t.Fatalf("count-only event missing: %s", public)
	}
	marker, ok, err := project.ReadLiveContinuity("backend")
	if err != nil || !ok {
		t.Fatal("continuity marker missing")
	}
	markerRaw, _ := json.Marshal(marker)
	for _, canary := range append(paths, root) {
		if strings.Contains(string(markerRaw), canary) {
			t.Fatalf("image leaked into continuity: %s", markerRaw)
		}
	}
}

func TestStartNextTurnAuthoritativeRefusalLeavesIdle(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-refusal",
		"SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "resume"), "SHIPMATES_FAKE_REJECT_TURN": "1",
	}
	m := New(nil, codexapp.StartOptions{
		Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env,
		StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond,
	})
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()
	if _, err := m.StartNextTurn(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "secret"); codexapp.ErrorCode(err) != codexapp.BackendRejected {
		t.Fatalf("refusal = %v", err)
	}
	if got := s.Snapshot(); got.State != Idle || got.TurnID != "" {
		t.Fatalf("refusal changed idle owner: %+v", got)
	}
	if _, err := project.AcquireDispatchLock("backend"); err == nil {
		t.Fatal("refusal released owned dispatch lock")
	}
}

func TestAttachedControllerMediatesExactAppServerApproval(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-approval", "SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record"), "SHIPMATES_FAKE_LIVE_EVENTS": "1"}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	attached, err := m.AttachController("backend", s.Snapshot().SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.StartNextTurn(context.Background(), "backend", attached.SessionID, attached.ThreadID, "secret"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	var auth ApprovalAuthority
	for auth.ApprovalID == "" {
		s.mu.Lock()
		if s.approval != nil {
			auth = s.approval.authority
		}
		s.mu.Unlock()
		if auth.ApprovalID != "" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("approval was not bound: %+v", s.Feed(0))
		case <-s.notify:
		}
	}
	if auth.ControllerID != attached.ControllerID || auth.ControllerLeaseGeneration != attached.ControllerLeaseGeneration {
		t.Fatalf("authority=%+v", auth)
	}
	if _, err = m.ResolveApproval(context.Background(), auth, AllowOnce); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.Feed(0))
	if !strings.Contains(string(b), `"outcome":"allowed_once"`) || strings.Contains(string(b), "SECRET_APPROVAL") {
		t.Fatalf("feed=%s", b)
	}
}

func TestPolicyAllowResolvesWithoutController(t *testing.T) {
	fixture(t, "backend")
	allow := "version: 1\nallow:\n  - id: permit.test\n    kind: process.exec\n    match:\n      command_exact: \"SECRET_APPROVAL\"\n    reason: test\nask: []\ndeny: []\n"
	if err := os.WriteFile(project.PolicyPath("backend"), []byte(allow), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "approval")
	env := map[string]string{"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-auto-allow", "SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "resume"), "SHIPMATES_FAKE_LIVE_EVENTS": "1", "SHIPMATES_FAKE_APPROVAL_RECORD": record}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	deadline := time.Now().Add(time.Second)
	for {
		b, err := os.ReadFile(record)
		if err == nil && strings.Contains(string(b), `"decision":"accept"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic allow was not delivered: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	pending := s.approval != nil
	s.mu.Unlock()
	if pending {
		t.Fatal("allow rule created an operator approval")
	}
}

func TestStartIdleSendsNoTurnAndStartNextTurnUsesExactTuple(t *testing.T) {
	fixture(t, "backend")
	turnRecord := filepath.Join(t.TempDir(), "turns")
	env := map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-idle",
		"SHIPMATES_FAKE_RECORD":      filepath.Join(t.TempDir(), "resume"),
		"SHIPMATES_FAKE_TURN_RECORD": turnRecord,
	}
	m := New(nil, codexapp.StartOptions{
		Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env,
		StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond,
	})
	s, err := m.StartIdle(context.Background(), StartIdleOptions{Persona: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()
	if snap.State != Idle || snap.ThreadID != "thread-idle" || snap.TurnID != "" {
		t.Fatalf("idle snapshot = %+v", snap)
	}
	if _, err := os.Stat(turnRecord); !os.IsNotExist(err) {
		t.Fatalf("idle start sent model input: %v", err)
	}
	if _, err := project.AcquireDispatchLock("backend"); err == nil {
		t.Fatal("idle owner did not retain dispatch lock")
	}
	if _, err := m.StartNextTurn(context.Background(), "backend", "stale-session", snap.ThreadID, "secret"); ErrorCode(err) != StaleTarget {
		t.Fatalf("stale start = %v", err)
	}
	result, err := m.StartNextTurn(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.State != Working || result.Snapshot.TurnID != "turn-1" {
		t.Fatalf("working snapshot = %+v", result.Snapshot)
	}
	if _, err := m.StartNextTurn(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "duplicate"); ErrorCode(err) != Busy {
		t.Fatalf("duplicate start = %v", err)
	}
	if _, err := m.Tell(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "turn-1", "finish first"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, Idle)
	second, err := m.StartNextTurn(context.Background(), snap.Persona, snap.SessionID, snap.ThreadID, "second")
	if err != nil || second.Snapshot.TurnID != "turn-2" || second.Snapshot.State != Working {
		t.Fatalf("second turn = %+v, %v", second.Snapshot, err)
	}
	b, err := os.ReadFile(turnRecord)
	if err != nil || string(b) != "turn-1\nturn-2\n" {
		t.Fatalf("turn calls = %q, %v", b, err)
	}
}

func TestInterruptErrorRestoresWorkingAndAllowsTellAndRetry(t *testing.T) {
	for _, mode := range []string{"error-once", "cancel-once"} {
		t.Run(mode, func(t *testing.T) {
			fixture(t, "backend")
			env := map[string]string{
				"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-interrupt-error",
				"SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record"), "SHIPMATES_FAKE_INTERRUPT": mode,
			}
			m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
			s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			target := s.Snapshot()
			ctx := context.Background()
			if mode == "cancel-once" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			if _, err = m.Interrupt(ctx, "backend", target.SessionID, target.ThreadID, target.TurnID); err == nil {
				t.Fatal("interrupt unexpectedly succeeded")
			}
			if got := s.Snapshot(); got.State != Working || got.TurnID != target.TurnID {
				t.Fatalf("failed interrupt left session wedged: %+v", got)
			}
			feedBytes, _ := json.Marshal(s.Feed(0))
			if got := string(feedBytes); !strings.Contains(got, `"kind":"interrupt.refused"`) || strings.Contains(got, "SECRET_ERROR") {
				t.Fatalf("unsafe or missing refusal event: %s", got)
			}

			if mode == "error-once" {
				if _, err = m.Tell(context.Background(), "backend", target.SessionID, target.ThreadID, target.TurnID, "continue"); err != nil {
					t.Fatalf("tell after failed interrupt: %v", err)
				}
				waitForState(t, s, Idle)
				return
			}
			if _, err = m.Interrupt(context.Background(), "backend", target.SessionID, target.ThreadID, target.TurnID); err != nil {
				t.Fatalf("retry interrupt: %v", err)
			}
			if got := s.Snapshot(); got.State != Interrupting {
				t.Fatalf("retry state = %s", got.State)
			}
		})
	}
}

func TestInterruptErrorDoesNotOverwriteRacingTerminalState(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-interrupt-race",
		"SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record"), "SHIPMATES_FAKE_INTERRUPT": "complete-then-error",
	}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	target := s.Snapshot()
	if _, err = m.Interrupt(context.Background(), "backend", target.SessionID, target.ThreadID, target.TurnID); err == nil {
		t.Fatal("interrupt unexpectedly succeeded")
	}
	waitForState(t, s, Idle)
	if got := s.Snapshot(); got.State == Interrupting || got.TurnID != "" {
		t.Fatalf("terminal race was overwritten: %+v", got)
	}
	if _, err = m.Tell(context.Background(), "backend", target.SessionID, target.ThreadID, target.TurnID, "late"); ErrorCode(err) != StaleTarget {
		t.Fatalf("tell after terminal race = %v", err)
	}
}

func waitForState(t *testing.T, s *Session, want State) {
	t.Helper()
	deadline := time.After(time.Second)
	for s.Snapshot().State != want {
		select {
		case <-s.Notify():
		case <-deadline:
			t.Fatalf("state = %s; want %s", s.Snapshot().State, want)
		}
	}
}

func TestObservationSteeringAndBoundedRedaction(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-events", "SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record"), "SHIPMATES_FAKE_LIVE_EVENTS": "1"}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "SECRET_PROMPT"})
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if _, err = m.Tell(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID, "SECRET_STEER"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for s.Snapshot().State != Idle {
		select {
		case <-s.Notify():
		case <-deadline:
			t.Fatal("turn did not complete")
		}
	}
	b, _ := json.Marshal(s.Feed(0))
	got := string(b)
	for _, secret := range []string{"SECRET_PROMPT", "SECRET_STEER", "SECRET_COMMAND", "SECRET_APPROVAL", "SECRET_QUERY"} {
		if strings.Contains(got, secret) {
			t.Fatalf("feed leaked %s: %s", secret, got)
		}
	}
	for _, kind := range []string{"session.starting", "session.ready", "activity", "steering.accepted", "agent.message", "turn.completed"} {
		if !strings.Contains(got, kind) {
			t.Fatalf("missing %s: %s", kind, got)
		}
	}
	if _, err = m.Tell(context.Background(), "backend", snap.SessionID, snap.ThreadID, snap.TurnID, "late"); ErrorCode(err) != StaleTarget {
		t.Fatalf("late steer=%v", err)
	}
	_ = s.Shutdown(context.Background())
}

func TestDelayedAndMismatchedRequestsAreNotRetargeted(t *testing.T) {
	fixture(t, "backend")
	env := map[string]string{
		"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": "thread-delayed",
		"SHIPMATES_FAKE_RECORD": filepath.Join(t.TempDir(), "record"), "SHIPMATES_FAKE_DELAYED_REQUESTS": "1",
	}
	m := New(nil, codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 100 * time.Millisecond})
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	first := s.Snapshot()
	if _, err := m.Tell(context.Background(), "backend", first.SessionID, first.ThreadID, first.TurnID, "finish"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, s, Idle)
	second, err := m.StartNextTurn(context.Background(), "backend", first.SessionID, first.ThreadID, "second")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		b, _ := json.Marshal(s.Feed(0))
		got := string(b)
		if strings.Contains(got, `"kind":"request.refused"`) {
			if strings.Count(got, `"kind":"request.refused"`) != 1 || !strings.Contains(got, `"turn_id":"turn-2","kind":"request.refused"`) {
				t.Fatalf("delayed request was retargeted: %s", got)
			}
			if strings.Contains(got, "SECRET_DELAYED") || strings.Contains(got, "SECRET_CURRENT") {
				t.Fatalf("request payload leaked: %s", got)
			}
			break
		}
		select {
		case <-s.Notify():
		case <-deadline:
			t.Fatalf("current refusal not observed; snapshot=%+v feed=%s", second.Snapshot, got)
		}
	}
}

func TestInterruptRequiresExactNonEmptyTurnTarget(t *testing.T) {
	fixture(t, "backend")
	m := managerFor(t, "thread-interrupt", filepath.Join(t.TempDir(), "record"), false)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	snap := s.Snapshot()

	tests := []struct {
		name, sessionID, threadID, turnID string
		want                              Code
	}{
		{name: "empty", sessionID: snap.SessionID, threadID: snap.ThreadID, want: InvalidTarget},
		{name: "stale", sessionID: "stale-session", threadID: snap.ThreadID, turnID: snap.TurnID, want: StaleTarget},
		{name: "mismatched", sessionID: snap.SessionID, threadID: snap.ThreadID, turnID: "other-turn", want: StaleTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := m.Interrupt(context.Background(), "backend", tt.sessionID, tt.threadID, tt.turnID); ErrorCode(err) != tt.want {
				t.Fatalf("code = %q, err = %v; want %q", ErrorCode(err), err, tt.want)
			}
			if got := s.Snapshot(); got.State != Working || got.TurnID != snap.TurnID {
				t.Fatalf("invalid target changed active turn: %+v", got)
			}
		})
	}
}

func TestShutdownAllReapsChildrenAndReleasesPersonaLocks(t *testing.T) {
	fixture(t, "backend", "tester")
	m := managerFor(t, "thread-clean", filepath.Join(t.TempDir(), "record"), false)
	a, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.StartLive(context.Background(), StartOptions{Persona: "tester", Prompt: "two"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.ShutdownAll(ctx)
	for _, s := range []*Session{a, b} {
		select {
		case <-s.Done():
		case <-time.After(time.Second):
			t.Fatal("child was not shut down")
		}
		if s.Snapshot().State != Stopped {
			t.Fatalf("state=%s", s.Snapshot().State)
		}
	}
	for _, p := range []string{"backend", "tester"} {
		release, err := project.AcquireDispatchLock(p)
		if err != nil {
			t.Fatalf("%s lock retained: %v", p, err)
		}
		release()
	}
}

func fixture(t *testing.T, personas ...string) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join(".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".shipmates", "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	if err := os.WriteFile(filepath.Join(".shipmates", "policy.yaml"), empty, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range personas {
		if err := os.WriteFile(filepath.Join(".codex", "agents", p+".toml"), []byte("developer_instructions = \"role for "+p+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(".shipmates", "policies", p+".yaml"), empty, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func managerFor(t *testing.T, thread, record string, fail bool) *Manager {
	t.Helper()
	env := map[string]string{"SHIPMATES_FAKE_LIVE_SERVER": "1", "SHIPMATES_FAKE_THREAD": thread, "SHIPMATES_FAKE_RECORD": record}
	if fail {
		env["SHIPMATES_FAKE_FAIL_THREAD"] = "1"
	}
	opts := codexapp.StartOptions{Command: []string{os.Args[0], "-test.run=^TestFakeLiveAppServer$"}, Environment: env, StartupTimeout: time.Second, ShutdownTimeout: 50 * time.Millisecond}
	return New(nil, opts)
}

func TestLifecyclePersistsResumesAndFreshStarts(t *testing.T) {
	fixture(t, "backend")
	record := filepath.Join(t.TempDir(), "resume")
	m := managerFor(t, "thread-one", record, false)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "secret task"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot(); got.State != Working || got.ThreadID != "thread-one" || got.TurnID != "turn-1" {
		t.Fatalf("snapshot = %+v", got)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A new manager models a server restart: no active state is adopted, but the
	// exact durable thread is resumed by the next explicit live request.
	m2 := managerFor(t, "unused-fresh-id", record, false)
	s2, err := m2.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "next turn"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(record)
	if err != nil || string(b) != "thread-one" {
		t.Fatalf("resume record = %q, %v", b, err)
	}
	if err := s2.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	m3 := managerFor(t, "thread-fresh", record, false)
	s3, err := m3.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "fresh turn", Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if s3.Snapshot().ThreadID != "thread-fresh" {
		t.Fatalf("fresh = %+v", s3.Snapshot())
	}
	if err := s3.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker, ok, err := project.ReadLiveContinuity("backend")
	if err != nil || !ok || marker.ThreadID != "thread-fresh" {
		t.Fatalf("marker = %+v, %v, %v", marker, ok, err)
	}
	if _, err := os.Stat(filepath.Join(".shipmates", "memory", "backend")); err == nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestFailureRetainsMarkerAndReleasesLock(t *testing.T) {
	fixture(t, "backend")
	fp := strings.Repeat("a", 64)
	prior := project.LiveContinuity{SchemaVersion: 1, Backend: project.CodexAppServerBackend, ThreadID: "last-good", ConfigFingerprint: fp}
	if err := project.WriteLiveContinuity("backend", prior); err != nil {
		t.Fatal(err)
	}
	path := project.LiveContinuityMarker("backend")
	before, _ := os.ReadFile(path)
	m := managerFor(t, "never", filepath.Join(t.TempDir(), "record"), true)
	if _, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "task", Fresh: true}); err == nil {
		t.Fatal("expected failure")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("pre-confirmation failure replaced marker")
	}
	release, err := project.AcquireDispatchLock("backend")
	if err != nil {
		t.Fatalf("lock retained: %v", err)
	}
	release()
}

func TestPersonaOwnershipBusyAndIndependent(t *testing.T) {
	fixture(t, "backend", "tester")
	m := managerFor(t, "thread-one", filepath.Join(t.TempDir(), "record"), false)
	s, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartLive(context.Background(), StartOptions{Persona: "backend", Prompt: "other"}); ErrorCode(err) != Busy {
		t.Fatalf("second live = %v", err)
	}
	if _, err := project.AcquireDispatchLock("backend"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("ask-compatible lock = %v", err)
	}
	other, err := m.StartLive(context.Background(), StartOptions{Persona: "tester", Prompt: "independent"})
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if _, err := m.ResolveTarget("backend", "old-session", snap.ThreadID, snap.TurnID); ErrorCode(err) != StaleTarget {
		t.Fatalf("stale session = %v", err)
	}
	if _, err := m.ResolveTarget("backend", snap.SessionID, snap.ThreadID, "old-turn"); ErrorCode(err) != StaleTarget {
		t.Fatalf("stale turn = %v", err)
	}
	_ = other.Shutdown(context.Background())
	_ = s.Shutdown(context.Background())
	if _, err := m.ResolveTarget("backend", snap.SessionID, snap.ThreadID, ""); ErrorCode(err) != StoppedCode {
		t.Fatalf("stopped target = %v", err)
	}
	restarted := managerFor(t, "thread-new", filepath.Join(t.TempDir(), "record2"), false)
	if _, err := restarted.ResolveTarget("backend", snap.SessionID, snap.ThreadID, snap.TurnID); ErrorCode(err) != NotFound {
		t.Fatalf("restart target = %v", err)
	}
}
