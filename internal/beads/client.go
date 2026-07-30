// Package beads provides a bounded adapter to the external bd CLI.
//
// Shipmates intentionally does not own Beads storage or schema. It stores only
// opaque issue IDs and invokes documented bd commands with argv-only execution.
package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultTimeout = 30 * time.Second
	MaxOutput      = 1 << 20
)

type Client struct {
	Root    string
	Command string
	Timeout time.Duration
}

func Workspace(root string) bool {
	info, err := os.Lstat(filepath.Join(root, ".beads"))
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func New(root string) (*Client, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if !Workspace(resolved) {
		return nil, errors.New("Beads workspace not initialized; run: shipmates beads init")
	}
	command, err := Available()
	if err != nil {
		return nil, errors.New("bd is not installed; see https://github.com/gastownhall/beads")
	}
	return &Client{Root: resolved, Command: command, Timeout: DefaultTimeout}, nil
}

// Available resolves the external bd CLI from PATH and nowhere else.
//
// It deliberately does not fall back to a bd sitting next to the shipmates
// executable: that turns any writable install directory — a downloads folder, a
// shared tools share, an unpacked release tree — into an implicit code-execution
// path for a binary the operator never chose to install. Beads is optional, so
// failing closed and telling the operator to install bd is the safe answer.
func Available() (string, error) {
	if command, err := exec.LookPath("bd"); err == nil {
		return command, nil
	}
	return "", errors.New("bd is not installed; see https://github.com/gastownhall/beads")
}

func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	if c == nil || c.Command == "" || c.Root == "" {
		return "", errors.New("invalid Beads client")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, c.Command, args...)
	cmd.Dir = c.Root
	// Beads is an optional external integration. Do not expose the parent
	// process's credentials or unrelated configuration to it; the CLI only
	// needs a conventional runtime environment and its explicit workspace.
	cmd.Env = boundedEnvironment()
	stdout, stderr := &limitedBuffer{limit: MaxOutput}, &limitedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", errors.New("bd command timed out")
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512]
		}
		if detail == "" {
			return "", fmt.Errorf("bd command failed: %w", err)
		}
		return "", fmt.Errorf("bd command failed: %s", detail)
	}
	if stdout.overflow {
		return "", errors.New("bd output exceeds limit")
	}
	return stdout.String(), nil
}

func boundedEnvironment() []string {
	allow := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "SYSTEMROOT": true, "WINDIR": true}
	values := map[string]string{"BD_NON_INTERACTIVE": "1"}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && allow[key] {
			values[key] = value
		}
	}
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func (c *Client) Prime(ctx context.Context) string {
	out, err := c.Run(ctx, "prime")
	if err != nil {
		return ""
	}
	return bounded(out, 32<<10)
}

type Task struct {
	Title       string
	Description string
	Assignee    string
	ExternalRef string
	Labels      []string
}

func (c *Client) CreateTask(ctx context.Context, task Task) (string, error) {
	args := []string{"create", "--json", "--type=task", "--title=" + strings.TrimSpace(task.Title)}
	if strings.TrimSpace(task.Description) != "" {
		args = append(args, "--description="+task.Description)
	}
	if strings.TrimSpace(task.Assignee) != "" {
		args = append(args, "--assignee="+strings.TrimSpace(task.Assignee))
	}
	if strings.TrimSpace(task.ExternalRef) != "" {
		args = append(args, "--external-ref="+strings.TrimSpace(task.ExternalRef))
	}
	if len(task.Labels) != 0 {
		args = append(args, "--labels="+strings.Join(task.Labels, ","))
	}
	out, err := c.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return issueID(out)
}

func (c *Client) AddDependency(ctx context.Context, issue, dependency string) error {
	if !validID(issue) || !validID(dependency) {
		return errors.New("invalid Beads issue id")
	}
	_, err := c.Run(ctx, "dep", "add", issue, dependency)
	return err
}

func (c *Client) Start(ctx context.Context, id, persona string) error {
	if !validID(id) {
		return errors.New("invalid Beads issue id")
	}
	_, err := c.Run(ctx, "update", id, "--status=in_progress", "--assignee="+persona)
	return err
}

func (c *Client) Complete(ctx context.Context, id, summary string) error {
	if !validID(id) {
		return errors.New("invalid Beads issue id")
	}
	if summary = strings.TrimSpace(summary); summary != "" {
		if _, err := c.Run(ctx, "comments", "add", id, "--author=shipmates", "--", bounded(summary, 4096)); err != nil {
			return err
		}
	}
	_, err := c.Run(ctx, "close", id, "--reason=Shipmates voyage task completed")
	return err
}

func (c *Client) Block(ctx context.Context, id, reason string) error {
	if !validID(id) {
		return errors.New("invalid Beads issue id")
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		if _, err := c.Run(ctx, "comments", "add", id, "--author=shipmates", "--", bounded(reason, 2048)); err != nil {
			return err
		}
	}
	_, err := c.Run(ctx, "update", id, "--status=blocked")
	return err
}

func (c *Client) Show(ctx context.Context, id string) string {
	if !validID(id) {
		return ""
	}
	out, err := c.Run(ctx, "show", id, "--json")
	if err != nil {
		return ""
	}
	return bounded(out, 16<<10)
}

func issueID(raw string) (string, error) {
	var object struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &object); err == nil && validID(object.ID) {
		return object.ID, nil
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err == nil && len(list) == 1 && validID(list[0].ID) {
		return list[0].ID, nil
	}
	return "", errors.New("bd create returned no valid issue id")
}

func validID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '-' {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

func bounded(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	if n > remaining {
		b.overflow = true
	}
	return n, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

var _ io.Writer = (*limitedBuffer)(nil)
