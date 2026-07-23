//go:build shipmates_qualifier_payload

package installer

import "embed"

// Qualifier payloads must not recursively embed the installer that publishes
// them. The build tag is used only for the standalone unprivileged runner.
var targetPayloads embed.FS

const payloadArch = "unsupported"
