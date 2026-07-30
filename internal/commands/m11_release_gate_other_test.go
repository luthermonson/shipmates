//go:build !unix

package commands

// The non-unix surface is the base map plus the per-project coordination server
// and the per-host supervisor. Both are cross-platform; only Fleet Commander is
// not, so `fleet` is absent and `ship` appears without `observe`, which is the
// unix-only tunnel/steer subcommand. m11_release_gate_unix_test.go adds `fleet`
// and `ship observe` on unix builds.
//
// In practice this file describes Windows alone. darwin satisfies Go's `unix`
// build tag, so macOS takes the unix branch and already has the full surface —
// an earlier version of this comment claimed both platforms were reduced, which
// was never true.
func init() {
	m11PublicCommandTree[""] = append(m11PublicCommandTree[""], "server", "ship")
	m11PublicCommandTree["server"] = []string{"serve", "stop"}
	m11PublicCommandTree["ship"] = []string{"serve", "add", "status", "install", "uninstall"}
}
