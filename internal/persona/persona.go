// Package persona owns the canonical Shipmates persona document format.
package persona

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter is the complete typed subset of persona metadata consumed by
// Shipmates. Renderers and runtime launch configuration use the same parser.
type Frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Byline      string   `yaml:"byline"`
	DomainGlob  []string `yaml:"domainGlob"`

	Permissions struct {
		Mode string `yaml:"mode"`
	} `yaml:"permissions"`
	RemoteControl              yaml.Node `yaml:"remoteControl"`
	DangerouslySkipPermissions *bool     `yaml:"dangerouslySkipPermissions"`
	Model                      string    `yaml:"model"`
	Effort                     string    `yaml:"effort"`
	Backend                    string    `yaml:"backend"`
	Command                    []string  `yaml:"command"`
	ShipmatesPersona           *bool     `yaml:"shipmatesPersona"`
}

// Definition is a parsed persona document.
type Definition struct {
	Frontmatter Frontmatter
	Body        string
}

// Parse separates YAML frontmatter from the persona instructions. Documents
// without frontmatter are valid and return the entire input as Body.
func Parse(raw []byte) (Definition, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Definition{Body: strings.TrimSpace(text)}, nil
	}

	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			end = len(rest) - len("\n---")
		} else {
			return Definition{Body: strings.TrimSpace(text)}, nil
		}
	}

	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return Definition{}, err
	}
	bodyStart := end + len("\n---")
	if bodyStart < len(rest) && rest[bodyStart] == '\n' {
		bodyStart++
	}
	return Definition{Frontmatter: fm, Body: strings.TrimSpace(rest[bodyStart:])}, nil
}
