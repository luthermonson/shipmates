package permissions

import (
	"strings"
)

// readOnlyBuiltins are commands Claude Code auto-allows without prompting or
// consulting settings.json. These are the "always safe" reads that would
// otherwise generate an absurd amount of permission noise. Matched by the
// first token (post-wrapper-stripping) OR by the first two tokens for git
// subcommands.
//
// This list is intentionally conservative — it's what Claude Code itself
// treats as read-only in its default configuration. Anything not on this
// list falls through to the settings-driven rules.
var readOnlyBuiltins = map[string]bool{
	"ls":    true,
	"cat":   true,
	"echo":  true,
	"pwd":   true,
	"head":  true,
	"tail":  true,
	"grep":  true,
	"find":  true,
	"wc":    true,
	"which": true,
	"diff":  true,
	"stat":  true,
	"du":    true,
	"cd":    true,
	"file":  true,
	"env":   true,
	"date":  true,
	"true":  true,
	"false": true,
	"test":  true,
	"[":     true,
}

// readOnlyGitSubcommands are the git verbs Claude Code treats as read-only.
// Anything else under git (add, commit, push, checkout, reset, rebase, …)
// falls through to the settings rules.
var readOnlyGitSubcommands = map[string]bool{
	"status":      true,
	"log":         true,
	"diff":        true,
	"show":        true,
	"blame":       true,
	"branch":      true,
	"tag":         true,
	"describe":    true,
	"config":      true, // read-only when no --set/--add; conservative here — Claude allows it
	"remote":      true, // ditto — bare `git remote` is a list
	"rev-parse":   true,
	"ls-files":    true,
	"ls-tree":     true,
	"cat-file":    true,
	"reflog":      true,
	"stash":       true, // `git stash list` etc.; write forms exist but common usage is read
	"shortlog":    true,
	"whatchanged": true,
	"grep":        true,
}

// processWrappers are prefix commands that just delegate to their remaining
// arguments. Claude Code strips these before rule evaluation so
// `timeout 30 npm test` matches `Bash(npm test)`.
//
// Note we do NOT strip environment-runners like `direnv exec`, `devbox run`,
// `npx`, `docker exec`, `env FOO=bar cmd` — those are not on Claude Code's
// wrapper list. They change the resolution semantics enough that treating
// them as transparent would be unsafe.
var processWrappers = map[string]bool{
	"timeout":  true,
	"time":     true,
	"nice":     true,
	"nohup":    true,
	"stdbuf":   true,
	"xargs":    true, // bare `xargs cmd` — options are dropped by stripWrappers
	"ionice":   true,
	"chrt":     true,
	"unbuffer": true,
}

// compoundOperators are the shell operators that chain independent commands.
// The permission model treats every subcommand as separately gated — the
// compound is allowed only if EVERY subcommand is allowed.
var compoundOperators = []string{"&&", "||", "|&", ";", "|", "&"}

// SplitCompound splits a shell command line into its independent subcommands
// on the compound operators. Quoting is respected so `echo "a && b"` stays
// one subcommand. Backslash-escapes are honored inside double quotes for the
// common `\"` case; more exotic escape handling is deliberately skipped —
// the goal is match Claude Code's own splitter, which is likewise pragmatic.
func SplitCompound(cmd string) []string {
	var parts []string
	var buf strings.Builder
	i := 0
	n := len(cmd)
	inSingle := false
	inDouble := false
	for i < n {
		c := cmd[i]
		if !inSingle && !inDouble {
			// Try each operator at this position.
			matched := ""
			for _, op := range compoundOperators {
				if strings.HasPrefix(cmd[i:], op) {
					if len(op) > len(matched) {
						matched = op
					}
				}
			}
			if matched != "" {
				parts = append(parts, strings.TrimSpace(buf.String()))
				buf.Reset()
				i += len(matched)
				continue
			}
		}
		switch c {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '\\':
			// Preserve the next char verbatim inside double quotes / unquoted.
			if !inSingle && i+1 < n {
				buf.WriteByte(c)
				buf.WriteByte(cmd[i+1])
				i += 2
				continue
			}
		case '$':
			// $(...) — treat as opaque; skip past matching paren so operators
			// inside a subshell don't split us. Nested handling is best-effort.
			if !inSingle && i+1 < n && cmd[i+1] == '(' {
				buf.WriteByte(c)
				buf.WriteByte('(')
				depth := 1
				i += 2
				for i < n && depth > 0 {
					if cmd[i] == '(' {
						depth++
					}
					if cmd[i] == ')' {
						depth--
					}
					buf.WriteByte(cmd[i])
					i++
				}
				continue
			}
		}
		buf.WriteByte(c)
		i++
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		parts = append(parts, s)
	}
	// Filter empties.
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripWrappers peels off process-wrapper prefixes. `timeout 30 npm test`
// becomes `npm test`; `nice -n 10 pytest` becomes `pytest`. Numeric args and
// short flags after a wrapper are consumed heuristically — enough to handle
// the common cases without needing a full arg parser per wrapper.
//
// The strip runs iteratively: `time timeout 30 make test` → `timeout 30 make
// test` → `make test`. That way stacked wrappers don't need bespoke handling.
func stripWrappers(cmd string) string {
	for {
		tokens := shellSplit(cmd)
		if len(tokens) == 0 {
			return cmd
		}
		head := tokens[0]
		if !processWrappers[head] {
			return cmd
		}
		// Consume trailing flag/numeric args on the wrapper. Options like
		// `-n 10` and bare numeric args like `timeout 30` all get eaten.
		rest := tokens[1:]
		for len(rest) > 0 && looksLikeWrapperArg(rest[0]) {
			// Some flags take a value (`-n 10`); heuristic: if the flag
			// doesn't include `=` and the next token is numeric, eat it too.
			flag := rest[0]
			rest = rest[1:]
			if strings.HasPrefix(flag, "-") && !strings.Contains(flag, "=") &&
				len(rest) > 0 && isNumeric(rest[0]) {
				rest = rest[1:]
			}
		}
		if len(rest) == 0 {
			return cmd
		}
		next := strings.Join(rest, " ")
		if next == cmd {
			return cmd
		}
		cmd = next
	}
}

func looksLikeWrapperArg(tok string) bool {
	if tok == "" {
		return false
	}
	if strings.HasPrefix(tok, "-") {
		return true
	}
	return isNumeric(tok)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// shellSplit is a minimal token split that respects single and double quotes.
// It's not a full shell parser; it just needs to find command boundaries and
// the first token or two for readOnly / wrapper checks.
func shellSplit(s string) []string {
	var out []string
	var buf strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ' ' && !inSingle && !inDouble:
			flush()
		default:
			buf.WriteByte(c)
		}
	}
	flush()
	return out
}

// isReadOnlyBuiltin reports whether the (post-strip) command is one Claude
// Code considers safe without any settings-rule consultation. Returns
// (true, reason) with a human-readable reason on match.
func isReadOnlyBuiltin(cmd string) (bool, string) {
	tokens := shellSplit(cmd)
	if len(tokens) == 0 {
		return false, ""
	}
	head := tokens[0]
	if readOnlyBuiltins[head] {
		return true, "read-only builtin: " + head
	}
	if head == "git" && len(tokens) >= 2 {
		sub := tokens[1]
		if readOnlyGitSubcommands[sub] {
			return true, "read-only git: git " + sub
		}
	}
	return false, ""
}

// prepareBashCommand strips process wrappers and returns the (possibly
// modified) command line ready for rule matching.
func prepareBashCommand(cmd string) string {
	return strings.TrimSpace(stripWrappers(strings.TrimSpace(cmd)))
}
