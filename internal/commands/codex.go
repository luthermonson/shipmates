package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	personadef "github.com/luthermonson/shipmates/internal/persona"
	"github.com/luthermonson/shipmates/internal/project"
)

// dispatchCodex runs one persistent Codex persona turn. Codex's JSONL output
// gives us a stable thread id for later resume without relying on a terminal
// transcript or on the Claude-specific session format.
func dispatchCodex(ctx context.Context, persona, prompt string, fresh bool, cfg project.PersonaConfig, stdout, stderr io.Writer) error {
	if _, err := os.Stat(project.AgentPath(persona)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("persona %q is not installed — run: shipmates add %s", persona, persona)
		}
		return err
	}

	meta, have := project.ReadBackendSessionMeta(persona, "codex")
	fingerprint := cfg.Fingerprint()
	if have && !fresh && meta.ConfigHash != "" && meta.ConfigHash != fingerprint {
		fresh = true
	}

	args, err := codexArgs(persona, prompt, fresh || !have || meta.ID == "", meta.ID, cfg)
	if err != nil {
		return err
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return fmt.Errorf("codex not on PATH: %w", err)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	// Codex treats piped stdin as additional prompt context. Give it immediate
	// EOF so headless Shipmates turns never pause waiting for terminal input.
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Codex is working as %s; the final response will print here.\n", persona)

	var threadID, finalText, reportedErr string
	scanner := bufio.NewScanner(pipe)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		id, final, eventErr := parseCodexEvent(scanner.Bytes())
		if id != "" {
			threadID = id
		}
		if final != "" {
			finalText = final
		}
		if eventErr != "" {
			reportedErr = eventErr
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex events: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if reportedErr != "" {
			return fmt.Errorf("codex: %s", reportedErr)
		}
		return err
	}
	if reportedErr != "" {
		return fmt.Errorf("codex: %s", reportedErr)
	}
	if finalText != "" {
		_, _ = fmt.Fprintln(stdout, finalText)
	}
	if threadID == "" {
		return fmt.Errorf("codex completed without reporting a thread id")
	}
	return project.WriteBackendSessionMeta(persona, "codex", threadID, threadID, fingerprint)
}

func codexArgs(persona, prompt string, fresh bool, threadID string, cfg project.PersonaConfig) ([]string, error) {
	if !fresh && threadID != "" {
		return []string{"exec", "resume", threadID, "--json", prompt}, nil
	}

	instructions, err := codexPersonaPrompt(persona, prompt)
	if err != nil {
		return nil, err
	}
	args := []string{"exec", "--json", "--sandbox", "workspace-write"}
	if cfg.Model != "" && !strings.HasPrefix(strings.ToLower(cfg.Model), "claude") {
		args = append(args, "--model", cfg.Model)
	}
	args = append(args, instructions)
	return args, nil
}

func codexPersonaPrompt(persona, prompt string) (string, error) {
	raw, err := os.ReadFile(project.AgentPath(persona))
	if err != nil {
		return "", err
	}
	def, err := personadef.Parse(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(def.Body) + "\n\n## Codex Harness\n\n" +
		"You are the Shipmates " + persona + " persona, running through Codex. " +
		"Read the relevant files under `.shipmates/memory/" + persona + "/` before work. " +
		"Record durable, verified project knowledge there when it will help a later task. " +
		"Use the workspace-write sandbox only for changes required by this task.\n\n" +
		"## Task\n\n" + strings.TrimSpace(prompt), nil
}

func parseCodexEvent(line []byte) (threadID, finalText, eventErr string) {
	var event struct {
		Type     string          `json:"type"`
		ThreadID string          `json:"thread_id"`
		Item     json.RawMessage `json:"item"`
		Error    json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return "", "", ""
	}
	threadID = event.ThreadID
	if event.Type == "item.completed" && len(event.Item) > 0 {
		var item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Item, &item) == nil && (item.Type == "agent_message" || item.Type == "agentMessage") {
			finalText = item.Text
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
	return threadID, finalText, eventErr
}
