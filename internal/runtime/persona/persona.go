// Package persona reads shipmates canonical persona files from the catalog
// and lets each runtime translate them to its own on-disk format.
//
// The canonical shape is what's already in catalog/<persona>/agent.md
// (from the codex-adaptation branch): YAML frontmatter with shipmates-side
// fields (name, description, byline, domainGlob, memoryDir, permissions,
// remoteControl) followed by a Markdown body. Runtime translators strip
// the shipmates-only fields, keep the Markdown body verbatim, and emit
// runtime-native frontmatter.
package persona

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Canonical is the parsed shape of catalog/<persona>/agent.md. Unknown
// frontmatter keys are preserved in Extra so a future runtime can consume
// them without a schema bump.
type Canonical struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	Byline        string            `yaml:"byline,omitempty"`
	DomainGlob    []string          `yaml:"domainGlob,omitempty"`
	MemoryDir     string            `yaml:"memoryDir,omitempty"`
	Permissions   map[string]any    `yaml:"permissions,omitempty"`
	RemoteControl bool              `yaml:"remoteControl,omitempty"`
	Model         string            `yaml:"model,omitempty"`
	Tools         []string          `yaml:"tools,omitempty"`
	Berth         string            `yaml:"berth,omitempty"`
	Extra         map[string]any    `yaml:",inline"`

	// Body is the Markdown body following the frontmatter, verbatim.
	Body string `yaml:"-"`
}

// Load parses a canonical persona file from disk.
func Load(path string) (*Canonical, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads a canonical persona from any io.Reader.
func Parse(r io.Reader) (*Canonical, error) {
	all, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	frontmatter, body, err := splitFrontmatter(all)
	if err != nil {
		return nil, fmt.Errorf("persona: %w", err)
	}
	var c Canonical
	if len(frontmatter) > 0 {
		if err := yaml.Unmarshal(frontmatter, &c); err != nil {
			return nil, fmt.Errorf("persona: parse frontmatter: %w", err)
		}
	}
	c.Body = string(body)
	if strings.TrimSpace(c.Name) == "" {
		return nil, errors.New("persona: name is required")
	}
	return &c, nil
}

// splitFrontmatter separates the leading `---\n...\n---\n` block from the
// rest. Returns (nil, all, nil) when no frontmatter is present.
func splitFrontmatter(input []byte) (front, body []byte, err error) {
	const sep = "---\n"
	if !bytes.HasPrefix(input, []byte(sep)) {
		// Also handle `---\r\n` line endings.
		if !bytes.HasPrefix(input, []byte("---\r\n")) {
			return nil, input, nil
		}
	}
	// Find the end of frontmatter.
	after := input[len(sep):]
	if bytes.HasPrefix(input, []byte("---\r\n")) {
		after = input[len("---\r\n"):]
	}
	endMarker := []byte("\n---\n")
	idx := bytes.Index(after, endMarker)
	if idx < 0 {
		endMarker = []byte("\n---\r\n")
		idx = bytes.Index(after, endMarker)
		if idx < 0 {
			return nil, nil, errors.New("frontmatter start without matching end")
		}
	}
	return after[:idx], after[idx+len(endMarker):], nil
}

// Write reserializes a Canonical to the on-disk shape (frontmatter + body).
// Round-trips are not guaranteed to be byte-identical: yaml.v3 canonicalizes
// key order and quoting. But Load(Write(c)) yields an equivalent Canonical.
func (c *Canonical) Write(w io.Writer) error {
	// Marshal a shallow struct so Extra doesn't get double-serialized.
	type header struct {
		Name          string         `yaml:"name"`
		Description   string         `yaml:"description,omitempty"`
		Byline        string         `yaml:"byline,omitempty"`
		DomainGlob    []string       `yaml:"domainGlob,omitempty"`
		MemoryDir     string         `yaml:"memoryDir,omitempty"`
		Permissions   map[string]any `yaml:"permissions,omitempty"`
		RemoteControl bool           `yaml:"remoteControl,omitempty"`
		Model         string         `yaml:"model,omitempty"`
		Tools         []string       `yaml:"tools,omitempty"`
		Berth         string         `yaml:"berth,omitempty"`
		Extra         map[string]any `yaml:",inline"`
	}
	h := header{
		Name: c.Name, Description: c.Description, Byline: c.Byline,
		DomainGlob: c.DomainGlob, MemoryDir: c.MemoryDir, Permissions: c.Permissions,
		RemoteControl: c.RemoteControl, Model: c.Model, Tools: c.Tools,
		Berth: c.Berth, Extra: c.Extra,
	}
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(h); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	_, err := io.WriteString(w, c.Body)
	return err
}
