package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/berth"
	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/runtime/env"
	"github.com/luthermonson/shipmates/internal/turninput"
	"github.com/urfave/cli/v3"
)

// selector resolves which runtime `ask` dispatches through — the first
// command migrated onto the runtime interface (runtime-interface-plan.md
// Phase 6). Package-level so tests can swap in a fake.
var selector runtime.Selector = env.New()

// Ask runs a one-shot, turn-based delegation: create the persona's session the
// first time, resume it afterward. Output streams to the terminal.
//
// Ask is the first command that honors the runtime selection (`--runtime`
// CLI flag / SHIPMATES_RUNTIME / `runtime:` config): when the selection
// resolves to claude, the turn dispatches through the runtime interface;
// otherwise it takes the codex-native path unchanged. The `--runtime` flag
// itself is declared once on the root command (main.go) and read here via
// urfave/cli's lineage lookup.
func Ask() *cli.Command {
	return &cli.Command{
		Name:      "ask",
		Usage:     "one-shot delegation to a persona (turn-based; resumes its session)",
		ArgsUsage: "<persona> <prompt>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (applies config changes like model/effort)"},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "maximum wall-clock duration for the crew turn"},
			removedImageFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if len(c.StringSlice("image")) > 0 {
				return removedImageFlagError("ask")
			}
			persona := c.Args().First()
			prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
			if persona == "" || prompt == "" {
				return errors.New("usage: shipmates ask <persona> <prompt>")
			}
			turnCtx, cancel := context.WithTimeout(ctx, c.Duration("timeout"))
			defer cancel()
			return dispatchAsk(turnCtx, c.String("runtime"), persona, prompt, c.Bool("fresh"))
		},
	}
}

// removedImageFlag keeps `--image` parseable but hidden on ask and live, so
// an operator who still types it gets removedImageFlagError instead of
// urfave/cli's bare "flag provided but not defined".
func removedImageFlag() cli.Flag {
	return &cli.StringSliceFlag{Name: "image", Hidden: true, Usage: "removed — use `shipmates show`"}
}

func removedImageFlagError(command string) error {
	return fmt.Errorf("--image was removed from %s; use `shipmates show <persona> <file-path>... [--caption <text>]`, which attaches any file, not just images", command)
}

// dispatchAsk routes an ask turn: resolve the runtime via the Selector,
// dispatch through the runtime interface when it resolves to claude, and
// fall back to the codex-native dispatcher otherwise.
func dispatchAsk(ctx context.Context, cliRuntime, persona, prompt string, fresh bool) error {
	return dispatchAskTo(ctx, cliRuntime, persona, prompt, fresh, os.Stdout, os.Stderr)
}

func dispatchAskTo(ctx context.Context, cliRuntime, persona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	rt, source, err := selectAskRuntime(ctx, cliRuntime)
	if err != nil {
		return err
	}
	if rt == nil {
		// Codex-native path, exactly as before the runtime interface.
		return dispatchTo(ctx, persona, prompt, fresh, stdout, stderr)
	}
	defer rt.Close(ctx)
	slog.Debug("ask: dispatching through runtime interface", "runtime", rt.Name(), "source", source, "persona", persona)
	return dispatchRuntimeTurn(ctx, rt, persona, prompt, fresh, nil, stdout, stderr)
}

// selectAskRuntime resolves the runtime for an ask turn. A nil Runtime with
// a nil error means "use the codex-native dispatcher": codex is selected
// (its transport needs codexapp.StartOptions the Selector cannot carry, so
// the Selector reports ErrNotConfigured for it by design).
func selectAskRuntime(ctx context.Context, cliRuntime string) (runtime.Runtime, string, error) {
	rt, source, err := selector.Select(ctx, ".", cliRuntime)
	if err != nil {
		var notCfg *runtime.ErrNotConfigured
		if errors.As(err, &notCfg) && notCfg.Runtime == "codex" {
			return nil, source, nil
		}
		return nil, source, fmt.Errorf("select runtime: %w", err)
	}
	if rt.Name() == "codex" {
		// A future Selector may construct codex directly; the codex-native
		// dispatcher already carries the full policy/containment posture, so
		// prefer it until the interface migration covers those.
		_ = rt.Close(ctx)
		return nil, source, nil
	}
	return rt, source, nil
}

// dispatchRuntimeTurn sends one turn (optionally carrying attachments)
// through a runtime.Runtime, streams its normalized events, and persists
// the session marker for later resume.
func dispatchRuntimeTurn(ctx context.Context, rt runtime.Runtime, persona, prompt string, fresh bool, attachments []runtime.Attachment, stdout, stderr io.Writer) error {
	if persona == "captain" {
		return errors.New("captain is reserved for the human operator; use skipper or quartermaster")
	}
	if _, err := project.CanonicalPersonaAt(".", persona); err != nil {
		return err
	}
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return err
	}
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return err
	}

	// Where the turn's tool calls originate. Empty means "the project root",
	// which is what every project without a configured berth gets. A berth is
	// created or reused here, before the session exists, so the very first
	// turn already lands in it.
	cwd, err := berth.ResolveSpawnCWDAt(root, persona, cfg)
	if err != nil {
		return err
	}

	// Session persistence mirrors the codex marker scheme:
	// .shipmates/sessions/<persona>.<runtime>.session with an auto-fresh on
	// config drift. The resolved working directory is part of that drift: a
	// session is never resumed into a different directory than the one it was
	// created in (see berth.SessionFingerprint).
	fingerprint := berth.SessionFingerprint(cfg.Fingerprint(), cwd)
	meta, have := project.ReadBackendSessionMeta(persona, rt.Name())
	if have && !fresh && meta.ConfigHash != "" && meta.ConfigHash != fingerprint {
		fresh = true
	}

	spec := runtime.SessionSpec{Persona: persona, ProjectDir: root, WorkingDir: cwd}
	var session runtime.Session
	if have && !fresh && meta.ID != "" {
		session, err = rt.ResumeSession(ctx, meta.ID, spec)
	} else {
		session, err = rt.StartSession(ctx, spec)
	}
	if err != nil {
		return fmt.Errorf("start %s session: %w", rt.Name(), err)
	}

	// A runtime that asks before running tools (claude's can_use_tool
	// control protocol) blocks the turn until it gets an answer, and `ask`
	// is one-shot: nobody is watching a feed and no controller holds a
	// lease. The persona's immutable policy snapshot is therefore the only
	// authority available, and it answers every request — allow when a rule
	// says so, deny otherwise. Loading it here means a bad policy fails the
	// turn before any prompt is sent, exactly like the codex path.
	//
	// Where the secure loader does not exist (non-unix), there is no
	// authority at all: the turn still runs, but every request that reaches
	// shipmates is refused. Nothing is silently waved through.
	var snapshot *policy.Snapshot
	if rt.Capabilities().Approvals {
		if !policy.SecureLoadSupported() {
			fmt.Fprintln(stderr, "approval: no secure policy loader on this platform; every tool permission request will be denied")
		} else {
			rootID, idErr := project.ScopeID(root)
			if idErr != nil {
				return errors.New("policy validation failed")
			}
			var diagnostics []policy.Diagnostic
			snapshot, diagnostics = policy.Load(root, persona, rootID)
			if snapshot == nil {
				for _, d := range diagnostics {
					fmt.Fprintf(stderr, "policy: %s %s\n", d.Code, d.Path)
				}
				return errors.New("policy validation failed")
			}
		}
	}

	if _, err := rt.SendTurn(ctx, session.ID(), runtime.TurnInput{Text: prompt, Attachments: attachments}); err != nil {
		return fmt.Errorf("send turn: %w", err)
	}
	if err := streamRuntimeEvents(ctx, rt, snapshot, stdout, stderr); err != nil {
		return err
	}
	return project.WriteBackendSessionMeta(persona, rt.Name(), persona, session.ID(), fingerprint)
}

// streamRuntimeEvents consumes the runtime's normalized event stream until
// the turn finishes. KindText goes to stdout; tool activity is summarized
// on stderr (matching the codex path's "activity:" lines); KindError ends
// the turn with a non-nil error. Approval requests are answered from the
// turn's policy snapshot — see askApproval.
func streamRuntimeEvents(ctx context.Context, rt runtime.Runtime, snapshot *policy.Snapshot, stdout, stderr io.Writer) error {
	events := rt.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				// Stream closed without a terminal event — treat as clean
				// EOF so the caller can persist session meta and exit.
				return nil
			}
			switch ev.Kind {
			case runtime.KindText:
				if s, _ := ev.Payload.(string); s != "" {
					fmt.Fprint(stdout, s)
					if !strings.HasSuffix(s, "\n") {
						fmt.Fprintln(stdout)
					}
				}
			case runtime.KindToolCall:
				if tc, ok := ev.Payload.(claude.ToolCall); ok && tc.Name != "" {
					fmt.Fprintln(stderr, "activity: "+tc.Name)
				}
			case runtime.KindApprovalNeeded:
				askApproval(ctx, rt, snapshot, ev, stderr)
			case runtime.KindTurnDone:
				return nil
			case runtime.KindError:
				return fmt.Errorf("%s turn failed: %s", rt.Name(), formatRuntimeErrorPayload(ev.Payload))
			default:
				// KindToolResult / KindBackend / KindSessionClosed are not
				// part of the one-shot ask UX.
				slog.Debug("ask: ignored runtime event", "kind", ev.Kind)
			}
		}
	}
}

// askApproval answers one approval request on the one-shot `ask` path.
//
// There is no operator to prompt and no controller lease to hold, so policy
// is the whole decision: a `process.exec` rule whose effect is allow lets
// the call through, anything else refuses it. An unanswered request would
// hang the turn until the runtime's own timeout, so every request gets an
// answer even when it cannot be evaluated. Use `shipmates live` when a
// human should decide instead.
func askApproval(ctx context.Context, rt runtime.Runtime, snapshot *policy.Snapshot, ev runtime.Event, stderr io.Writer) {
	req, ok := ev.Payload.(claude.ApprovalRequest)
	if !ok || req.RequestID == "" {
		return
	}
	command, named := livesession.ApprovalCommandExact(req)
	effect := policy.Ask
	if named && snapshot != nil {
		effect = policy.Evaluate(snapshot, policy.Request{Kind: policy.ProcessExec, CommandExact: command}).PolicyEffect
	}
	decision := runtime.ApprovalDecision{AllowFor: "once"}
	if effect == policy.Allow {
		decision.Allow = true
		fmt.Fprintf(stderr, "approval: allowed by policy: %s\n", command)
	} else {
		decision.Rationale = "No shipmates policy rule allows this, and `ask` has no operator to approve it. Re-run under `shipmates live`, or add an allow rule."
		fmt.Fprintf(stderr, "approval: denied (no allow rule; `ask` cannot prompt): %s\n", command)
	}
	answerCtx, cancel := context.WithTimeout(ctx, approvalAnswerTimeout)
	defer cancel()
	if _, err := rt.ResolveApproval(answerCtx, runtime.ApprovalResponse{
		ID: req.RequestID, SessionID: ev.SessionID, TurnID: ev.TurnID, Kind: "tool_use",
	}, decision); err != nil {
		slog.Warn("ask: could not answer approval request", "runtime", rt.Name(), "request", req.RequestID, "error", err)
	}
}

// approvalAnswerTimeout bounds a single approval write so a wedged runtime
// cannot stall the ask event loop.
const approvalAnswerTimeout = 5 * time.Second

// formatRuntimeErrorPayload renders whatever a runtime put in an error
// event as a scrubbed, single-line string safe for the terminal.
func formatRuntimeErrorPayload(payload any) string {
	var s string
	switch p := payload.(type) {
	case string:
		s = p
	case []byte:
		s = string(p)
	case error:
		s = p.Error()
	default:
		s = fmt.Sprintf("%v", p)
	}
	return scrubTerminalControl(s)
}

func Live() *cli.Command {
	return &cli.Command{Name: "live", Usage: "start a live turn", ArgsUsage: "<persona> <prompt>", Flags: []cli.Flag{&cli.BoolFlag{Name: "fresh"}, removedImageFlag()}, Action: func(ctx context.Context, c *cli.Command) error {
		if len(c.StringSlice("image")) > 0 {
			return removedImageFlagError("live")
		}
		persona := c.Args().First()
		prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
		if persona == "" || prompt == "" {
			return errors.New("usage: shipmates live <persona> <prompt>")
		}
		if err := project.ValidatePersonaName(persona); err != nil {
			return err
		}
		if err := client.EnsureRunning(); err != nil {
			return err
		}
		body := map[string]any{"prompt": prompt, "fresh": c.Bool("fresh")}
		resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona), body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return safeServerError(resp)
		}
		var snap livesession.Snapshot
		if json.NewDecoder(resp.Body).Decode(&snap) != nil {
			return errors.New("invalid live server response")
		}
		if err := json.NewEncoder(c.Writer).Encode(snap); err != nil {
			return err
		}
		fmt.Fprintf(c.ErrWriter, "live session ready; watch with: shipmates feed %s --follow\n", persona)
		return nil
	}}
}

func safeServerError(resp *http.Response) error {
	return errors.New(serverErrorCode(resp))
}

// serverErrorCode extracts the server's stable error code, never its
// message: response bodies are protocol data and are not surfaced verbatim.
func serverErrorCode(resp *http.Response) string {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
	if v.Code == "" {
		return "server_error"
	}
	return v.Code
}

// dispatch runs a one-shot turn-based delegation: resolve the persona's config,
// create the session the first time / resume it after (auto-fresh on config
// drift or when fresh is requested), apply launch flags, run, and record the
// session. Shared by `ask` and `drain`. Output streams to the terminal.
func dispatch(ctx context.Context, persona, prompt string, fresh bool) error {
	return dispatchImages(ctx, persona, prompt, fresh, nil)
}

func dispatchImages(ctx context.Context, persona, prompt string, fresh bool, paths []string) error {
	return dispatchToImages(ctx, persona, prompt, fresh, paths, os.Stdout, os.Stderr)
}

// dispatchTo is dispatch with caller-supplied output writers, so parallel
// callers (drain-many) can capture each persona's output into its own buffer.
func dispatchTo(ctx context.Context, persona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	return dispatchToImages(ctx, persona, prompt, fresh, nil, stdout, stderr)
}
func dispatchToImages(ctx context.Context, persona, prompt string, fresh bool, paths []string, stdout, stderr io.Writer) error {
	if persona == "captain" {
		return errors.New("captain is reserved for the human operator; use skipper or quartermaster")
	}
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return err
	}
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return err
	}
	root, err := project.CanonicalRoot(".")
	if err != nil {
		return errors.New("image_root_invalid")
	}
	batch, err := turninput.ValidateImages(root, paths)
	if err != nil {
		return err
	}
	defer batch.Close()
	return dispatchToInstalledImages(ctx, installed, prompt, fresh, cfg, batch, stdout, stderr)
}

func dispatchToInstalled(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	persona := installed.Name
	cfg, err := project.ResolvePersonaConfig(persona)
	if err != nil {
		return err
	}
	return dispatchToInstalledImages(ctx, installed, prompt, fresh, cfg, nil, stdout, stderr)
}
func dispatchToInstalledImages(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, batch *turninput.ImageBatchV1, stdout, stderr io.Writer) error {
	return dispatchCodexInstalledImages(ctx, installed, prompt, fresh, cfg, batch, stdout, stderr)
}

// Tell sends a plain-string message to a live crew process via the server. The
// CLI translates the string to a stream-json user message server-side; the caller
// never touches JSON.
func Tell() *cli.Command {
	return &cli.Command{
		Name:      "tell",
		Usage:     "send a message to a live crew process while it works",
		ArgsUsage: "<persona> <session-id> <thread-id> <turn-id> <message>",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			tail := c.Args().Tail()
			if persona == "" || len(tail) < 4 {
				return errors.New("usage: shipmates tell <persona> <session-id> <thread-id> <turn-id> <message>")
			}
			msg := strings.TrimSpace(strings.Join(tail[3:], " "))
			if msg == "" {
				return errors.New("usage: shipmates tell <persona> <session-id> <thread-id> <turn-id> <message>")
			}
			if err := client.EnsureRunning(); err != nil {
				return err
			}
			resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/tell", map[string]string{"session_id": tail[0], "thread_id": tail[1], "turn_id": tail[2], "message": msg})
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return safeServerError(resp)
			}
			_, err = io.Copy(c.Writer, io.LimitReader(resp.Body, 64<<10))
			return err
		},
	}
}

// Feed prints the server's activity feed (crew output, tells, events).
func Feed() *cli.Command {
	return &cli.Command{
		Name: "feed", ArgsUsage: "<persona>", Flags: []cli.Flag{&cli.BoolFlag{Name: "follow"}, &cli.Uint64Flag{Name: "after"}},
		Usage: "print the live activity feed from the server",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates feed <persona> [--follow] [--after sequence]")
			}
			if !client.Healthy() {
				return errors.New("no server running")
			}
			path := fmt.Sprintf("/api/live/%s/feed?after=%d&follow=%t", url.PathEscape(persona), c.Uint64("after"), c.Bool("follow"))
			resp, err := client.Do(ctx, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return safeServerError(resp)
			}
			if resp.Header.Get("X-Shipmates-History-Dropped") == "true" {
				fmt.Fprintln(c.ErrWriter, "warning: requested feed history was dropped")
			}
			_, err = io.Copy(c.Writer, resp.Body)
			return err
		},
	}
}

func Interrupt() *cli.Command {
	return &cli.Command{Name: "interrupt", Usage: "interrupt an exact Codex live turn", ArgsUsage: "<persona> <session-id> <thread-id> <turn-id>", Action: func(ctx context.Context, c *cli.Command) error {
		persona := c.Args().First()
		tail := c.Args().Tail()
		if persona == "" || len(tail) != 3 || tail[2] == "" {
			return errors.New("invalid_target")
		}
		if err := client.EnsureRunning(); err != nil {
			return err
		}
		resp, err := client.Do(ctx, http.MethodPost, "/api/live/"+url.PathEscape(persona)+"/interrupt", map[string]string{"session_id": tail[0], "thread_id": tail[1], "turn_id": tail[2]})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return safeServerError(resp)
		}
		_, err = io.Copy(c.Writer, io.LimitReader(resp.Body, 64<<10))
		return err
	}}
}
