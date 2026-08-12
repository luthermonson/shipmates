package tracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/project"
	"gopkg.in/yaml.v3"
)

// MarkdownDirName is the project-relative directory holding one markdown file
// per tracked voyage task.
const MarkdownDirName = ".shipmates/voyage"

// maxMarkdownTaskBytes bounds a task file. A hand-edited file may grow, but a
// runaway one stops being a task record.
const maxMarkdownTaskBytes = 256 << 10

// Markdown tracks voyage tasks as plain files under .shipmates/voyage/, one
// file per task: a small YAML frontmatter block (id, status, dependencies)
// over a human-readable body with an append-only log. Writes are atomic
// (temp file + project.DurableRename) so a mid-run crash leaves either the
// previous record or the new one, never a torn file.
type Markdown struct {
	root string
}

// NewMarkdown returns a markdown tracker rooted at the project root.
func NewMarkdown(root string) *Markdown {
	return &Markdown{root: root}
}

func (m *Markdown) Name() string { return "markdown" }

func (m *Markdown) dir() string { return filepath.Join(m.root, filepath.FromSlash(MarkdownDirName)) }

func (m *Markdown) path(id string) string { return filepath.Join(m.dir(), id+".md") }

// taskMeta is the frontmatter schema. Unknown keys a human adds are
// preserved-by-reparse tolerant: decoding ignores them, and rewrites preserve
// only the known fields (the body is preserved verbatim).
type taskMeta struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Status      string   `yaml:"status"`
	Assignee    string   `yaml:"assignee,omitempty"`
	ExternalRef string   `yaml:"external_ref,omitempty"`
	DependsOn   []string `yaml:"depends_on,omitempty"`
	Labels      []string `yaml:"labels,omitempty"`
	Created     string   `yaml:"created,omitempty"`
	Updated     string   `yaml:"updated,omitempty"`
	CloseReason string   `yaml:"close_reason,omitempty"`
}

var markdownStatuses = map[string]bool{"open": true, "in_progress": true, "closed": true, "blocked": true}

// taskID derives a stable, filesystem- and argv-safe id. A sail ExternalRef
// ("shipmates:voyage:<hash16>:<task>") maps to "<hash16>-<task>", so the file
// name in a PR diff says which plan and task it belongs to. Without an
// external ref the id is a content digest prefix.
func taskID(t Task) string {
	ref := strings.TrimSpace(t.ExternalRef)
	if ref != "" {
		trimmed := ref
		for _, prefix := range []string{"shipmates:voyage:", "shipmates:derivative:"} {
			if strings.HasPrefix(trimmed, prefix) {
				trimmed = strings.TrimPrefix(trimmed, prefix)
				break
			}
		}
		id := sanitizeID(strings.ReplaceAll(trimmed, ":", "-"))
		if id != "" {
			return id
		}
	}
	sum := sha256.Sum256([]byte(t.Title + "\x00" + t.Description + "\x00" + t.Assignee))
	return "vt-" + hex.EncodeToString(sum[:6])
}

func sanitizeID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-.")
	if len(id) > 100 {
		id = id[:100]
	}
	return id
}

func validMarkdownID(id string) bool {
	if id == "" || len(id) > 128 || id[0] == '-' || id[0] == '.' {
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

func (m *Markdown) CreateTask(ctx context.Context, t Task) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(t.Title) == "" {
		return "", errors.New("markdown tracker: task title is required")
	}
	id := taskID(t)
	if !validMarkdownID(id) {
		return "", fmt.Errorf("markdown tracker: cannot derive a valid task id from %q", t.ExternalRef)
	}
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return "", err
	}
	path := m.path(id)
	if _, err := os.Lstat(path); err == nil {
		// Sail may crash between creating the record and persisting the id in
		// voyage state; recreating the same task must be idempotent.
		meta, _, loadErr := m.load(id)
		if loadErr != nil {
			return "", loadErr
		}
		if meta.ExternalRef != strings.TrimSpace(t.ExternalRef) {
			return "", fmt.Errorf("markdown tracker: %s already exists for a different task (external_ref %q)", path, meta.ExternalRef)
		}
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	meta := taskMeta{ID: id, Title: strings.TrimSpace(t.Title), Status: "open", Assignee: strings.TrimSpace(t.Assignee), ExternalRef: strings.TrimSpace(t.ExternalRef), Labels: append([]string(nil), t.Labels...), Created: now, Updated: now}
	body := "# " + meta.Title + "\n"
	if strings.TrimSpace(t.Description) != "" {
		body += "\n" + strings.TrimSpace(t.Description) + "\n"
	}
	body += "\n## Log\n"
	if err := m.write(id, meta, body); err != nil {
		return "", err
	}
	return id, nil
}

func (m *Markdown) AddDependency(ctx context.Context, id, dependsOn string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validMarkdownID(id) || !validMarkdownID(dependsOn) {
		return errors.New("markdown tracker: invalid task id")
	}
	if _, _, err := m.load(dependsOn); err != nil {
		return fmt.Errorf("markdown tracker: dependency %q does not resolve: %w", dependsOn, err)
	}
	meta, body, err := m.load(id)
	if err != nil {
		return err
	}
	for _, existing := range meta.DependsOn {
		if existing == dependsOn {
			return nil
		}
	}
	meta.DependsOn = append(meta.DependsOn, dependsOn)
	sort.Strings(meta.DependsOn)
	return m.update(id, meta, body, "")
}

func (m *Markdown) Start(ctx context.Context, id, persona string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta, body, err := m.load(id)
	if err != nil {
		return err
	}
	meta.Status = "in_progress"
	if strings.TrimSpace(persona) != "" {
		meta.Assignee = strings.TrimSpace(persona)
	}
	return m.update(id, meta, body, "started by "+strings.TrimSpace(persona))
}

func (m *Markdown) Complete(ctx context.Context, id, summary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta, body, err := m.load(id)
	if err != nil {
		return err
	}
	meta.Status = "closed"
	meta.CloseReason = "Shipmates voyage task completed"
	note := ""
	if summary = strings.TrimSpace(summary); summary != "" {
		note = "completed: " + bounded(summary, 4096)
	} else {
		note = "completed"
	}
	return m.update(id, meta, body, note)
}

func (m *Markdown) Block(ctx context.Context, id, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	meta, body, err := m.load(id)
	if err != nil {
		return err
	}
	meta.Status = "blocked"
	note := "blocked"
	if reason = strings.TrimSpace(reason); reason != "" {
		note = "blocked: " + bounded(reason, 2048)
	}
	return m.update(id, meta, body, note)
}

func (m *Markdown) Show(ctx context.Context, id string) string {
	if ctx.Err() != nil || !validMarkdownID(id) {
		return ""
	}
	raw, err := os.ReadFile(m.path(id))
	if err != nil || len(raw) > maxMarkdownTaskBytes {
		return ""
	}
	return bounded(string(raw), 16<<10)
}

func (m *Markdown) Prime(ctx context.Context) string {
	return "Voyage tasks are tracked as markdown files under " + MarkdownDirName + "/ (one file per task: YAML frontmatter for id/status/dependencies, then a description and an append-only Log section)."
}

func (m *Markdown) TaskGuidance(id string) string {
	return fmt.Sprintf("Task record: %s/%s.md\nRead it for authoritative task context and append concise durable findings as bullet items under its \"## Log\" heading. Shipmates synchronizes the frontmatter status field for you; do not change frontmatter and do not create a duplicate task file.", MarkdownDirName, id)
}

// load parses one task file into frontmatter and body. Garbage gets an error
// naming the file; a hand-edited but well-formed file parses and keeps going.
func (m *Markdown) load(id string) (taskMeta, string, error) {
	if !validMarkdownID(id) {
		return taskMeta{}, "", errors.New("markdown tracker: invalid task id")
	}
	path := m.path(id)
	info, err := os.Lstat(path)
	if err != nil {
		return taskMeta{}, "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxMarkdownTaskBytes {
		return taskMeta{}, "", fmt.Errorf("markdown tracker: %s must be a bounded regular file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return taskMeta{}, "", err
	}
	front, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return taskMeta{}, "", fmt.Errorf("markdown tracker: %s: %w", path, err)
	}
	var meta taskMeta
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		return taskMeta{}, "", fmt.Errorf("markdown tracker: %s has unreadable frontmatter: %w", path, err)
	}
	if meta.ID == "" {
		meta.ID = id
	}
	if meta.ID != id {
		return taskMeta{}, "", fmt.Errorf("markdown tracker: %s frontmatter id %q does not match its file name", path, meta.ID)
	}
	if !markdownStatuses[meta.Status] {
		return taskMeta{}, "", fmt.Errorf("markdown tracker: %s has unknown status %q (want open, in_progress, closed, or blocked)", path, meta.Status)
	}
	return meta, body, nil
}

func splitFrontmatter(raw string) (string, string, error) {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", "", errors.New("missing frontmatter (file must start with ---)")
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return "", "", errors.New("unterminated frontmatter (missing closing ---)")
	}
	return rest[:end+1], rest[end+len("\n---\n"):], nil
}

func (m *Markdown) update(id string, meta taskMeta, body, logNote string) error {
	meta.Updated = time.Now().UTC().Format(time.RFC3339)
	if logNote != "" {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if !strings.Contains(body, "## Log") {
			body += "\n## Log\n"
		}
		body += "- " + meta.Updated + " " + sanitizeLogNote(logNote) + "\n"
	}
	return m.write(id, meta, body)
}

// sanitizeLogNote keeps an appended note on one line so it cannot forge a
// frontmatter block or a fake log structure inside the file.
func sanitizeLogNote(note string) string {
	note = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, note)
	return strings.TrimSpace(note)
}

func (m *Markdown) write(id string, meta taskMeta, body string) error {
	front, err := yaml.Marshal(meta)
	if err != nil {
		return err
	}
	content := "---\n" + string(front) + "---\n" + body
	if len(content) > maxMarkdownTaskBytes {
		return fmt.Errorf("markdown tracker: task %s exceeds the %d byte bound", id, maxMarkdownTaskBytes)
	}
	dir := m.dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".task-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return project.DurableRename(name, m.path(id))
}

func bounded(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
