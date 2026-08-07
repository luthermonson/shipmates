// Package project models the on-disk layout shipmates manages inside a user's
// repository: the .shipmates/ control dir, per-persona memory, and the manifest
// used to detect user edits during `shipmates update`.
package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/luthermonson/shipmates/internal/backend"
	personadef "github.com/luthermonson/shipmates/internal/persona"
	"gopkg.in/yaml.v3"
)

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
	// sessions, hooks, tells), "codex" (persistent headless Codex sessions),
	// or "command" (spawn Command under a PTY — for
	// foreign agents like opencode/aider). Command-backed mates are PTY-only:
	// no session resume, no hooks, no headless tells; their status dots derive
	// from screen activity instead of hook events.
	Backend string
	Command []string // argv for backend "command"
}

// BackendDescriptor returns the resolved harness and its supported surfaces.
func (c PersonaConfig) BackendDescriptor() backend.Descriptor { return backend.Resolve(c.Backend) }

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

// IsFleetPersonaFile reports whether a persona file is a shipmates fleet member
// (true unless its frontmatter sets `shipmatesPersona: false`). Lets non-fleet
// agents in .claude/agents/ (e.g. a project-Q&A subagent) opt out of membership
// walks and `routing apply --all`.
func IsFleetPersonaFile(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	def, err := personadef.Parse(raw)
	if err != nil {
		return true
	}
	fm := def.Frontmatter
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

	def, err := personadef.Parse(raw)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", AgentPath(persona), err)
	}
	fm := def.Frontmatter

	cfg.Mode = strings.TrimSpace(fm.Permissions.Mode)
	cfg.Model = strings.TrimSpace(fm.Model)
	cfg.Effort = strings.TrimSpace(fm.Effort)
	cfg.Backend = strings.TrimSpace(fm.Backend)
	cfg.Command = fm.Command
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
