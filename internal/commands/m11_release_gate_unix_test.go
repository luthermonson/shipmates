//go:build unix

package commands

// The Fleet Commander subsystem (fleet/server/ship) is only registered on
// unix (see public_unix.go). Extend m11PublicCommandTree with those
// branches so TestM11ExactPublicCommandAndSubcommandTree asserts them on
// unix builds while the base map (defined in m11_release_gate_test.go)
// remains the authoritative Windows/macOS surface.
func init() {
	m11PublicCommandTree[""] = append(m11PublicCommandTree[""], "fleet", "server", "ship")
	m11PublicCommandTree["fleet"] = []string{"init", "enrollment", "credential", "serve-observer", "steer", "steer-targets", "interrupt", "interrupt-targets", "ships", "status", "events", "follow"}
	m11PublicCommandTree["fleet/enrollment"] = []string{"create", "consume"}
	m11PublicCommandTree["fleet/credential"] = []string{"issue", "inspect", "rotate", "commit", "revoke"}
	m11PublicCommandTree["server"] = []string{"serve", "stop"}
	m11PublicCommandTree["ship"] = []string{"observe", "serve", "add", "status", "install", "uninstall"}
}
