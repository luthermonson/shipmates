package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// The claude-cli conversation backend: instead of an OpenAI-compatible LLM
// server with a hand-rolled tool catalog, each voice turn runs through a
// persistent `claude -p` session on the bridge host — the captain's mate is
// itself a mate, using the operator's existing Claude Code login (no API
// key). Tools come for free: the session is allowed to run `shipmates
// bridge …` CLI commands and curl the bridge's own API, which covers
// everything the JSON tool catalog did and more.
//
// History lives in the claude session (resumed by id per turn), so only the
// newest user message travels; the UI's history array is display-only here.

// claudeBrain serializes turns (voice is sequential anyway) and tracks the
// resumable session.
type claudeBrain struct {
	mu        sync.Mutex
	sessionID string
	model     string // claude model tag/alias (e.g. "haiku"); "" = CLI default
	addr      string // this bridge's listen address, for the tool instructions
	token     string // bridge bearer token, passed via env to the session
}

// captainPrompt is the appended system prompt for the captain's-mate session.
// It teaches the CLI-and-curl tool surface instead of JSON function calls.
const captainPrompt = `You are the operator's VOICE assistant for a fleet of AI coding agents
(shipmates). Your replies are spoken aloud: 1-2 short sentences, no markdown,
no code blocks, no URLs.

You control the fleet by running "shipmates bridge" commands with the Bash tool:
- shipmates bridge ls                              → list ships (leads) and whether connected
- shipmates bridge status                          → per-mate status: blocked|working|idle|done|off
- shipmates bridge tell <ship> <persona> <msg…>    → message a mate (wakes it if asleep)
- shipmates bridge tail <ship>                     → recent activity feed (read replies here)
- shipmates bridge pending <ship>                  → that ship's pending permission requests
- shipmates bridge resolve <ship> <id> allow|deny  → decide a pending request
- shipmates bridge beads [ship]                    → open beads (the fleet's shared work graph)
- shipmates bridge dispatch <carrying-ship> <bead-id> <target-ship> <persona>
                                                   → assign a bead and wake that mate to work it

Ship ids look like "laptop:lead". Mates with status "off" are asleep, not
gone — a tell wakes them. Never invent bead ids; read them with the beads
command first. Do the work with commands, then give the operator the short
spoken answer.`

// newClaudeBrain wires the backend from bridge options.
func newClaudeBrain(model, addr, token string) *claudeBrain {
	return &claudeBrain{model: model, addr: addr, token: token}
}

// claudeTurnResult is the subset of `claude -p --output-format json` we read.
type claudeTurnResult struct {
	Result    string  `json:"result"`
	SessionID string  `json:"session_id"`
	IsError   bool    `json:"is_error"`
	TotalCost float64 `json:"total_cost_usd"`
}

// turn runs one conversation turn and returns the spoken reply. A failed
// resume (dead/aged session id) retries once with a fresh session.
func (c *claudeBrain) turn(ctx context.Context, userText string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reply, err := c.run(ctx, userText)
	if err != nil && c.sessionID != "" {
		slog.Warn("claude brain: resume failed, starting fresh", "err", err)
		c.sessionID = ""
		return c.run(ctx, userText)
	}
	return reply, err
}

// run executes one `claude -p` invocation. Caller holds the mutex.
func (c *claudeBrain) run(ctx context.Context, userText string) (string, error) {
	args := []string{"-p", "--output-format", "json"}
	if c.sessionID != "" {
		args = append(args, "--resume", c.sessionID)
	} else {
		args = append(args, "--append-system-prompt", captainPrompt)
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	args = append(args, "--allowedTools", "Bash(shipmates bridge:*),Bash(curl:*)")

	cmd := exec.CommandContext(ctx, "claude", args...)
	// the user turn rides stdin — a positional prompt after --allowedTools
	// gets swallowed by that flag's variadic parsing
	cmd.Stdin = strings.NewReader(userText)
	cmd.Env = c.childEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude: %v: %s", err, firstLine(stderr.String()))
	}
	return c.readResult(stdout.Bytes())
}

func (c *claudeBrain) readResult(out []byte) (string, error) {
	var res claudeTurnResult
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("decode claude output: %w", err)
	}
	if res.SessionID != "" {
		c.sessionID = res.SessionID
	}
	if res.IsError {
		return "", fmt.Errorf("claude turn errored: %s", firstLine(res.Result))
	}
	slog.Info("claude brain turn", "cost_usd", res.TotalCost, "session", res.SessionID)
	return despeak(res.Result), nil
}

// despeak strips the markdown claude drifts into despite the spoken-output
// instruction — a TTS voice reading "asterisk asterisk" is worse than a
// slightly flatter sentence.
func despeak(s string) string {
	s = strings.NewReplacer("**", "", "`", "", "__", "").Replace(s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		t = strings.TrimLeft(t, "#")
		t = strings.TrimPrefix(strings.TrimSpace(t), "- ")
		t = strings.TrimSpace(t)
		// markdown tables: drop |---|---| separator rows entirely; flatten
		// cell rows to comma phrases so the voice reads data, not pipes
		if strings.Contains(t, "|") {
			if strings.Trim(t, "|-: ") == "" {
				continue
			}
			var cells []string
			for _, c := range strings.Split(t, "|") {
				if c = strings.TrimSpace(c); c != "" && c != "—" && c != "-" {
					cells = append(cells, c)
				}
			}
			t = strings.Join(cells, ", ")
		}
		out = append(out, t)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// childEnv equips the session to reach the fleet: bridge URL + token, and the
// running binary's own directory FIRST on PATH so "shipmates" resolves to
// this exact build — not a stale copy elsewhere on the system. The prepend
// must edit the EXISTING Path entry: appending a second "PATH=" var loses to
// Windows' canonical "Path=" and the stale copy wins.
func (c *claudeBrain) childEnv() []string {
	env := os.Environ()
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if exeDir != "" {
		prepended := false
		for i, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "PATH") {
				env[i] = k + "=" + exeDir + string(os.PathListSeparator) + v
				prepended = true
				break
			}
		}
		if !prepended {
			env = append(env, "PATH="+exeDir)
		}
	}
	return append(env,
		"SHIPMATES_BRIDGE_URL=http://"+c.addr,
		"SHIPMATES_BRIDGE_TOKEN="+c.token,
	)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// lastUserMessage extracts the newest user turn from the UI's history array —
// the claude session keeps its own memory, so that's all it needs.
func lastUserMessage(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}
