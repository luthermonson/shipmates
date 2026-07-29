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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/luthermonson/shipmates/internal/recovery"
	"gopkg.in/yaml.v3"
)

var personaNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ValidatePersonaName rejects names that could escape or alias persona-scoped paths.
func ValidatePersonaName(persona string) error {
	if !personaNameRE.MatchString(persona) {
		return fmt.Errorf("invalid persona %q (use lowercase letters, digits, hyphens, or underscores)", persona)
	}
	return nil
}

// InstalledPersonaPath returns the canonical, Codex-native persona artifact.
func InstalledPersonaPath(persona string) (string, error) {
	return InstalledPersonaPathAt(".", persona)
}

func InstalledPersonaPathAt(root, persona string) (string, error) {
	if _, err := CanonicalPersonaAt(root, persona); err != nil {
		return "", err
	}
	return filepath.Join(root, CodexAgentPath(persona)), nil
}

// CodexPersonaInstructions validates and returns the canonical persona's
// developer instructions. Add renders this field as a quoted TOML string.
func CodexPersonaInstructions(persona string) (string, error) {
	return CodexPersonaInstructionsAt(".", persona)
}

func CodexPersonaInstructionsAt(root, persona string) (string, error) {
	item, err := CanonicalPersonaAt(root, persona)
	if err != nil {
		return "", err
	}
	return item.Doc.Instructions, nil
}

const (
	ManifestVersion = "2"
	Dir             = ".shipmates"
	MemoryDirName   = "memory"
	SessionsDirName = "sessions"
	PoliciesDirName = "policies"
	ManifestName    = "manifest.json"
	ConfigName      = "shipmates.yaml"
	InstallIDName   = "install-id"
	CodexAgentsDir  = ".codex/agents"
)

// MemoryDir is a persona's persistent memory directory.
func MemoryDir(persona string) string {
	return filepath.Join(Dir, MemoryDirName, persona)
}

// CodexAgentPath is the project-scoped Codex custom-agent configuration.
// It is also shipmates' canonical persona inventory: `list`, `remove` and
// `update` read it to decide what is installed, on every runtime.
func CodexAgentPath(persona string) string {
	return filepath.Join(CodexAgentsDir, persona+".toml")
}

// PoliciesDir is where per-persona policy overlays are vendored on install.
// Policies are Shipmates-owned rules consumed by the Codex-native evaluator.
func PoliciesDir() string {
	return filepath.Join(Dir, PoliciesDirName)
}

// PolicyPath is the vendored location of a persona's policy.yaml overlay.
// See PoliciesDir for why it lives under .shipmates/.
func PolicyPath(persona string) string {
	return filepath.Join(PoliciesDir(), persona+".yaml")
}

// ManifestPath is the location of the install manifest.
func ManifestPath() string {
	return filepath.Join(Dir, ManifestName)
}

// SessionsDir holds transient coordination state (server discovery record, session markers).
func SessionsDir() string { return filepath.Join(Dir, SessionsDirName) }

// ServerRecordFile / LogFile locate the running server's metadata.
func LogFile() string          { return filepath.Join(SessionsDir(), "server.log") }
func ServerRecordFile() string { return filepath.Join(SessionsDir(), "server.json") }

// DelegationJournalPathAt returns the isolated M1 journal path for one exact
// voyage plan hash. It performs no filesystem writes and rejects path aliases
// or non-digest plan identifiers before constructing the path.
func DelegationJournalPathAt(root, planHash string) (string, error) {
	if len(planHash) != 64 {
		return "", errors.New("invalid delegation plan hash")
	}
	if _, err := hex.DecodeString(planHash); err != nil {
		return "", errors.New("invalid delegation plan hash")
	}
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(canonical, Dir, "delegations", planHash+".jsonl"), nil
}

// CanonicalRoot returns the single filesystem identity used for project-scoped
// coordination metadata and request authentication.
func CanonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// FindRoot locates the nearest initialized Shipmates project at or above start.
// Commands that operate on a whole project use this to avoid creating a second
// coordination scope when invoked from a repository subdirectory.
func FindRoot(start string) (string, error) {
	root, err := CanonicalRoot(start)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Stat(filepath.Join(root, ConfigName))
		if statErr == nil && !info.IsDir() {
			return root, nil
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", errors.New("shipmates project not found; run shipmates init")
		}
		root = parent
	}
}

// ScopeID returns a stable, non-secret identifier for a project root. It is
// used only to prevent a loopback coordination server discovered through a
// stale port file from accepting requests belonging to another checkout.
func ScopeID(root string) (string, error) {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return "", err
	}
	return SHA([]byte(canonical)), nil
}

// ProcessAlive checks the PID recorded in project-scoped server discovery.
func ProcessAlive(pid int) (bool, error) { return dispatchProcessAlive(pid) }

// BackendSessionMarker isolates Codex session records by harness.
func BackendSessionMarker(persona, backend string) string {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return filepath.Join(SessionsDir(), persona+".session")
	}
	return filepath.Join(SessionsDir(), persona+"."+backend+".session")
}

// SessionMeta is the per-persona session record stored at SessionMarker. It
// tracks the session name and the config fingerprint at creation time, so
// callers can detect config drift and start a fresh session automatically.
type SessionMeta struct {
	Name       string `json:"name"`
	ID         string `json:"id"` // the session UUID — resume by this to avoid --resume name ambiguity
	ConfigHash string `json:"config"`
}

// ReadBackendSessionMeta loads a backend-specific session record.
func ReadBackendSessionMeta(persona, backend string) (meta SessionMeta, ok bool) {
	b, err := os.ReadFile(BackendSessionMarker(persona, backend))
	if err != nil {
		return SessionMeta{}, false
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return SessionMeta{Name: strings.TrimSpace(string(b))}, true
	}
	return meta, true
}

// WriteBackendSessionMeta stores a backend-specific session record.
func WriteBackendSessionMeta(persona, backend, name, id, configHash string) error {
	if err := ValidatePersonaName(persona); err != nil {
		return err
	}
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(SessionMeta{Name: name, ID: id, ConfigHash: configHash}, "", "  ")
	if err != nil {
		return err
	}
	path := BackendSessionMarker(persona, backend)
	tmp, err := os.CreateTemp(SessionsDir(), ".session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// AcquireDispatchLock gives one process exclusive ownership of a persona turn.
// The returned release function must be called after marker commit.
func AcquireDispatchLock(persona string) (func(), error) {
	return AcquireDispatchLockAt(".", persona)
}

func AcquireDispatchLockAt(root, persona string) (func(), error) {
	if err := ValidatePersonaName(persona); err != nil {
		return nil, err
	}
	sessionsDir := filepath.Join(root, SessionsDir())
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(sessionsDir, persona+".dispatch.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryDispatchFileLock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock persona %q: %w", persona, err)
	}
	if !locked {
		f.Close()
		return nil, fmt.Errorf("persona %q is busy with another live dispatch; wait for it to finish", persona)
	}
	fail := func(err error) (func(), error) { _ = unlockDispatchFile(f); _ = f.Close(); return nil, err }

	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return fail(err)
	}
	contents := strings.TrimSpace(string(raw))
	if contents != "" {
		pid64, parseErr := strconv.ParseInt(contents, 10, 32)
		if parseErr != nil || pid64 <= 0 {
			return fail(fmt.Errorf("persona %q has a malformed dispatch lock %s; expected one positive PID", persona, path))
		}
		alive, err := dispatchProcessAlive(int(pid64))
		if err != nil {
			return fail(fmt.Errorf("verify owner of persona %q dispatch lock: %w", persona, err))
		}
		if alive {
			return fail(fmt.Errorf("persona %q dispatch lock names live PID %d; refusing to reclaim it", persona, pid64))
		}
	} else if info.Size() != 0 {
		return fail(fmt.Errorf("persona %q has a malformed dispatch lock %s; expected one positive PID", persona, path))
	}
	if err := f.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			// Unlink while ownership is still held so another process can never
			// acquire this inode and then have its lock path removed by us.
			if held, err := f.Stat(); err == nil {
				if current, err := os.Lstat(path); err == nil && os.SameFile(held, current) {
					_ = os.Remove(path)
				}
			}
			_ = unlockDispatchFile(f)
			_ = f.Close()
		})
	}, nil
}

// DeleteBackendSessionMeta removes a backend-specific session record.
func DeleteBackendSessionMeta(persona, backend string) error {
	err := os.Remove(BackendSessionMarker(persona, backend))
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
	// SkipperPersona names the human-facing execution lead for this project.
	// The human operator remains captain.
	SkipperPersona string                  `yaml:"skipperPersona"`
	ModelLadder    []string                `yaml:"modelLadder"`
	SharedMemory   bool                    `yaml:"sharedMemory"`
	Routing        string                  `yaml:"routing"`
	RoutingOptions RoutingOptions          `yaml:"routingOptions"`
	RoutingOnBoot  bool                    `yaml:"routingOnBoot"`
	Fleet          FleetConfig             `yaml:"fleet"`
	Recovery       recovery.Config         `yaml:"recovery"`
	Crew           map[string]CrewOverride `yaml:"crew"`
}

// FleetConfig points the project server at a central observer instance.
// When URL is non-empty the server opens an outbound websocket on boot using
// rancher/remotedialer; the fleet can then dial back through the tunnel to the
// the project's existing 127.0.0.1 server.
//
// The secret is NEVER stored in shipmates.yaml (it gets committed to git).
// TokenEnv names an environment variable the server reads at boot to get the
// bearer token; default is SHIPMATES_FLEET_TOKEN. Set this var in your shell
// (or systemd unit, launchd plist, etc.) — the config file stays clean.
//
// Name overrides the project's identity on the fleet. Defaults to the repo
// directory name, so the clientKey is `<repo>:<persona>`. Set this if two
// clones of the same repo connect to the same fleet and collide (e.g.
// "card-cannon-dev" vs "card-cannon-scratch").
type FleetConfig struct {
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"tokenEnv"`
	Name     string `yaml:"name"`
}

// DefaultFleetTokenEnv is the env var the server reads when FleetConfig.TokenEnv
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

// CrewOverride is a per-persona Codex launch override in shipmates.yaml.
type CrewOverride struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

// LoadConfig reads shipmates.yaml, returning a zero Config if it's absent.
func LoadConfig() (*Config, error) {
	return LoadConfigAt(".")
}

func LoadConfigAt(root string) (*Config, error) {
	path := filepath.Join(root, ConfigName)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Recovery.Validate(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// PersonaConfig is the fully resolved Codex or explicit-command launch config.
type PersonaConfig struct {
	Model  string
	Effort string // --effort value (low|medium|high|xhigh|max); "" = default
}

// Fingerprint is a stable hash of the config settings that are baked into a
// session at creation and cannot change on resume: model and effort. Callers
// compare this against the stored value to auto-fresh on configuration drift.
func (c PersonaConfig) Fingerprint() string {
	return SHA([]byte(fmt.Sprintf("model=%s|effort=%s", c.Model, c.Effort)))
}

// ResolvePersonaConfig reads only shipmates.yaml. Codex persona artifacts are
// runtime instructions, not a second configuration source.
func ResolvePersonaConfig(persona string) (PersonaConfig, error) {
	return ResolvePersonaConfigAt(".", persona)
}

func ResolvePersonaConfigAt(root, persona string) (PersonaConfig, error) {
	cfg := PersonaConfig{}
	conf, err := LoadConfigAt(root)
	if err != nil {
		return cfg, err
	}
	if ov, ok := conf.Crew[persona]; ok {
		if m := strings.TrimSpace(ov.Model); m != "" {
			cfg.Model = m
		}
		if e := strings.TrimSpace(ov.Effort); e != "" {
			cfg.Effort = e
		}
	}
	return cfg, nil
}

// ModelLadderAt returns the project's explicit least-to-most-capable model
// ordering used by Sail to validate and select escalation tiers.
func ModelLadderAt(root string) ([]string, error) {
	conf, err := LoadConfigAt(root)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(conf.ModelLadder))
	ladder := make([]string, 0, len(conf.ModelLadder))
	for _, raw := range conf.ModelLadder {
		model := strings.TrimSpace(raw)
		if model == "" || seen[model] {
			return nil, errors.New("modelLadder must contain unique non-empty model names")
		}
		seen[model] = true
		ladder = append(ladder, model)
	}
	return ladder, nil
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
		return &Manifest{Version: ManifestVersion, Files: map[string]string{}}, nil
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
	return CommitManifestV2(".", m)
}

// SHA returns the hex-encoded sha256 of b, used for manifest comparisons.
func SHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
