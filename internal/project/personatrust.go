package project

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// # Persona trust boundary
//
// A persona's launch config has two sources, and they are NOT the same schema.
// That asymmetry is the point, and it mirrors internal/runtime/config exactly.
//
// The persona file (.claude/agents/<persona>.md) and shipmates.yaml both live
// inside the project checkout. On a cloned repository they are
// attacker-controlled content that shipmates reads before the operator has
// reviewed anything. They may therefore answer only presentation-shaped
// questions — which model, how much effort, whether the persona wants a berth,
// whether it is a fleet member at all. They may NOT name an executable,
// contribute arguments to one, choose the directory it runs in, or waive the
// human permission gate. A repository that could do any of those would get
// arbitrary code execution out of `git clone` plus one `shipmates open`.
//
// Enforcement is structural, not a filter: [personaFrontmatter] and
// [CrewOverride] simply have no fields for backend/command/cwd/
// dangerouslySkipPermissions. A filter is a denylist, and the first
// execution-shaped key someone adds without updating the denylist silently
// reopens the hole. A type cannot forget.
//
// Those settings are honored only from [UserPersonaFile] —
// ~/.shipmates/personas.yaml, the operator's own file, outside every checkout.
//
// permissions.mode is the one field both files carry, because every shipped
// catalog persona sets it. The repo-supplied side is bounded by
// [RepoPermissionModes], an ALLOWLIST — a repository may pick a mode no weaker
// than the ones shipmates ships with, and bypassPermissions (plus any mode
// invented later) is refused by default rather than by being enumerated.
//
// See docs/security.md for the operator-facing version of this.

// UserPersonasName is the operator's persona-config file under ~/.shipmates/.
const UserPersonasName = "personas.yaml"

// RepoPermissionModes is the allowlist of permission modes a repo-supplied
// persona file or shipmates.yaml crew override may select.
//
// It is deliberately an allowlist. "bypassPermissions" is absent because it
// waives the human gate entirely, and so is every mode nobody has invented
// yet: a new mode name reaching this code is refused until an operator adds
// it here on purpose. An operator who wants bypass for a persona says so in
// ~/.shipmates/personas.yaml, where a checkout cannot reach.
var RepoPermissionModes = []string{"ask", "acceptEdits", "plan", "default"}

// repoModeAllowed reports whether a repo-supplied permissions.mode may be
// honored. The empty string ("unset") always may.
func repoModeAllowed(mode string) bool {
	if mode == "" {
		return true
	}
	for _, m := range RepoPermissionModes {
		if m == mode {
			return true
		}
	}
	return false
}

// UserPersonaEntry is the full per-persona launch schema. Honored only from
// the operator's ~/.shipmates/personas.yaml — see the trust-boundary note
// above. Every field a project checkout may also set is repeated here, so an
// operator can override a repo's presentation choices from one place.
type UserPersonaEntry struct {
	// Backend selects the mate driver: "claude" (default) or "command".
	Backend string `yaml:"backend"`
	// Command is the argv for backend "command". Operator-only: this names an
	// executable, which is the whole reason this file exists.
	Command []string `yaml:"command"`
	// CWD is an explicit spawn directory for the persona.
	CWD string `yaml:"cwd"`
	// Permissions.Mode may be any mode, including bypassPermissions.
	Permissions struct {
		Mode string `yaml:"mode"`
	} `yaml:"permissions"`
	// DangerouslySkipPermissions waives the gate outright.
	DangerouslySkipPermissions *bool `yaml:"dangerouslySkipPermissions"`

	// Presentation-shaped fields, repeated so the operator's file wins when
	// they disagree with the checkout.
	Model         string    `yaml:"model"`
	Effort        string    `yaml:"effort"`
	Berth         string    `yaml:"berth"`
	RemoteControl yaml.Node `yaml:"remoteControl"`
}

// UserPersonaFile is the on-disk shape of ~/.shipmates/personas.yaml:
//
//	personas:
//	  aider:
//	    backend: command
//	    command: [aider, --model, gpt-5]
//	    cwd: /home/me/src/thing
//	  backend:
//	    dangerouslySkipPermissions: true
//	  security:
//	    permissions: { mode: bypassPermissions }
//
// Keys are persona names, matching .claude/agents/<persona>.md.
type UserPersonaFile struct {
	Personas map[string]UserPersonaEntry `yaml:"personas"`
}

// UserPersonasPath returns ~/.shipmates/personas.yaml. An empty home resolves
// via os.UserHomeDir; if that fails the second return is false and callers
// should treat the operator's file as absent.
func UserPersonasPath(home string) (string, bool) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = h
	}
	return filepath.Join(home, Dir, UserPersonasName), true
}

// LoadUserPersonas reads ~/.shipmates/personas.yaml. A missing file or an
// undiscoverable home is not an error — it is the ordinary "the operator has
// not vouched for anything" case, which is also the default-deny case.
func LoadUserPersonas(home string) (UserPersonaFile, error) {
	path, ok := UserPersonasPath(home)
	if !ok {
		return UserPersonaFile{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return UserPersonaFile{}, nil
		}
		return UserPersonaFile{}, fmt.Errorf("read %s: %w", path, err)
	}
	var out UserPersonaFile
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return UserPersonaFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// RepoRefusal records one repo-supplied setting shipmates declined to honor.
// It exists so a refusal is visible — silently dropping a setting is how an
// operator ends up believing it applied.
type RepoRefusal struct {
	// Path is the file the setting came from, e.g. ".claude/agents/x.md".
	Path string
	// Key is the dotted YAML key, e.g. "command" or "permissions.mode".
	Key string
	// Value is what it tried to set, rendered for a log line.
	Value string
}

// String renders a refusal for a human.
func (r RepoRefusal) String() string {
	return fmt.Sprintf("%s: %s=%s", r.Path, r.Key, r.Value)
}

// operatorOnlyPersonaKeys is the set of YAML keys the operator's schema
// understands but a repo-supplied schema does not — computed once from the
// difference between the structs themselves.
//
// It is derived rather than written down so it cannot drift: adding a field to
// [UserPersonaEntry] without adding it to [personaFrontmatter] automatically
// makes it operator-only AND automatically makes a repo that sets it get
// warned about. The set is used only for REPORTING; the enforcement is that
// the repo structs have no such fields to decode into.
var operatorOnlyPersonaKeys = func() map[string]bool {
	user := map[string]bool{}
	yamlKeys(reflect.TypeOf(UserPersonaEntry{}), "", user)
	for _, t := range []reflect.Type{
		reflect.TypeOf(personaFrontmatter{}),
		reflect.TypeOf(CrewOverride{}),
	} {
		repo := map[string]bool{}
		yamlKeys(t, "", repo)
		for k := range repo {
			delete(user, k)
		}
	}
	return user
}()

// yamlLeafStructs are struct types yamlKeys must treat as a single value
// rather than recursing into.
var yamlLeafStructs = map[reflect.Type]bool{reflect.TypeOf(yaml.Node{}): true}

// yamlKeys collects the dotted YAML key paths a struct type decodes.
func yamlKeys(t reflect.Type, prefix string, out map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		key := prefix + tag
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && !yamlLeafStructs[ft] {
			yamlKeys(ft, key+".", out)
			continue
		}
		out[key] = true
	}
}

// scanRefusedKeys walks a decoded YAML mapping and reports every key that only
// the operator's schema may supply, so the caller can warn about it. node must
// be a mapping node (or zero, for "nothing supplied").
//
// path names the file for the message. report prefixes the reported key with
// wherever the mapping sits in that file (e.g. "crew.security." for a
// shipmates.yaml override); the schema lookup itself is always unprefixed.
func scanRefusedKeys(node *yaml.Node, path, report string) []RepoRefusal {
	out := scanRefusedAt(node, path, report, "")
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func scanRefusedAt(node *yaml.Node, path, report, within string) []RepoRefusal {
	if node == nil {
		return nil
	}
	n := node
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	var out []RepoRefusal
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		key := within + k.Value
		if operatorOnlyPersonaKeys[key] {
			out = append(out, RepoRefusal{Path: path, Key: report + key, Value: renderNode(v)})
			continue
		}
		// Recurse into nested mappings the operator schema knows about (today:
		// permissions:), so a permissions.cwd-style key added later is caught
		// too without anyone remembering to update a list.
		if v.Kind == yaml.MappingNode && hasKeyPrefix(key+".") {
			out = append(out, scanRefusedAt(v, path, report, key+".")...)
		}
	}
	return out
}

// hasKeyPrefix reports whether any operator-only key sits under prefix.
func hasKeyPrefix(prefix string) bool {
	for k := range operatorOnlyPersonaKeys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// renderNode flattens a YAML value to a single-line string for a log message.
func renderNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.SequenceNode:
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			parts = append(parts, renderNode(c))
		}
		return "[" + strings.Join(parts, " ") + "]"
	default:
		return "(" + strings.ToLower(strings.TrimSuffix(n.Tag, "!!")) + ")"
	}
}

// warnedRefusals dedupes the refusal warning. ResolvePersonaConfig runs on
// every permission decision, so warning unconditionally would bury the log in
// the same line thousands of times; an operator needs to see it once.
var warnedRefusals sync.Map

// warnRefusals logs each refusal once per process, naming the file, the key,
// and where the setting has to live instead.
func warnRefusals(persona string, refusals []RepoRefusal) {
	for _, r := range refusals {
		if _, dup := warnedRefusals.LoadOrStore(r.Path+"|"+r.Key+"|"+r.Value, struct{}{}); dup {
			continue
		}
		path, _ := UserPersonasPath("")
		slog.Warn("ignoring repo-supplied persona setting: this key is operator-only",
			"persona", persona,
			"file", r.Path,
			"key", r.Key,
			"value", r.Value,
			"why", "a checkout that could set it would get code execution or waive the permission gate on clone",
			"move_it_to", path)
	}
}

// applyUserPersona overlays the operator's entry for a persona. It runs last,
// so the operator's file outranks anything in the checkout — including the
// presentation fields both schemas carry.
func applyUserPersona(cfg *PersonaConfig, rc *yaml.Node, entry UserPersonaEntry) {
	if b := strings.TrimSpace(entry.Backend); b != "" {
		cfg.Backend = b
	}
	if len(entry.Command) > 0 {
		cfg.Command = entry.Command
	}
	if c := strings.TrimSpace(entry.CWD); c != "" {
		cfg.CWD = c
	}
	if m := strings.TrimSpace(entry.Permissions.Mode); m != "" {
		cfg.Mode = m
	}
	if entry.DangerouslySkipPermissions != nil {
		cfg.DangerouslySkipPermissions = *entry.DangerouslySkipPermissions
	}
	if m := strings.TrimSpace(entry.Model); m != "" {
		cfg.Model = m
	}
	if e := strings.TrimSpace(entry.Effort); e != "" {
		cfg.Effort = e
	}
	if b := strings.TrimSpace(entry.Berth); b != "" {
		cfg.Berth = b
	}
	if entry.RemoteControl.Kind != 0 {
		*rc = entry.RemoteControl
	}
}
