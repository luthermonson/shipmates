package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings is the subset of Claude Code's settings.json we care about. Only
// permissions.allow/ask/deny are used here — everything else in the file is
// left alone. Fields are pointers so a missing key stays distinguishable
// from an empty array (needed for merge precedence).
type Settings struct {
	Permissions *PermissionsSection `json:"permissions,omitempty"`
}

// PermissionsSection carries the three rule arrays. Order within an array is
// preserved so users can rely on rule ordering the way Claude Code documents.
type PermissionsSection struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// MergedRules is the outcome of walking all three settings files. Rules are
// pre-parsed into Rule structs so evaluator.go doesn't reparse per call.
//
// Merge precedence follows Claude Code's documented behavior:
//   - Allow and Ask: project-local (.claude/settings.local.json) overrides
//     project (.claude/settings.json), which overrides user
//     (~/.claude/settings.json). "Overrides" means later sources win when a
//     rule text appears in more than one — but ALL rules stay present; we
//     just dedupe by raw text keeping the highest-precedence entry.
//   - Deny is a UNION across all sources. A deny anywhere wins.
type MergedRules struct {
	Allow []Rule
	Ask   []Rule
	Deny  []Rule
}

// LoadSettings walks the three settings files (user, project, project-local)
// in order and returns a merged rule set. Missing files are silently
// skipped; unreadable or malformed files are logged (by returning them via
// the errors slice) but do not abort the load — a broken settings.local.json
// should never wedge the permission gate.
func LoadSettings(projectRoot string) (MergedRules, []error) {
	var errs []error
	sources := []string{
		userSettingsPath(),
		projectSettingsPath(projectRoot),
		projectLocalSettingsPath(projectRoot),
	}
	var loaded []Settings
	for _, p := range sources {
		if p == "" {
			loaded = append(loaded, Settings{})
			continue
		}
		s, err := readSettings(p)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			loaded = append(loaded, Settings{})
			continue
		}
		loaded = append(loaded, s)
	}
	return mergeSettings(loaded), errs
}

// readSettings reads and JSON-parses one settings file. A file that exists
// but has no `permissions` key is fine — we return an empty Settings.
func readSettings(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}

// mergeSettings combines an ordered list of Settings (lowest precedence
// first) into a MergedRules. For allow/ask, higher-precedence sources
// replace duplicates in lower-precedence ones; for deny, all sources union.
func mergeSettings(sources []Settings) MergedRules {
	var out MergedRules
	// Build allow/ask by walking from highest precedence to lowest, deduping.
	seenAllow := map[string]bool{}
	seenAsk := map[string]bool{}
	seenDeny := map[string]bool{}
	for i := len(sources) - 1; i >= 0; i-- {
		s := sources[i]
		if s.Permissions == nil {
			continue
		}
		for _, raw := range s.Permissions.Allow {
			if seenAllow[raw] {
				continue
			}
			seenAllow[raw] = true
			out.Allow = append(out.Allow, ParseRule(raw))
		}
		for _, raw := range s.Permissions.Ask {
			if seenAsk[raw] {
				continue
			}
			seenAsk[raw] = true
			out.Ask = append(out.Ask, ParseRule(raw))
		}
	}
	// Deny: union in source order (lowest precedence first), deduped.
	for _, s := range sources {
		if s.Permissions == nil {
			continue
		}
		for _, raw := range s.Permissions.Deny {
			if seenDeny[raw] {
				continue
			}
			seenDeny[raw] = true
			out.Deny = append(out.Deny, ParseRule(raw))
		}
	}
	return out
}

// userSettingsPath returns ~/.claude/settings.json (or "" if $HOME isn't set,
// which shouldn't happen but shouldn't crash if it does).
func userSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

func projectSettingsPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".claude", "settings.json")
}

func projectLocalSettingsPath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".claude", "settings.local.json")
}
