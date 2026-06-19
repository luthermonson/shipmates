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
	SessionPrefix string `yaml:"sessionPrefix"`
	SharedMemory  bool   `yaml:"sharedMemory"`
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
