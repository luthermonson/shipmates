package project

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml"
)

// CodexAgentDocument is a validated, editable Codex agent TOML document.
// go-toml's Tree retains the document structure and comments when it is
// rendered after an update.
type CodexAgentDocument struct {
	Raw          []byte
	Instructions string
	tree         *toml.Tree
}

// ParseCodexAgent validates the complete TOML document. The managed key must
// occur exactly once, at the document root, and its value must be a non-empty
// string. LoadBytes also rejects duplicate/redefined TOML keys and tables.
func ParseCodexAgent(raw []byte) (*CodexAgentDocument, error) {
	if len(raw) > MaxCodexAgentBytes {
		return nil, errors.New("Codex artifact exceeds size limit")
	}
	tree, err := toml.LoadBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("malformed Codex TOML: %w", err)
	}
	if nestedManagedKey(tree, true) {
		return nil, errors.New("developer_instructions must be a top-level key")
	}
	if !tree.HasPath([]string{"developer_instructions"}) {
		return nil, errors.New("missing developer_instructions")
	}
	instructions, ok := tree.GetPath([]string{"developer_instructions"}).(string)
	if !ok {
		return nil, errors.New("developer_instructions must be a string")
	}
	if strings.TrimSpace(instructions) == "" {
		return nil, errors.New("developer_instructions must not be empty")
	}
	return &CodexAgentDocument{
		Raw:          append([]byte(nil), raw...),
		Instructions: instructions,
		tree:         tree,
	}, nil
}

// nestedManagedKey walks parsed tables rather than searching source text, so
// quoted and dotted keys have their TOML semantics applied before policy.
func nestedManagedKey(tree *toml.Tree, root bool) bool {
	for _, key := range tree.Keys() {
		value := tree.GetPath([]string{key})
		if key == "developer_instructions" && !root {
			return true
		}
		switch value := value.(type) {
		case *toml.Tree:
			if nestedManagedKey(value, false) {
				return true
			}
		case []*toml.Tree:
			for _, item := range value {
				if nestedManagedKey(item, false) {
					return true
				}
			}
		}
	}
	return false
}

// ReplaceInstructions updates only the managed root value in the parsed
// document. The document API preserves unrelated values and comments to the
// extent supported by go-toml; formatting may be normalized on serialization.
func (d *CodexAgentDocument) ReplaceInstructions(value string) ([]byte, error) {
	if d == nil || d.tree == nil {
		return nil, errors.New("invalid Codex agent document")
	}
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("developer_instructions must not be empty")
	}
	out, err := replaceRootInstructions(d.Raw, value)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxCodexAgentBytes {
		return nil, errors.New("generated Codex artifact exceeds size limit")
	}
	if _, err := ParseCodexAgent(out); err != nil {
		return nil, err
	}
	return out, nil
}

// replaceRootInstructions edits the one field whose uniqueness and type were
// already established by ParseCodexAgent. Avoiding a Tree marshal is
// intentional: go-toml v1 normalizes the whole document and discards comments.
func replaceRootInstructions(raw []byte, value string) ([]byte, error) {
	lines := bytes.SplitAfter(raw, []byte("\n"))
	offset := 0
	for _, line := range lines {
		trimmed := bytes.TrimLeft(line, " \t")
		for _, key := range [][]byte{[]byte("developer_instructions"), []byte(`"developer_instructions"`), []byte("'developer_instructions'")} {
			if !bytes.HasPrefix(trimmed, key) {
				continue
			}
			rest := trimmed[len(key):]
			i := 0
			for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
				i++
			}
			if i >= len(rest) || rest[i] != '=' {
				continue
			}
			i++
			for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
				i++
			}
			start := offset + len(line) - len(trimmed) + len(key) + i
			end, err := tomlStringEnd(raw, start)
			if err != nil {
				return nil, err
			}
			out := make([]byte, 0, len(raw)+len(value))
			out = append(out, raw[:start]...)
			out = strconv.AppendQuote(out, value)
			out = append(out, raw[end:]...)
			return out, nil
		}
		offset += len(line)
	}
	return nil, errors.New("developer_instructions source location not found")
}

func tomlStringEnd(raw []byte, start int) (int, error) {
	if start >= len(raw) || (raw[start] != '\'' && raw[start] != '"') {
		return 0, errors.New("developer_instructions must be a string")
	}
	quote := raw[start]
	triple := start+2 < len(raw) && raw[start+1] == quote && raw[start+2] == quote
	i := start + 1
	if triple {
		i = start + 3
	}
	for i < len(raw) {
		if triple && i+2 < len(raw) && raw[i] == quote && raw[i+1] == quote && raw[i+2] == quote {
			return i + 3, nil
		}
		if !triple && raw[i] == quote {
			return i + 1, nil
		}
		if quote == '"' && raw[i] == '\\' {
			i += 2
			continue
		}
		i++
	}
	return 0, errors.New("unterminated developer_instructions string")
}
