// Package project models the on-disk layout shipmates manages inside a user's
// repository: the .shipmates/ control dir, per-persona memory, and the manifest
// used to detect user edits during `shipmates update`.
package project

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	Dir             = ".shipmates"
	MemoryDirName   = "memory"
	SessionsDirName = "sessions"
	ManifestName    = "manifest.json"
	ConfigName      = "shipmates.yaml"
	AgentsDir       = ".claude/agents"
	CommandsDir     = ".claude/commands"
)

// MemoryDir is a persona's persistent memory directory.
func MemoryDir(persona string) string {
	return filepath.Join(Dir, MemoryDirName, persona)
}

// AgentPath is where a persona's subagent file is vendored for Claude Code.
func AgentPath(persona string) string {
	return filepath.Join(AgentsDir, persona+".md")
}

// ManifestPath is the location of the install manifest.
func ManifestPath() string {
	return filepath.Join(Dir, ManifestName)
}

// SessionsDir holds transient coordination state (server port/pid, session markers).
func SessionsDir() string { return filepath.Join(Dir, SessionsDirName) }

// PortFile / PidFile / LogFile locate the running server's metadata.
func PortFile() string { return filepath.Join(SessionsDir(), "server.port") }
func PidFile() string  { return filepath.Join(SessionsDir(), "server.pid") }
func LogFile() string  { return filepath.Join(SessionsDir(), "server.log") }

// SessionMarker records that a persona's claude session has been created, so
// `ask` knows whether to create (--session-id) or continue (--resume).
func SessionMarker(persona string) string {
	return filepath.Join(SessionsDir(), persona+".session")
}

// SessionMeta is the per-persona session record stored at SessionMarker. It
// tracks the session name and the config fingerprint at creation time, so
// callers can detect config drift and start a fresh session automatically.
type SessionMeta struct {
	Name       string `json:"name"`
	ConfigHash string `json:"config"`
}

// ReadSessionMeta loads a persona's session record. ok is false if no session
// exists yet. A legacy plain-name marker is read as a name with empty hash
// (which suppresses auto-fresh — we don't abandon a pre-upgrade session).
func ReadSessionMeta(persona string) (meta SessionMeta, ok bool) {
	b, err := os.ReadFile(SessionMarker(persona))
	if err != nil {
		return SessionMeta{}, false
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return SessionMeta{Name: strings.TrimSpace(string(b))}, true
	}
	return meta, true
}

// WriteSessionMeta records a persona's session name and config fingerprint.
func WriteSessionMeta(persona, name, configHash string) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(SessionMeta{Name: name, ConfigHash: configHash}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SessionMarker(persona), b, 0o644)
}

// RepoName is the current project's directory name — the default session prefix.
func RepoName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "shipmates"
	}
	return filepath.Base(wd)
}

// Config is the subset of shipmates.yaml that the CLI reads.
type Config struct {
	// SessionPrefix namespaces per-persona session names. `shipmates init`
	// writes the repo name here; an empty value means "no prefix" (session
	// names are just the persona name). Configurable so two checkouts of the
	// same repo (or same-named projects) don't collide on session handles.
	SessionPrefix string                  `yaml:"sessionPrefix"`
	SharedMemory  bool                    `yaml:"sharedMemory"`
	Routing       string                  `yaml:"routing"`
	Crew          map[string]CrewOverride `yaml:"crew"`
}

// CrewOverride is a crew-level override of a persona's frontmatter config, keyed
// by persona name under shipmates.yaml's `crew:` map. A field only overrides
// when it's set: a non-empty Mode, a present RemoteControl node, or a non-nil
// DangerouslySkipPermissions wins over the persona's own frontmatter.
type CrewOverride struct {
	Permissions struct {
		Mode string `yaml:"mode"`
	} `yaml:"permissions"`
	RemoteControl              yaml.Node `yaml:"remoteControl"`
	DangerouslySkipPermissions *bool     `yaml:"dangerouslySkipPermissions"`
	Model                      string    `yaml:"model"`
	Effort                     string    `yaml:"effort"`
}

// LoadConfig reads shipmates.yaml, returning a zero Config if it's absent.
func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigName)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigName, err)
	}
	return &c, nil
}

// SessionPrefix is the configured prefix verbatim. Empty means no prefix. The
// repo-name default is applied once, at `init` time, by writing it into the
// config — not re-derived here.
func SessionPrefix() string {
	c, err := LoadConfig()
	if err != nil {
		return ""
	}
	return c.SessionPrefix
}

// SessionName is the stable --name / --resume handle for a persona's session.
// With no configured prefix it's just the persona name.
func SessionName(persona string) string {
	if p := SessionPrefix(); p != "" {
		return p + "-" + persona
	}
	return persona
}

// PersonaConfig is the fully-resolved launch config for a persona: its
// frontmatter overlaid with any crew-level override from shipmates.yaml.
type PersonaConfig struct {
	Mode                       string // ask|acceptEdits|bypassPermissions|plan|""
	RemoteControl              string // resolved --remote-control value; "" = off
	DangerouslySkipPermissions bool
	Model                      string // --model value; "" = claude's configured default
	Effort                     string // --effort value (low|medium|high|xhigh|max); "" = default
}

// Fingerprint is a stable hash of the config settings that are baked into a
// session at creation and can't change on resume — currently model and effort.
// Permission mode, dangerouslySkipPermissions, and remoteControl are deliberately
// excluded: they're passed as flags on every invocation (create AND resume), so
// changing them applies immediately without abandoning the session. Callers
// compare this against the value stored at creation to auto-fresh on drift.
func (c PersonaConfig) Fingerprint() string {
	return SHA([]byte(fmt.Sprintf("model=%s|effort=%s", c.Model, c.Effort)))
}

// LaunchFlags returns the claude CLI flags derived from this config — the single
// source of truth so every spawn site (ask/tell/open/fanout/live server) stays
// consistent. permission controls whether the permission knobs are included:
// pass true for direct one-shot/interactive spawns, false for the live-server
// path (which mediates permission via its PreToolUse gate instead). remoteControl
// (--remote-control) is interactive-only and handled by the caller (open).
func (c PersonaConfig) LaunchFlags(permission bool) []string {
	var f []string
	if permission {
		if c.DangerouslySkipPermissions {
			f = append(f, "--dangerously-skip-permissions")
		}
		if c.Mode != "" {
			f = append(f, "--permission-mode", c.Mode)
		}
	}
	if c.Model != "" {
		f = append(f, "--model", c.Model)
	}
	if c.Effort != "" {
		f = append(f, "--effort", c.Effort)
	}
	return f
}

// personaFrontmatter is the subset of a persona's YAML frontmatter that affects
// how its session is launched. RemoteControl may be a bool or a string, so it's
// captured as a yaml.Node and decoded on demand.
type personaFrontmatter struct {
	Permissions struct {
		Mode string `yaml:"mode"`
	} `yaml:"permissions"`
	RemoteControl              yaml.Node `yaml:"remoteControl"`
	DangerouslySkipPermissions *bool     `yaml:"dangerouslySkipPermissions"`
	Model                      string    `yaml:"model"`
	Effort                     string    `yaml:"effort"`
}

// ResolvePersonaConfig reads the installed persona's frontmatter, overlays any
// crew-level override from shipmates.yaml, and resolves the result. A missing
// persona file yields a zero PersonaConfig and nil error.
func ResolvePersonaConfig(persona string) (PersonaConfig, error) {
	var cfg PersonaConfig

	raw, err := os.ReadFile(AgentPath(persona))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}

	fm, err := parsePersonaFrontmatter(raw)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", AgentPath(persona), err)
	}

	cfg.Mode = strings.TrimSpace(fm.Permissions.Mode)
	cfg.Model = strings.TrimSpace(fm.Model)
	cfg.Effort = strings.TrimSpace(fm.Effort)
	rcNode := fm.RemoteControl
	if fm.DangerouslySkipPermissions != nil {
		cfg.DangerouslySkipPermissions = *fm.DangerouslySkipPermissions
	}

	conf, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	if ov, ok := conf.Crew[persona]; ok {
		if m := strings.TrimSpace(ov.Permissions.Mode); m != "" {
			cfg.Mode = m
		}
		if m := strings.TrimSpace(ov.Model); m != "" {
			cfg.Model = m
		}
		if e := strings.TrimSpace(ov.Effort); e != "" {
			cfg.Effort = e
		}
		if ov.RemoteControl.Kind != 0 {
			rcNode = ov.RemoteControl
		}
		if ov.DangerouslySkipPermissions != nil {
			cfg.DangerouslySkipPermissions = *ov.DangerouslySkipPermissions
		}
	}

	cfg.RemoteControl = resolveRemoteControl(rcNode, SessionName(persona))
	return cfg, nil
}

// parsePersonaFrontmatter isolates the YAML frontmatter block and unmarshals the
// launch-relevant fields. (render.go's parseFrontmatter drops nested maps like
// permissions, so it can't supply these.)
func parsePersonaFrontmatter(raw []byte) (personaFrontmatter, error) {
	var fm personaFrontmatter
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")
	if !strings.HasPrefix(text, "---\n") {
		return fm, nil
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, nil
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, err
	}
	return fm, nil
}

// resolveRemoteControl turns a remoteControl node into a --remote-control value:
// bool true => sessionName; a non-empty string => that string; false/absent => "".
func resolveRemoteControl(node yaml.Node, sessionName string) string {
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	var b bool
	if err := node.Decode(&b); err == nil {
		if b {
			return sessionName
		}
		return ""
	}
	var s string
	if err := node.Decode(&s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// NewUUID returns a random v4 UUID string using only the standard library.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Manifest records the SHA of each file shipmates has written, so `update` can
// tell an untouched file (safe to overwrite) from a user-edited one (prompt).
type Manifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

// LoadManifest reads the manifest, returning an empty one if none exists yet.
func LoadManifest() (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Files: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

// Save writes the manifest back to disk.
func (m *Manifest) Save() error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ManifestPath(), b, 0o644)
}

// SHA returns the hex-encoded sha256 of b, used for manifest comparisons.
func SHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
