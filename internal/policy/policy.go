// Package policy implements the pure, versioned Shipmates policy contract.
// It intentionally performs no I/O and has no runtime or protocol dependencies.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion     = 1
	MaxSourceBytes    = 256 << 10
	MaxRules          = 512
	MaxRulesPerEffect = 256
	MaxDiagnostics    = 32
	MaxMatchedRules   = 64
)

type Layer string

const (
	LayerProject      Layer = "project"
	LayerProjectLocal Layer = "project-local"
	LayerPersona      Layer = "persona"
)

type Effect string

const (
	Allow Effect = "allow"
	Ask   Effect = "ask"
	Deny  Effect = "deny"
)

type RequestKind string

const ProcessExec RequestKind = "process.exec"

type SourceDescriptor struct {
	Layer   Layer
	Path    string
	Present bool
}
type Source struct {
	Descriptor SourceDescriptor
	Bytes      []byte
}
type Rule struct {
	ID           string
	Kind         RequestKind
	CommandExact string
	Reason       string
	Effect       Effect
	Source       SourceDescriptor
}
type Snapshot struct {
	SchemaVersion              int
	Persona, ProjectRootID, ID string
	Sources                    []SourceDescriptor
	Rules                      []Rule
}
type Request struct {
	Kind         RequestKind
	CommandExact string
}
type MatchedRule struct {
	Layer    Layer
	Path, ID string
	Effect   Effect
}
type Explanation struct {
	SchemaVersion    int
	PolicySnapshotID string
	RequestKind      RequestKind
	PolicyEffect     Effect
	ReasonCode       string
	MatchedRules     []MatchedRule
	OmittedMatches   int
}
type Diagnostic struct {
	SchemaVersion   int
	Code            string
	Layer           Layer
	Path            string
	Line, Column    int
	RuleID, Message string
}

var ruleIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// Parse validates all supplied sources as one effective snapshot. Values returned
// in a Snapshot are deep copies and are safe for concurrent evaluation.
func Parse(persona, projectRootID string, sources []Source) (*Snapshot, []Diagnostic) {
	ds := make([]Diagnostic, 0)
	add := func(d Diagnostic) {
		if len(ds) < MaxDiagnostics {
			d.SchemaVersion = SchemaVersion
			if len(d.Message) > 160 {
				d.Message = d.Message[:160]
			}
			ds = append(ds, d)
		}
	}
	if !ruleIDRE.MatchString(persona) || len(persona) > 80 || projectRootID == "" || len(projectRootID) > 256 {
		add(Diagnostic{Code: "invalid_type", Message: "snapshot identity fields must be non-empty"})
		return nil, ds
	}
	expected := map[Layer]string{
		LayerProject:      ".shipmates/policy.yaml",
		LayerProjectLocal: ".shipmates/policy.local.yaml",
		LayerPersona:      ".shipmates/policies/" + persona + ".yaml",
	}
	seenSources := map[Layer]bool{}
	rules := make([]Rule, 0)
	descs := make([]SourceDescriptor, 0, len(sources))
	ids := map[string]bool{}
	dups := map[string]bool{}
	for _, src := range sources {
		wantPath, known := expected[src.Descriptor.Layer]
		if !known || src.Descriptor.Path != wantPath {
			add(diag(src, "policy_outside_project", 0, 0, "policy source descriptor is not canonical"))
			continue
		}
		if seenSources[src.Descriptor.Layer] {
			add(diag(src, "duplicate_key", 0, 0, "logical policy layer is duplicated"))
			continue
		}
		seenSources[src.Descriptor.Layer] = true
		descs = append(descs, src.Descriptor)
		if !src.Descriptor.Present {
			if src.Descriptor.Layer != LayerProjectLocal {
				add(diag(src, "policy_missing", 0, 0, "required policy source is absent"))
			}
			if len(src.Bytes) != 0 {
				add(diag(src, "invalid_type", 0, 0, "absent source has content"))
			}
			continue
		}
		if len(src.Bytes) > MaxSourceBytes {
			add(diag(src, "policy_too_large", 0, 0, "policy source exceeds size limit"))
			continue
		}
		if !utf8.Valid(src.Bytes) {
			add(diag(src, "policy_invalid_utf8", 0, 0, "policy source is not valid UTF-8"))
			continue
		}
		rr, findings := parseDocument(src)
		for _, d := range findings {
			add(d)
		}
		for _, r := range rr {
			if ids[r.ID] {
				add(Diagnostic{Code: "duplicate_rule_id", Layer: r.Source.Layer, Path: r.Source.Path, RuleID: r.ID, Message: "rule ID is not unique"})
			} else {
				ids[r.ID] = true
			}
			key := string(r.Source.Layer) + "\x00" + r.Source.Path + "\x00" + string(r.Effect) + "\x00" + string(r.Kind) + "\x00" + r.CommandExact
			if dups[key] {
				add(Diagnostic{Code: "duplicate_rule", Layer: r.Source.Layer, Path: r.Source.Path, RuleID: r.ID, Message: "duplicate rule matcher in source"})
			} else {
				dups[key] = true
			}
			rules = append(rules, r)
		}
	}
	for _, layer := range []Layer{LayerProject, LayerProjectLocal, LayerPersona} {
		if !seenSources[layer] {
			add(Diagnostic{Code: "policy_missing", Layer: layer, Path: expected[layer], Message: "policy source descriptor is missing"})
		}
	}
	if len(rules) > MaxRules {
		add(Diagnostic{Code: "too_many_rules", Message: "effective policy exceeds rule limit"})
	}
	if len(ds) > 0 {
		sortDiagnostics(ds)
		return nil, ds
	}
	cloneRules := append([]Rule(nil), rules...)
	cloneSources := append([]SourceDescriptor(nil), descs...)
	s := &Snapshot{SchemaVersion: SchemaVersion, Persona: persona, ProjectRootID: projectRootID, Sources: cloneSources, Rules: cloneRules}
	s.ID = snapshotID(*s)
	return s, nil
}

func diag(s Source, code string, line, col int, msg string) Diagnostic {
	return Diagnostic{Code: code, Layer: s.Descriptor.Layer, Path: s.Descriptor.Path, Line: line, Column: col, Message: msg}
}

func parseDocument(src Source) ([]Rule, []Diagnostic) {
	dec := yaml.NewDecoder(bytes.NewReader(src.Bytes))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		return nil, []Diagnostic{diag(src, "policy_invalid_yaml", 0, 0, "policy document is invalid YAML")}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, []Diagnostic{diag(src, "policy_multiple_documents", 0, 0, "policy source must contain one document")}
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, []Diagnostic{diag(src, "invalid_type", root.Line, root.Column, "policy document must be a mapping")}
	}
	if badNode(root.Content[0], 0) {
		return nil, []Diagnostic{diag(src, "policy_invalid_yaml", root.Line, root.Column, "aliases, anchors, tags, or excessive nesting are not allowed")}
	}
	n := root.Content[0]
	fields, ds := mapping(src, n, []string{"version", "allow", "ask", "deny"})
	if len(ds) > 0 {
		return nil, ds
	}
	v := fields["version"]
	if v.Kind != yaml.ScalarNode || v.Tag != "!!int" || v.Value != "1" {
		code := "invalid_type"
		if v.Kind == yaml.ScalarNode && v.Tag == "!!int" {
			code = "unknown_version"
		}
		return nil, []Diagnostic{diag(src, code, v.Line, v.Column, "version must be integer 1")}
	}
	var out []Rule
	for _, effect := range []Effect{Allow, Ask, Deny} {
		a := fields[string(effect)]
		if a.Kind != yaml.SequenceNode {
			ds = append(ds, diag(src, "invalid_type", a.Line, a.Column, "effect value must be an array"))
			continue
		}
		if len(a.Content) > MaxRulesPerEffect {
			ds = append(ds, diag(src, "too_many_rules", a.Line, a.Column, "effect array exceeds rule limit"))
			continue
		}
		for _, rn := range a.Content {
			r, d := parseRule(src, effect, rn)
			if d.Code != "" {
				ds = append(ds, d)
			} else {
				out = append(out, r)
			}
		}
	}
	return out, ds
}

func mapping(src Source, n *yaml.Node, want []string) (map[string]*yaml.Node, []Diagnostic) {
	out := map[string]*yaml.Node{}
	allowed := map[string]bool{}
	for _, k := range want {
		allowed[k] = true
	}
	var ds []Diagnostic
	if n.Kind != yaml.MappingNode {
		return out, []Diagnostic{diag(src, "invalid_type", n.Line, n.Column, "value must be a mapping")}
	}
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			ds = append(ds, diag(src, "invalid_type", k.Line, k.Column, "mapping key must be a string"))
			continue
		}
		if _, ok := out[k.Value]; ok {
			ds = append(ds, diag(src, "duplicate_key", k.Line, k.Column, "duplicate mapping key"))
			continue
		}
		out[k.Value] = v
		if !allowed[k.Value] {
			ds = append(ds, diag(src, "unknown_key", k.Line, k.Column, "unknown mapping key"))
		}
	}
	for _, k := range want {
		if _, ok := out[k]; !ok {
			ds = append(ds, diag(src, "missing_key", n.Line, n.Column, "required mapping key is missing"))
		}
	}
	return out, ds
}

func parseRule(src Source, e Effect, n *yaml.Node) (Rule, Diagnostic) {
	m, ds := mapping(src, n, []string{"id", "kind", "match", "reason"})
	if len(ds) > 0 {
		return Rule{}, ds[0]
	}
	str := func(k string) (string, Diagnostic) {
		v := m[k]
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return "", diag(src, "invalid_type", v.Line, v.Column, "field must be a string")
		}
		return v.Value, Diagnostic{}
	}
	id, d := str("id")
	if d.Code != "" {
		return Rule{}, d
	}
	if len(id) < 1 || len(id) > 80 || !ruleIDRE.MatchString(id) {
		return Rule{}, diag(src, "invalid_rule_id", m["id"].Line, m["id"].Column, "rule ID is invalid")
	}
	kind, d := str("kind")
	if d.Code != "" {
		return Rule{}, d
	}
	if kind != string(ProcessExec) {
		return Rule{}, Diagnostic{Code: "unknown_kind", Layer: src.Descriptor.Layer, Path: src.Descriptor.Path, Line: m["kind"].Line, Column: m["kind"].Column, RuleID: id, Message: "rule kind is unsupported"}
	}
	mm, md := mapping(src, m["match"], []string{"command_exact"})
	if len(md) > 0 {
		return Rule{}, withID(md[0], id)
	}
	c := mm["command_exact"]
	if c.Kind != yaml.ScalarNode || c.Tag != "!!str" || !validCommand(c.Value) {
		return Rule{}, Diagnostic{Code: "invalid_match", Layer: src.Descriptor.Layer, Path: src.Descriptor.Path, Line: c.Line, Column: c.Column, RuleID: id, Message: "exact command is invalid"}
	}
	reason, d := str("reason")
	if d.Code != "" {
		return Rule{}, withID(d, id)
	}
	if !validReason(reason) {
		return Rule{}, Diagnostic{Code: "invalid_reason", Layer: src.Descriptor.Layer, Path: src.Descriptor.Path, Line: m["reason"].Line, Column: m["reason"].Column, RuleID: id, Message: "rule reason is invalid"}
	}
	return Rule{ID: id, Kind: ProcessExec, CommandExact: c.Value, Reason: reason, Effect: e, Source: src.Descriptor}, Diagnostic{}
}
func withID(d Diagnostic, id string) Diagnostic { d.RuleID = id; return d }
func validCommand(s string) bool {
	return utf8.ValidString(s) && len(s) >= 1 && len(s) <= 8192 && !badControls(s, false)
}
func validReason(s string) bool {
	return utf8.ValidString(s) && len(s) >= 1 && len(s) <= 240 && !badControls(s, true)
}
func badControls(s string, all bool) bool {
	for _, r := range s {
		if r == 0 || r == 0x7f || r < 0x20 {
			return true
		}
	}
	return false
}
func badNode(n *yaml.Node, depth int) bool {
	if depth > 64 || n.Kind == yaml.AliasNode || n.Anchor != "" || n.Style&yaml.TaggedStyle != 0 {
		return true
	}
	for _, c := range n.Content {
		if badNode(c, depth+1) {
			return true
		}
	}
	return false
}

// Evaluate returns a deterministic policy intent. Requests must already be normalized.
func Evaluate(s *Snapshot, r Request) Explanation {
	e := Explanation{SchemaVersion: SchemaVersion, RequestKind: r.Kind, PolicyEffect: Ask, ReasonCode: "unmatched"}
	if s == nil {
		return e
	}
	e.PolicySnapshotID = s.ID
	if r.Kind != ProcessExec || !validCommand(r.CommandExact) {
		e.ReasonCode = "unsupported_request"
		return e
	}
	var matches []MatchedRule
	winning := Allow
	for _, rule := range s.Rules {
		if rule.Kind == r.Kind && rule.CommandExact == r.CommandExact {
			matches = append(matches, MatchedRule{rule.Source.Layer, rule.Source.Path, rule.ID, rule.Effect})
			if rule.Effect == Deny {
				winning = Deny
			} else if rule.Effect == Ask && winning != Deny {
				winning = Ask
			}
		}
	}
	if len(matches) == 0 {
		return e
	}
	e.PolicyEffect = winning
	e.ReasonCode = "matched_" + string(winning)
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if effectRank(a.Effect) != effectRank(b.Effect) {
			return effectRank(a.Effect) < effectRank(b.Effect)
		}
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.ID < b.ID
	})
	for _, m := range matches {
		if m.Effect == winning {
			if len(e.MatchedRules) < MaxMatchedRules {
				e.MatchedRules = append(e.MatchedRules, m)
			} else {
				e.OmittedMatches++
			}
		}
	}
	return e
}
func effectRank(e Effect) int {
	if e == Deny {
		return 0
	}
	if e == Ask {
		return 1
	}
	return 2
}

func snapshotID(s Snapshot) string {
	h := sha256.New()
	h.Write([]byte("shipmates-policy-snapshot-v1\x00"))
	put := func(x string) { _ = binary.Write(h, binary.BigEndian, uint32(len(x))); h.Write([]byte(x)) }
	put(s.Persona)
	put(s.ProjectRootID)
	src := append([]SourceDescriptor(nil), s.Sources...)
	sort.Slice(src, func(i, j int) bool {
		if src[i].Layer != src[j].Layer {
			return src[i].Layer < src[j].Layer
		}
		return src[i].Path < src[j].Path
	})
	for _, x := range src {
		put(string(x.Layer))
		put(x.Path)
		if x.Present {
			put("1")
		} else {
			put("0")
		}
	}
	rr := append([]Rule(nil), s.Rules...)
	sort.Slice(rr, func(i, j int) bool {
		a, b := rr[i], rr[j]
		ka := fmt.Sprint(a.Source.Layer, "\x00", a.Effect, "\x00", a.ID, "\x00", a.Kind, "\x00", a.CommandExact)
		kb := fmt.Sprint(b.Source.Layer, "\x00", b.Effect, "\x00", b.ID, "\x00", b.Kind, "\x00", b.CommandExact)
		return ka < kb
	})
	for _, r := range rr {
		put(string(r.Source.Layer))
		put(r.Source.Path)
		put(string(r.Effect))
		put(r.ID)
		put(string(r.Kind))
		put(r.CommandExact)
		put(r.Reason)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func sortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Code < b.Code
	})
}
