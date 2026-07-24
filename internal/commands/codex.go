package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/turninput"
)

// dispatchCodex runs one persistent Codex persona turn. Codex's JSONL output
// gives us a stable thread id for later resume without relying on a terminal
// transcript or on any external session format.
func dispatchCodex(ctx context.Context, persona, prompt string, fresh bool, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return err
	}
	return dispatchCodexInstalled(ctx, installed, prompt, fresh, cfg, stdout, stderr)
}

func dispatchCodexInstalled(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
	return dispatchCodexInstalledImages(ctx, installed, prompt, fresh, cfg, nil, stdout, stderr)
}
func dispatchCodexInstalledImages(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, batch *turninput.ImageBatchV1, stdout, stderr io.Writer) error {
	var images []turninput.ImageDescriptorV1
	if batch != nil {
		if err := batch.Revalidate(); err != nil {
			return err
		}
		images = batch.Images()
	}
	return codexTurnDispatcher(ctx, installed, prompt, fresh, cfg, images, stdout, stderr)
}

// dispatchCodexExecInstalledImages is retained for the low-level Codex JSONL
// parser tests. Production dispatch uses app-server mediation above so every
// process request is evaluated against the immutable Shipmates policy snapshot.
func dispatchCodexExecInstalledImages(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, batch *turninput.ImageBatchV1, stdout, stderr io.Writer) error {
	persona := installed.Name
	var images []turninput.ImageDescriptorV1
	if batch != nil {
		images = batch.Images()
	}
	path, err := resolveCodexExecutable()
	if err != nil {
		return err
	}
	if len(images) > 0 {
		if err := probeCodexImageCapability(ctx, path); err != nil {
			return err
		}
	}
	release, err := project.AcquireDispatchLock(persona)
	if err != nil {
		return err
	}
	defer release()
	root, err := os.Getwd()
	if err != nil {
		return errors.New("policy validation failed")
	}
	rootID, err := project.ScopeID(root)
	if err != nil {
		return errors.New("policy validation failed")
	}
	policySnapshot, diagnostics := policy.Load(root, persona, rootID)
	if policySnapshot == nil {
		for _, d := range diagnostics {
			fmt.Fprintf(stderr, "policy: %s %s\n", d.Code, d.Path)
		}
		return errors.New("policy validation failed")
	}
	// Hold the immutable turn snapshot until the subprocess has terminated.
	// `codex exec` is non-interactive and exposes no approval-response channel.
	_ = policySnapshot

	meta, have := project.ReadBackendSessionMeta(persona, "codex")
	fingerprint := cfg.Fingerprint()
	if have && meta.ID != "" && !validThreadID.MatchString(meta.ID) {
		return fmt.Errorf("stored Codex session for %q has an invalid thread id; use --fresh", persona)
	}
	if have && !fresh && meta.ConfigHash != "" && meta.ConfigHash != fingerprint {
		fresh = true
	}

	if batch != nil {
		if err := batch.Revalidate(); err != nil {
			return err
		}
	}
	args, err := codexArgsInstalledImages(installed, prompt, fresh || !have || meta.ID == "", meta.ID, cfg, images)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	// Codex stderr is intentionally not forwarded: it is not part of the
	// normalized progress contract and may contain unrestricted diagnostics.
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "started: Codex persona turn")

	var threadID, finalText, reportedErr string
	scanner := bufio.NewScanner(pipe)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		id, final, eventErr, activity := parseCodexEvent(scanner.Bytes())
		if id != "" {
			threadID = id
		}
		if final != "" {
			finalText = final
		}
		if eventErr != "" {
			reportedErr = eventErr
		}
		if activity != "" {
			fmt.Fprintln(stderr, "activity: "+activity)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex events: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		fmt.Fprintln(stderr, "failed: Codex persona turn")
		if ctx.Err() != nil {
			return fmt.Errorf("codex turn canceled: %w", ctx.Err())
		}
		if reportedErr != "" {
			return fmt.Errorf("codex: %s", scrubTerminalControl(reportedErr))
		}
		return err
	}
	if reportedErr != "" {
		fmt.Fprintln(stderr, "failed: Codex persona turn")
		return fmt.Errorf("codex: %s", reportedErr)
	}
	if threadID == "" {
		fmt.Fprintln(stderr, "failed: Codex persona turn")
		return fmt.Errorf("codex completed without reporting a thread id")
	}
	if !validThreadID.MatchString(threadID) {
		fmt.Fprintln(stderr, "failed: Codex persona turn")
		return fmt.Errorf("codex reported an invalid thread id")
	}
	if strings.TrimSpace(finalText) == "" {
		fmt.Fprintln(stderr, "failed: Codex persona turn")
		return fmt.Errorf("codex completed without a final answer")
	}
	if err := project.WriteBackendSessionMeta(persona, "codex", threadID, threadID, fingerprint); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, finalText)
	fmt.Fprintln(stderr, "completed: Codex persona turn")
	return nil
}

var validThreadID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

func codexArgs(persona, prompt string, fresh bool, threadID string, cfg project.PersonaConfig) ([]string, error) {
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return nil, err
	}
	return codexArgsInstalled(installed, prompt, fresh, threadID, cfg)
}

func codexArgsInstalled(installed *project.InstalledPersona, prompt string, fresh bool, threadID string, cfg project.PersonaConfig) ([]string, error) {
	return codexArgsInstalledImages(installed, prompt, fresh, threadID, cfg, nil)
}
func codexArgsInstalledImages(installed *project.InstalledPersona, prompt string, fresh bool, threadID string, cfg project.PersonaConfig, images []turninput.ImageDescriptorV1) ([]string, error) {
	if !fresh && threadID != "" {
		args := []string{"exec", "resume", threadID, "--json"}
		for _, image := range images {
			args = append(args, "--image", image.AbsolutePath())
		}
		if len(images) != 0 {
			args = append(args, "--")
		}
		return append(args, prompt), nil
	}

	instructions := codexPersonaPromptInstalled(installed, prompt)
	args := []string{"exec", "--json", "--sandbox", "workspace-write"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--config", "model_reasoning_effort="+strconv.Quote(cfg.Effort))
	}
	for _, image := range images {
		args = append(args, "--image", image.AbsolutePath())
	}
	if len(images) != 0 {
		args = append(args, "--")
	}
	args = append(args, instructions)
	return args, nil
}

func resolveCodexExecutable() (string, error) {
	p, e := exec.LookPath("codex")
	if e != nil {
		return "", fmt.Errorf("codex not on PATH: %w", e)
	}
	p, e = filepath.EvalSymlinks(p)
	if e != nil {
		return "", errors.New("codex executable unavailable")
	}
	p, e = filepath.Abs(p)
	if e != nil {
		return "", errors.New("codex executable unavailable")
	}
	st, e := os.Stat(p)
	if e != nil || !st.Mode().IsRegular() {
		return "", errors.New("codex executable unavailable")
	}
	return p, nil
}
func probeCodexImageCapability(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, path, "exec", "--help")
	b, e := cmd.Output()
	if e != nil || len(b) > 1<<20 || !strings.Contains(string(b), "--image") {
		return errors.New("codex_image_unsupported")
	}
	return nil
}

func codexPersonaPrompt(persona, prompt string) (string, error) {
	installed, err := project.CanonicalPersonaAt(".", persona)
	if err != nil {
		return "", err
	}
	return codexPersonaPromptInstalled(installed, prompt), nil
}

func codexPersonaPromptInstalled(installed *project.InstalledPersona, prompt string) string {
	persona := installed.Name
	return strings.TrimSpace(installed.Doc.Instructions) + "\n\n## Codex Harness\n\n" +
		"You are the Shipmates " + persona + " persona, running through Codex. " +
		"Read the relevant files under `.shipmates/memory/" + persona + "/` before work. " +
		"Record durable, verified project knowledge there when it will help a later task. " +
		"Use the workspace-write sandbox only for changes required by this task. " +
		"Do not recursively delegate through Shipmates. Return control after the requested deliverable.\n\n" +
		"## Task\n\n" + strings.TrimSpace(prompt)
}

func parseCodexEvent(line []byte) (threadID, finalText, eventErr, activity string) {
	var event struct {
		Type     string          `json:"type"`
		ThreadID string          `json:"thread_id"`
		Item     json.RawMessage `json:"item"`
		Error    json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return "", "", "", ""
	}
	threadID = event.ThreadID
	if event.Type == "item.completed" && len(event.Item) > 0 {
		var item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Item, &item) == nil && (item.Type == "agent_message" || item.Type == "agentMessage") {
			finalText = item.Text
		} else if item.Type != "" {
			activity = safeActivity(item.Type)
		}
	}
	if event.Type == "item.started" && len(event.Item) > 0 {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Item, &item) == nil {
			activity = safeActivity(item.Type)
		}
	}
	if event.Type == "error" && len(event.Error) > 0 {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(event.Error, &detail) == nil {
			eventErr = detail.Message
		}
	}
	return threadID, finalText, eventErr, activity
}

// scrubTerminalControl strips control characters and BiDi override runes from
// a string so an attacker-influenced field (e.g. a Codex-reported error
// message) cannot rewrite the operator's terminal via ANSI escapes.
func scrubTerminalControl(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '?'
		}
		return r
	}, s)
}

func safeActivity(itemType string) string {
	switch itemType {
	case "command_execution", "commandExecution":
		return "running a command"
	case "file_change", "fileChange":
		return "updating files"
	case "web_search", "webSearch":
		return "searching documentation"
	case "mcp_tool_call", "mcpToolCall":
		return "using a connected tool"
	default:
		return ""
	}
}
