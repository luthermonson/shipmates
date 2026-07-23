//go:build (!linux || (!amd64 && !arm64)) && !shipmates_qualifier_payload

package installer

import "embed"

var targetPayloads embed.FS

const payloadArch = "unsupported"
