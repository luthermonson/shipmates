// Package backend describes the execution capabilities of supported agent
// harnesses. Callers ask for capabilities instead of scattering string checks.
package backend

import (
	"os/exec"
	"strings"
)

type Kind string

const (
	Claude  Kind = "claude"
	Codex   Kind = "codex"
	Command Kind = "command"
)

type Capability uint8

const (
	Headless Capability = 1 << iota
	Interactive
	LiveTell
	PTY
)

// Descriptor is the resolved backend and the surfaces it supports.
type Descriptor struct {
	Kind         Kind
	Capabilities Capability
}

func (d Descriptor) Supports(capability Capability) bool {
	return d.Capabilities&capability == capability
}

// Resolve applies the auto-selection rule and returns the backend's supported
// execution surfaces. Unknown explicit values intentionally have no
// capabilities so callers fail closed with a useful unsupported error.
func Resolve(configured string) Descriptor {
	kind := Kind(strings.ToLower(strings.TrimSpace(configured)))
	if kind == "" || kind == "auto" {
		if _, err := exec.LookPath(string(Claude)); err == nil {
			kind = Claude
		} else if _, err := exec.LookPath(string(Codex)); err == nil {
			kind = Codex
		} else {
			kind = Claude
		}
	}

	switch kind {
	case Claude:
		return Descriptor{Kind: kind, Capabilities: Headless | Interactive | LiveTell | PTY}
	case Codex:
		return Descriptor{Kind: kind, Capabilities: Headless}
	case Command:
		return Descriptor{Kind: kind, Capabilities: PTY}
	default:
		return Descriptor{Kind: kind}
	}
}
