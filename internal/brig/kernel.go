package brig

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/luthermonson/shipmates/internal/permissions"
)

// The kernel layer compiles the conduct Articles into rules the existing
// permissions evaluator enforces — the same Rule shape, the same Bash/path
// matchers, the same deny > ask > allow precedence as project settings and
// persona overlays. There is no parallel enforcement engine: the Brig is
// one more rule source on the evaluator, installed via
// permissions.Evaluator.SetBrigPolicy.
//
// Each compiled rule's Raw string carries a "brig: Article N (handle)" tag,
// which does two jobs: the evaluator's reason strings name the Article that
// fired (so the operator sees WHY in the event feed), and the server's
// PreToolUse gate recognizes Brig denials and appends them to the
// .shipmates/brig.log denial log.
//
// Persona overlays cannot loosen these: deny beats everything in the
// evaluator, and ask beats allow, so neither a persona `allow` nor a
// project settings rule can shadow a Brig deny or skip a Brig ask. The one
// sanctioned waiver path is the operator's own config —
// `brig.disabled_articles` in ~/.shipmates/config.yaml.

// kernelEntry is one Article's compiled rule strings.
type kernelEntry struct {
	article int
	ask     []string
	deny    []string
}

// kernelEntries enumerates the evaluator rules each kernel-layer Article
// compiles to. Patterns use the evaluator's documented semantics: Bash
// patterns are space-boundary globs matched against every subcommand of a
// compound (pipes included — which is what lands the piped-execution denies
// on the bare interpreter), path patterns are gitignore-style.
var kernelEntries = []kernelEntry{
	{
		article: 6, // no-prod-db
		ask: []string{
			"Bash(psql)", "Bash(psql *)",
			"Bash(mysql)", "Bash(mysql *)",
			"Bash(mongo)", "Bash(mongo *)",
			"Bash(mongosh)", "Bash(mongosh *)",
			"Bash(*DROP TABLE*)", "Bash(*DROP DATABASE*)", "Bash(*TRUNCATE TABLE*)",
			"PowerShell(*DROP TABLE*)", "PowerShell(*DROP DATABASE*)", "PowerShell(*TRUNCATE TABLE*)",
		},
	},
	{
		article: 7, // no-destructive-git
		deny: []string{
			"Bash(git push*--force*)",
			"Bash(git push -f)", "Bash(git push -f *)", "Bash(git push * -f)", "Bash(git push * -f *)",
			"Bash(git reset --hard origin/*)", "Bash(git reset --hard upstream/*)",
			"Bash(git branch -D)", "Bash(git branch -D *)",
			"Bash(git clean -fdx*)", "Bash(git clean -fx*)", "Bash(git clean -xdf*)", "Bash(git clean -xfd*)",
			"Bash(git filter-repo*)", "Bash(git filter-branch*)",
			"Bash(git tag -f)", "Bash(git tag -f *)",
			"Bash(git rebase -i)", "Bash(git rebase -i *)", "Bash(git rebase --interactive*)",
		},
	},
	{
		article: 8, // no-secrets-in-commits
		deny: []string{
			"Write(.env)", "Edit(.env)", "MultiEdit(.env)",
			"Write(.env.*)", "Edit(.env.*)", "MultiEdit(.env.*)",
			"Write(id_rsa)", "Edit(id_rsa)", "MultiEdit(id_rsa)",
			"Write(id_ed25519)", "Edit(id_ed25519)", "MultiEdit(id_ed25519)",
			"Write(*.pem)", "Edit(*.pem)", "MultiEdit(*.pem)",
			"Write(credentials*)", "Edit(credentials*)", "MultiEdit(credentials*)",
		},
	},
	{
		article: 9, // verify-every-package
		ask: []string{
			"Bash(npm install *)", "Bash(npm i *)", "Bash(npm add *)",
			"Bash(yarn add *)", "Bash(pnpm add *)",
			"Bash(pip install *)", "Bash(pip3 install *)", "Bash(pipx install *)",
			"Bash(go get *)",
			"Bash(cargo add *)",
			"Bash(gem install *)",
			"Bash(brew install *)",
		},
	},
	{
		article: 10, // no-piped-execution
		// The evaluator splits `curl … | sh` into ["curl …", "sh"], so the
		// deny lands on the bare interpreter subcommand — exactly the shape
		// that only ever appears as the receiving end of a pipe in agent
		// usage (an interactive bare shell would hang the session anyway).
		//
		// A directly typed `sh -c 'curl … | sh'` reaches the SAME rule rather
		// than needing one of its own: the evaluator now judges an `sh -c`
		// script (and a command-substitution body) as a command line, so the
		// pipe inside it splits and the inner bare `sh` matches here. Adding
		// `Bash(sh -c*)` was the other option and was rejected — a shell
		// one-liner is ordinary work, and denying every one of them would
		// make this Article mean "no shell scripting", which is not what it
		// says and not what the incident basis supports.
		deny: []string{
			"Bash(sh)", "Bash(bash)", "Bash(zsh)",
			"Bash(sh -s*)", "Bash(bash -s*)", "Bash(zsh -s*)",
			"PowerShell(iex)", "PowerShell(iex *)",
			"PowerShell(Invoke-Expression)", "PowerShell(Invoke-Expression *)",
			"PowerShell(*Invoke-Expression*Invoke-WebRequest*)",
			"PowerShell(*iex*(*irm *)",
		},
	},
	{
		article: 13, // confirm-before-destroying
		ask: []string{
			"Bash(rm -rf*)", "Bash(rm -fr*)", "Bash(rm -r *)", "Bash(rm -R *)",
			"PowerShell(Remove-Item * -Recurse*)", "PowerShell(Remove-Item -Recurse*)",
			"PowerShell(rm -rf*)", "PowerShell(rm -r *)",
		},
	},
	{
		article: 14, // no-self-escalation
		deny: []string{
			"Write(**/.shipmates/policies/**)", "Edit(**/.shipmates/policies/**)", "MultiEdit(**/.shipmates/policies/**)",
			"Write(**/.claude/settings.json)", "Edit(**/.claude/settings.json)", "MultiEdit(**/.claude/settings.json)",
			"Write(**/.claude/settings.local.json)", "Edit(**/.claude/settings.local.json)", "MultiEdit(**/.claude/settings.local.json)",
			"Write(**/.shipmates/config.yaml)", "Edit(**/.shipmates/config.yaml)", "MultiEdit(**/.shipmates/config.yaml)",
		},
	},
	{
		article: 15, // stay-aboard
		// The .ssh/.aws/.gnupg targets are `**/`-anchored on a basename, so
		// they already match `C:/Users/x/.ssh/...` — no Windows twin needed.
		// The only Unix-shaped absolute target is /etc, and its intent —
		// "the machine's system configuration, outside the ship" — has a
		// Windows equivalent the matcher cannot reach from a Unix path:
		// C:\Windows (the system tree, which is also where the real hosts
		// file lives, under System32\drivers\etc). The per-user Startup
		// folder is the second: a write there is an autorun-persistence
		// foothold, the same "touch a high-value OS location, not the
		// project" move /etc guards against. Patterns are written in the
		// slash spelling the matcher normalizes every path to.
		deny: []string{
			"Write(/etc/**)", "Edit(/etc/**)", "MultiEdit(/etc/**)",
			"Write(C:/Windows/**)", "Edit(C:/Windows/**)", "MultiEdit(C:/Windows/**)",
			"Write(**/Start Menu/Programs/Startup/**)", "Edit(**/Start Menu/Programs/Startup/**)", "MultiEdit(**/Start Menu/Programs/Startup/**)",
			"Write(**/.ssh/**)", "Edit(**/.ssh/**)", "MultiEdit(**/.ssh/**)",
			"Write(**/.aws/**)", "Edit(**/.aws/**)", "MultiEdit(**/.aws/**)",
			"Write(**/.gnupg/**)", "Edit(**/.gnupg/**)", "MultiEdit(**/.gnupg/**)",
		},
	},
}

// KernelRules compiles the in-force kernel Articles into evaluator rules,
// honoring the operator's Settings: a disabled brig compiles to nothing,
// and a waived Article contributes no rules. The returned slices are ready
// for permissions.Evaluator.SetBrigPolicy.
func KernelRules(s Settings) (ask, deny []permissions.Rule) {
	if !s.Enabled {
		return nil, nil
	}
	for _, e := range kernelEntries {
		r, err := Get(e.article)
		if err != nil {
			continue // unreachable while kernelEntries and canonicalRules agree
		}
		if s.Disabled(r.Handle) {
			continue
		}
		for _, raw := range e.ask {
			ask = append(ask, taggedRule(e.article, r.Handle, raw))
		}
		for _, raw := range e.deny {
			deny = append(deny, taggedRule(e.article, r.Handle, raw))
		}
	}
	return ask, deny
}

// taggedRule parses one rule string and stamps the Article attribution into
// its Raw text, which is what the evaluator quotes in reason strings.
func taggedRule(article int, handle, raw string) permissions.Rule {
	r := permissions.ParseRule(raw)
	r.Raw = fmt.Sprintf("brig: Article %d (%s) %s", article, handle, raw)
	return r
}

// denialReasonRE extracts the Article number from an evaluator reason string
// produced by a tagged rule.
var denialReasonRE = regexp.MustCompile(`brig: Article (\d+) \(`)

// DenialArticle reports which Article (if any) a permission-decision reason
// string is attributed to. Returns (0, false) for reasons the Brig didn't
// produce. The server's PreToolUse gate uses this to decide what belongs in
// the denial log.
func DenialArticle(reason string) (int, bool) {
	m := denialReasonRE.FindStringSubmatch(reason)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
