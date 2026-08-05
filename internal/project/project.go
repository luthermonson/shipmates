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
	Dir              = ".shipmates"
	MemoryDirName    = "memory"
	SessionsDirName  = "sessions"
	PoliciesDirName  = "policies"
	ManifestName     = "manifest.json"
	ConfigName       = "shipmates.yaml"
	InstallIDName    = "install-id"
	AgentsDir        = ".claude/agents"
	CommandsDir      = ".claude/commands"
)

// MemoryDir is a persona's persistent memory directory.
func MemoryDir(persona string) string {
	return filepath.Join(Dir, MemoryDirName, persona)
}

// AgentPath is where a persona's subagent file is vendored for Claude Code.
func AgentPath(persona string) string {
	return filepath.Join(AgentsDir, persona+".md")
}

// CommandPath is where a slash command is vendored for Claude Code.
func CommandPath(name string) string {
	return filepath.Join(CommandsDir, name+".md")
}

// PoliciesDir is where per-persona policy overlays are vendored on install.
// Kept under .shipmates/ (not .claude/) because they're shipmates-owned rules
// consumed by the shipmates permission evaluator, not by Claude Code itself.
func PoliciesDir() string {
	return filepath.Join(Dir, PoliciesDirName)
}

// PolicyPath is the vendored location of a persona's policy.yaml overlay.
// See PoliciesDir for why it lives under .shipmates/.
func PolicyPath(persona string) string {
	return filepath.Join(PoliciesDir(), persona+".yaml")
}

// ArticlesPath is the vendored location of the Ship's Articles document
// (the brig's canonical rules — see internal/brig and docs/brig.md).
func ArticlesPath() string {
	return filepath.Join(Dir, "ARTICLES.md")
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
//
// CWD is the child claude's cmd.Dir at session-creation time — the berth
// guardrail. Empty means "no override" (spawn at the shipmates process cwd,
// today's behavior). Sessions created before berthing existed have no CWD
// field on disk; ReadSessionMeta preserves that as an empty string so their
// resume path stays unchanged — the "berth only at session creation" rule.
type SessionMeta struct {
	Name       string `json:"name"`
	ID         string `json:"id"` // the session UUID — resume by this to avoid --resume name ambiguity
	ConfigHash string `json:"config"`
	CWD        string `json:"cwd,omitempty"`
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

// WriteSessionMeta records a persona's session name, UUID, and config
// fingerprint. cwd is stored so subsequent resumes land in the same directory
// the session was created in — the "berth only at creation" guardrail. Pass
// "" for cwd to preserve today's repo-root behavior.
func WriteSessionMeta(persona, name, id, configHash, cwd string) error {
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(SessionMeta{Name: name, ID: id, ConfigHash: configHash, CWD: cwd}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(SessionMarker(persona), b, 0o644)
}

// DeleteSessionMeta removes a persona's session marker. Called by auto-repair
// when the tracked session UUID has vanished from Claude's local store
// (rotated jsonl, cache clean, upgrade wipe). A missing marker is not an
// error — the next SessionLaunch will start fresh regardless.
func DeleteSessionMeta(persona string) error {
	err := os.Remove(SessionMarker(persona))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// RepoName is the current project's directory name — the default session prefix.
func RepoName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "shipmates"
	}
	return filepath.Base(wd)
}

// InstallIDPath is the on-disk location of the per-install stable ID.
func InstallIDPath() string { return filepath.Join(Dir, InstallIDName) }

// InstallID returns the stable per-install UUID, generating and persisting one
// on first read. Used to disambiguate multi-clone of the same repo connecting
// to the same fleet (clientKey = <repo>/<install-id>/<persona>).
func InstallID() (string, error) {
	b, err := os.ReadFile(InstallIDPath())
	if err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	id := NewUUID()
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(InstallIDPath(), []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

// Config is the subset of shipmates.yaml that the CLI reads.
type Config struct {
	// SessionPrefix namespaces per-persona session names. `shipmates init`
	// writes the repo name here; an empty value means "no prefix" (session
	// names are just the persona name). Configurable so two checkouts of the
	// same repo (or same-named projects) don't collide on session handles.
	SessionPrefix string `yaml:"sessionPrefix"`
	// CaptainPersona names the coordinating persona on this ship — the front
	// door the fleet opens and the identity in the tunnel clientKey.
	// Defaults to "captain"; crews with themed coordinators set their own
	// (e.g. picard, coach).
	CaptainPersona string                  `yaml:"captainPersona"`
	SharedMemory   bool                    `yaml:"sharedMemory"`
	Routing        string                  `yaml:"routing"`
	RoutingOptions RoutingOptions          `yaml:"routingOptions"`
	RoutingOnBoot  bool                    `yaml:"routingOnBoot"`
	Fleet          FleetConfig             `yaml:"fleet"`
	Crew           map[string]CrewOverride `yaml:"crew"`
}

// FleetConfig points the captain at a central `shipmates fleet serve` instance.
// When URL is non-empty the captain opens an outbound websocket on boot using
// rancher/remotedialer; the fleet can then dial back through the tunnel to the
// captain's existing 127.0.0.1 server.
//
// The secret is NEVER stored in shipmates.yaml (it gets committed to git).
// TokenEnv names an environment variable the captain reads at boot to get the
// bearer token; default is SHIPMATES_FLEET_TOKEN. Set this var in your shell
// (or systemd unit, launchd plist, etc.) — the config file stays clean.
//
// Name overrides the captain's identity on the fleet. Defaults to the repo
// directory name, so the clientKey is `<repo>:<persona>`. Set this if two
// clones of the same repo connect to the same fleet and collide (e.g.
// "card-cannon-dev" vs "card-cannon-scratch").
type FleetConfig struct {
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"tokenEnv"`
	Name     string `yaml:"name"`
}

// DefaultFleetTokenEnv is the env var the captain reads when FleetConfig.TokenEnv
// is empty. Matches the var the fleet server and operator commands also read.
const DefaultFleetTokenEnv = "SHIPMATES_FLEET_TOKEN"

// Token returns the resolved bearer token, read from the env var named by
// TokenEnv (or DefaultFleetTokenEnv when unset). Empty result means "no auth".
func (b FleetConfig) Token() string {
	name := strings.TrimSpace(b.TokenEnv)
	if name == "" {
		name = DefaultFleetTokenEnv
	}
	return strings.TrimSpace(os.Getenv(name))
}

// RoutingOptions toggles parts of the routing block that are private-fleet
// conventions rather than universal: byline intros on GitHub messages, and
// persona-name labels as a work queue. Both default ON (the fleet case); set
// them false for open-source contribution where neither applies. Pointers so
// "absent" is distinguishable from explicit false.
type RoutingOptions struct {
	Bylines *bool `yaml:"bylines"`
	Labels  *bool `yaml:"labels"`
}

// Resolved returns the effective flags, defaulting absent (nil) to true.
func (o RoutingOptions) Resolved() (bylines, labels bool) {
	bylines = o.Bylines == nil || *o.Bylines
	labels = o.Labels == nil || *o.Labels
	return
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
	Backend                    string    `yaml:"backend"`
	Command                    []string  `yaml:"command"`
	Berth                      string    `yaml:"berth"`
	CWD                        string    `yaml:"cwd"`
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

	// Backend selects the mate driver: "claude" (default, full integration:
	// sessions, hooks, tells) or "command" (spawn Command under a PTY — for
	// foreign agents like opencode/aider). Command-backed mates are PTY-only:
	// no session resume, no hooks, no headless tells; their status dots derive
	// from screen activity instead of hook events.
	Backend string
	Command []string // argv for backend "command"

	// Berth is the persona's berth policy: "off" (run at repo root, today's
	// behavior — fleet default), "auto" (create the worktree from origin/main
	// if missing, use it — captain default), or "require" (error if absent).
	// Resolved from frontmatter or crew override; see internal/berth.
	Berth string

	// CWD is an explicit cwd override for the persona (frontmatter/crew field).
	// Empty means "no override" — the caller falls back to the berth path (if
	// berth is on) or the repo root. Never enters Fingerprint(): changing cwd
	// must NOT auto-fresh the session it means to preserve.
	CWD string
}

// CommandBacked reports whether the persona runs a foreign agent (PTY-only).
func (c PersonaConfig) CommandBacked() bool { return c.Backend == "command" }

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
	Backend                    string    `yaml:"backend"`
	Command                    []string  `yaml:"command"`
	Berth                      string    `yaml:"berth"`
	CWD                        string    `yaml:"cwd"`
	ShipmatesPersona           *bool     `yaml:"shipmatesPersona"`
}

// IsFleetPersonaFile reports whether a persona file is a shipmates fleet member
// (true unless its frontmatter sets `shipmatesPersona: false`). Lets non-fleet
// agents in .claude/agents/ (e.g. a project-Q&A subagent) opt out of membership
// walks and `routing apply --all`.
func IsFleetPersonaFile(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fm, err := parsePersonaFrontmatter(raw)
	if err != nil {
		return true
	}
	return fm.ShipmatesPersona == nil || *fm.ShipmatesPersona
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
	cfg.Backend = strings.TrimSpace(fm.Backend)
	cfg.Command = fm.Command
	cfg.Berth = strings.TrimSpace(fm.Berth)
	cfg.CWD = strings.TrimSpace(fm.CWD)
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
		if b := strings.TrimSpace(ov.Backend); b != "" {
			cfg.Backend = b
		}
		if len(ov.Command) > 0 {
			cfg.Command = ov.Command
		}
		if b := strings.TrimSpace(ov.Berth); b != "" {
			cfg.Berth = b
		}
		if c := strings.TrimSpace(ov.CWD); c != "" {
			cfg.CWD = c
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
