package fleet

import (
	"regexp"
	"strings"
	"testing"
)

// M3 — XSS in the fleet UI.
//
// There is no JavaScript toolchain in this repo (no node, no package.json, and
// CI installs only Go), so the UI's behaviour is pinned two ways:
//
//  1. Server-side, where the hostile value actually enters the system:
//     sanitizeRepoURL in hardening_test.go proves the ship's
//     X-Shipmates-Repo-URL header can never reach a browser as a
//     javascript: URL or as a string carrying a quote.
//
//  2. Source invariants over ui/app.js, below. These are the properties the
//     finding was about — escape() must cover quotes, a URL must be validated
//     before it becomes an href, and the bead row must not be assembled as an
//     HTML string — and each one FAILS against the pre-fix file.
//
// The rendering itself was verified by executing the real app.js in headless
// Chrome against the hostile inputs from the finding (a bead whose status and
// title are `" onmouseover=… autofocus onfocus=…`, an external_ref of gh-7,
// and repo_url values of `javascript:…` and `" onload=… x="`), driving
// renderBeads/appendEventDOM and then walking the resulting DOM for injected
// attributes. Before the fix that produced SPAN[onmouseover], SPAN[onfocus],
// A[href=javascript:…] and A[onload=…]; after it, zero injected attributes and
// zero anchors for the hostile inputs, with legitimate repo URLs still
// linking. Re-run instructions are in the PR for #42.

func appJS(t *testing.T) string {
	t.Helper()
	raw, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("read embedded ui/app.js: %v", err)
	}
	return string(raw)
}

// escape()'s output is interpolated into double-quoted HTML attributes. A `"`
// closes the attribute early, and `" onmouseover=x` in a bead title or status
// then becomes an event handler on the fleet origin — the origin that holds
// the operator's session cookie.
func TestUI_EscapeCoversQuotes(t *testing.T) {
	src := appJS(t)
	start := strings.Index(src, "function escape(s)")
	if start < 0 {
		t.Fatal("ui/app.js no longer defines escape()")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("could not find the end of escape()")
	}
	body := src[start : start+end]

	for _, want := range []struct{ char, entity string }{
		{"&", "&amp;"},
		{"<", "&lt;"},
		{">", "&gt;"},
		{`"`, "&quot;"},
		{"'", "&#39;"},
	} {
		if !strings.Contains(body, want.entity) {
			t.Errorf("escape() does not produce %s for %q — its output lands in quoted attributes:\n%s",
				want.entity, want.char, body)
		}
	}
	// & must be replaced first or the later replacements get double-escaped.
	amp := strings.Index(body, "&amp;")
	for _, later := range []string{"&lt;", "&gt;", "&quot;", "&#39;"} {
		if i := strings.Index(body, later); i >= 0 && i < amp {
			t.Errorf("%s is replaced before &amp;, which double-escapes entities", later)
		}
	}
}

// A URL is a code-execution sink: escaping does nothing to javascript:alert(1),
// which contains no character escape() touches. Every URL that reaches an href
// has to go through a scheme check first.
func TestUI_HasURLSchemeAllowList(t *testing.T) {
	src := appJS(t)
	if !strings.Contains(src, "function safeURL(") {
		t.Fatal("ui/app.js has no safeURL() — nothing validates a URL before it becomes an href")
	}
	for _, want := range []string{`"http:"`, `"https:"`} {
		if !strings.Contains(src, want) {
			t.Errorf("safeURL must allow-list %s explicitly", want)
		}
	}
	if !strings.Contains(src, "new URL(") {
		t.Error("safeURL should parse with the URL constructor, which also encodes the result")
	}
}

// repo_url arrives from the ship's X-Shipmates-Repo-URL header — i.e. from
// `git config remote.origin.url` on a machine the fleet does not control. It
// gets exactly one entry point in the UI, and that entry point validates.
func TestUI_RepoURLOnlyReadThroughSafeURL(t *testing.T) {
	for _, line := range strings.Split(appJS(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "repo_url") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(trimmed, "safeURL(") {
			t.Errorf("repo_url is read without safeURL():\n\t%s", trimmed)
		}
	}
}

// attrInterp finds an interpolation that lands inside a double-quoted HTML
// attribute in a template literal — the exact shape `href="${captain.repo_url}"`
// that the finding cited.
var attrInterp = regexp.MustCompile(`[A-Za-z-]+="[^"\n]*\$\{([^{}]*)\}`)

func TestUI_AttributeInterpolationsAreEscaped(t *testing.T) {
	for _, name := range []string{"ui/app.js", "ui/conversation.js"} {
		raw, err := uiFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range attrInterp.FindAllStringSubmatch(string(raw), -1) {
			expr := strings.TrimSpace(m[1])
			if !strings.HasPrefix(expr, "escape(") {
				t.Errorf("%s: value interpolated into a quoted attribute without escape(): %s", name, m[0])
			}
		}
	}
}

// The bead row is built from ship-supplied fields (status, id, title,
// external_ref). Assembling it as an HTML string is what made a quote in any
// of them dangerous; nodes with textContent/setAttribute have no parser behind
// them to escape out of.
func TestUI_BeadRowsAreBuiltFromNodes(t *testing.T) {
	src := appJS(t)
	if strings.Contains(src, "row.innerHTML") {
		t.Error("bead rows are still assembled with innerHTML from ship-supplied fields")
	}
	if !strings.Contains(src, "function refLinkEl(") {
		t.Error("external refs should render as a node (refLinkEl), not as an HTML string")
	}
	if strings.Contains(src, "function refLink(") {
		t.Error("the HTML-string refLink() is back; it returns markup built from a remote ref")
	}
	if !strings.Contains(src, `a.setAttribute("href", url)`) {
		t.Error("the ref anchor should get its href via setAttribute, not string interpolation")
	}
}

// A quick smoke check that the pieces are actually wired together: the two
// call sites the finding named must go through the validated helpers.
func TestUI_LinkHelpersAreUsed(t *testing.T) {
	src := appJS(t)
	for _, want := range []string{
		"function repoBase(",
		"const base = repoBase(",
		"const base = repoBase(knownCaptains.get(captainKey));",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("ui/app.js is missing %q", want)
		}
	}
}
