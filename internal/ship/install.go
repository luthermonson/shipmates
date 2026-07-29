package ship

// InstallOptions tunes how `ship install` registers the supervisor.
//
// The zero value is the historical behaviour: start the supervisor when the
// operator logs in, run it with their interactive token. That is the right
// default for a laptop, and wrong for a box that is expected to come back on
// its own after a power cut — see Unattended.
type InstallOptions struct {
	// Unattended registers the supervisor to start at boot, with no logon
	// session present. Windows only.
	//
	// The distinction matters more than it looks. A logon-triggered task
	// needs somebody to log in; a machine that boots to the lock screen
	// after an outage never starts the ship. Unattended swaps the trigger
	// for a boot trigger and the principal for a non-interactive logon
	// type, so the supervisor is running before anyone touches the console.
	//
	// darwin rejects this rather than ignoring it: a LaunchAgent is bound
	// to a user session by definition, so accepting the flag would promise
	// something the plist cannot deliver.
	Unattended bool

	// StorePassword selects the Password logon type over S4U for an
	// unattended install.
	//
	// S4U stores no secret, but the task gets a token with no network
	// credentials and no access to the user's DPAPI store. Neither matters
	// for shipmates today — the runtimes authenticate over outbound HTTPS
	// and read a plain file — so S4U is the default. StorePassword is the
	// fallback for when S4U turns out not to be enough, and costs an
	// LSA-stored password.
	//
	// shipmates never handles the password itself: schtasks is given a bare
	// /RP and prompts on the console, so it reaches Windows without passing
	// through our memory or a command line.
	StorePassword bool
}
