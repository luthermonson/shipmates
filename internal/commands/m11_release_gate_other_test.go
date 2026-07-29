//go:build !unix

package commands

// Windows and macOS get the per-host supervisor but not the Fleet Commander
// subsystem (see public_other.go). `ship` is therefore present with its
// supervisor subcommands and without `observe`, which is the unix-only
// tunnel/steer surface — the unix build adds that in
// m11_release_gate_unix_test.go along with the fleet and server branches.
func init() {
	m11PublicCommandTree[""] = append(m11PublicCommandTree[""], "ship")
	m11PublicCommandTree["ship"] = []string{"serve", "add", "status", "install", "uninstall"}
}
