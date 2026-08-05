package brig

import (
	"log/slog"
	"strings"

	"github.com/luthermonson/shipmates/internal/runtime/config"
)

// Settings is the operator's resolved brig posture. The zero value is NOT
// the default posture — use DefaultSettings() or Load(); the distinction
// keeps "forgot to load" visibly different from "operator disabled it".
type Settings struct {
	// Enabled is the master switch. Default true: installed means bound by
	// the Articles; opting out is the explicit act.
	Enabled bool
	// disabled holds the waived Article handles.
	disabled map[string]bool
}

// DefaultSettings is the posture with no user config at all: brig on, no
// Articles waived.
func DefaultSettings() Settings {
	return Settings{Enabled: true}
}

// Disabled reports whether the named Article handle is waived. A disabled
// brig waives everything.
func (s Settings) Disabled(handle string) bool {
	if !s.Enabled {
		return true
	}
	return s.disabled[handle]
}

// ArticleEnabled reports whether the Article (by number) is in force.
func (s Settings) ArticleEnabled(number int) bool {
	r, err := Get(number)
	if err != nil {
		return false
	}
	return !s.Disabled(r.Handle)
}

// DisabledHandles returns the waived handles in canonical Article order.
// Empty when the brig is fully in force.
func (s Settings) DisabledHandles() []string {
	var out []string
	for _, r := range canonicalRules {
		if s.disabled[r.Handle] {
			out = append(out, r.Handle)
		}
	}
	return out
}

// FromConfig resolves a user-config brig block into Settings. Unknown
// handles in disabled_articles are warned about and ignored — a typo must
// not silently waive a different Article, and must not waive nothing
// silently either.
func FromConfig(b config.Brig) Settings {
	s := DefaultSettings()
	if b.Enabled != nil {
		s.Enabled = *b.Enabled
	}
	for _, raw := range b.DisabledArticles {
		handle := strings.ToLower(strings.TrimSpace(raw))
		if handle == "" {
			continue
		}
		if _, ok := ByHandle(handle); !ok {
			slog.Warn("brig: unknown Article handle in disabled_articles; ignoring",
				"handle", raw)
			continue
		}
		if s.disabled == nil {
			s.disabled = map[string]bool{}
		}
		s.disabled[handle] = true
	}
	return s
}

// Load reads the operator's brig posture from ~/.shipmates/config.yaml.
// home="" resolves the real home directory; tests pass a temp dir.
//
// The brig block is honored from USER config only. That is deliberate and
// mirrors internal/runtime/config's trust boundary: a project checkout may
// not name executables, may not weaken containment, and may not un-sign the
// Articles. A `brig:` key in a project's .shipmates/config.yaml lands in
// ProjectFile.Ignored and is warned about by the runtime selector.
//
// A missing or unreadable user config yields the default posture (enabled),
// same as every other absent user-config block.
func Load(home string) Settings {
	user, err := config.LoadUser(home)
	if err != nil {
		slog.Warn("brig: could not read user config; the Articles stay in force", "err", err)
		return DefaultSettings()
	}
	return FromConfig(user.Brig)
}
