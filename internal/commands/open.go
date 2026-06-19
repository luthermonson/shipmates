package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// Open launches a long-running INTERACTIVE Claude Code session as a persona.
// Unlike Ask (one-shot -p), this hands the terminal to claude's TUI and blocks
// until the user exits. Session continuity mirrors Ask: create on first open,
// resume thereafter.
func Open() *cli.Command {
	return &cli.Command{
		Name:      "open",
		Usage:     "launch a long-running interactive session for a persona",
		ArgsUsage: "<persona>",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates open <persona>")
			}

			agentPath := project.AgentPath(persona)
			raw, err := os.ReadFile(agentPath)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("persona %q is not installed — run: shipmates add %s", persona, persona)
				}
				return err
			}

			meta, err := readPersonaMeta(raw)
			if err != nil {
				return fmt.Errorf("parse %s: %w", agentPath, err)
			}

			name := project.SessionName(persona)
			marker := project.SessionMarker(persona)

			var args []string
			resumed := false
			if _, err := os.Stat(marker); err == nil {
				args = []string{"--resume", name, "--agent", persona}
				resumed = true
			} else {
				args = []string{"--session-id", project.NewUUID(), "--name", name, "--agent", persona}
			}

			if mode := strings.TrimSpace(meta.Permissions.Mode); mode != "" {
				args = append(args, "--permission-mode", mode)
			}

			if rc := meta.remoteControlName(name); rc != "" {
				args = append(args, "--remote-control", rc)
				fmt.Fprintf(os.Stderr,
					"remote control is ON for %q (session %q) — traffic routes through Anthropic-hosted relay infrastructure so the desktop/mobile app can drive it.\n",
					persona, rc)
			}

			cmd := exec.CommandContext(ctx, "claude", args...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}

			if !resumed {
				if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
					return err
				}
				return os.WriteFile(marker, []byte(name), 0o644)
			}
			return nil
		},
	}
}

// personaMeta is the subset of a persona's YAML frontmatter that `open` honors:
// the permission mode and the remote-control knob. remoteControl may be a bool
// or a string, so it's captured as a yaml.Node and decoded on demand.
type personaMeta struct {
	Permissions struct {
		Mode string `yaml:"mode"`
	} `yaml:"permissions"`
	RemoteControl yaml.Node `yaml:"remoteControl"`
}

// readPersonaMeta isolates the YAML frontmatter block and unmarshals the fields
// `open` needs. (The package-level parseFrontmatter intentionally drops nested
// maps like permissions, so it can't supply these.)
func readPersonaMeta(raw []byte) (personaMeta, error) {
	var meta personaMeta
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")
	if !strings.HasPrefix(text, "---\n") {
		return meta, nil
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return meta, nil
	}
	fm := rest[:end]
	if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// remoteControlName resolves the remoteControl knob to a --remote-control value.
// bool true => the persona's session name; a non-empty string => that string;
// false/absent/empty => "" (omit the flag).
func (m personaMeta) remoteControlName(sessionName string) string {
	switch m.RemoteControl.Kind {
	case 0:
		return ""
	case yaml.ScalarNode:
		var b bool
		if err := m.RemoteControl.Decode(&b); err == nil {
			if b {
				return sessionName
			}
			return ""
		}
		var s string
		if err := m.RemoteControl.Decode(&s); err == nil {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
